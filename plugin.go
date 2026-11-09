package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"budgetguard/guard"
)

//go:embed status.html
var statusHTML []byte

var (
	engine = guard.NewEngine(nil)

	lifeMu    sync.Mutex
	stateFile string
	loaded    bool
	stopCh    chan struct{}
	pollEvery = make(chan time.Duration, 1)
	reqLog    *guard.RequestLog // nil when disabled
)

// ---- wire types (subset of CPA's sdk/pluginapi; JSON uses Go field names unless tagged) ----

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type configField struct {
	Name        string
	Type        string
	Description string
}

type metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	ConfigFields     []configField
}

type capabilities struct {
	RequestInterceptor               bool `json:"request_interceptor"`
	RequestInterceptorOmitBodyBefore bool `json:"request_interceptor_omit_body_before,omitempty"`
	RequestInterceptorOmitBodyAfter  bool `json:"request_interceptor_omit_body_after,omitempty"`
	UsagePlugin                      bool `json:"usage_plugin"`
	ManagementAPI                    bool `json:"management_api"`
}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

type interceptRequest struct {
	RequestID      string
	SourceFormat   string
	Model          string
	RequestedModel string
	CallerScope    string
	Metadata       map[string]any
}

type interceptResponse struct {
	Terminate       bool        `json:",omitempty"`
	StatusCode      int         `json:",omitempty"`
	ResponseHeaders http.Header `json:",omitempty"`
	ResponseBody    []byte      `json:",omitempty"`
}

type usageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	// CacheCreation1hTokens is the 1-hour TTL part of CacheCreationTokens; hosts that predate
	// the TTL split omit it.
	CacheCreation1hTokens *int64 `json:",omitempty"`
}

type usageRecord struct {
	Provider        string
	Model           string
	APIKey          string
	AuthID          string
	AuthIndex       string
	Failed          bool
	SessionID       string
	ParentSessionID string
	Detail          usageDetail
	ResponseHeaders http.Header
}

type managementRoute struct {
	Method      string
	Path        string
	Menu        string
	Description string
}

type managementRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
	Body    []byte
}

type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// ---- dispatch ----

func handleMethod(method string, req []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configure(req); err != nil {
			return nil, err
		}
		return okEnvelope(registrationInfo())
	case "plugin.quiesce":
		return okEnvelope(struct{}{})
	case "plugin.shutdown":
		shutdown()
		return okEnvelope(struct{}{})
	case "request.intercept_before":
		return interceptBefore(req)
	case "request.intercept_after":
		return okEnvelope(interceptResponse{})
	case "usage.handle":
		handleUsage(req)
		return okEnvelope(struct{}{})
	case "management.register":
		return okEnvelope(map[string]any{
			"routes": []managementRoute{
				{Method: "GET", Path: "/budget-guard/status",
					Description: "Per-key budget usage and per-account limit windows (JSON)."},
				{Method: "GET", Path: "/budget-guard/stats",
					Description: "Request log summary (?period=1h|5h|24h|7d|30d|90d): cost and cache use per key, account and model, and how well the capacity estimates explain the reported usage."},
				{Method: "GET", Path: "/budget-guard/costs",
					Description: "Spend breakdown for charting (?period=24h|7d|30d|90d, ?step=300|900|3600): tokens and cost per billing type by time bucket, key, model, account and agent, the previous period's totals, and the costliest conversations."},
				{Method: "GET", Path: "/budget-guard/timeline",
					Description: "One provider window over time (query: provider, window, hours): pool remaining per step, utilization per account, spend per key."},
				{Method: "GET", Path: "/budget-guard/hub/auth-files",
					Description: "Pool accounts for usage-limit clients such as T3 Code (client key in " + hubKeyHeader + ")."},
				{Method: "POST", Path: "/budget-guard/hub/api-call",
					Description: "Provider usage endpoints for usage-limit clients (client key in " + hubKeyHeader + ")."},
			},
			"resources": []managementRoute{{Path: "/status", Menu: "Budgets",
				Description: "Per-key budget usage against the limits of the subscription pool."}},
		})
	case "management.handle":
		return handleManagement(req)
	}
	return errorEnvelope("unknown_method", "unknown method: "+method), nil
}

func registrationInfo() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: metadata{
			Name: pluginID, Version: pluginVersion, Author: "rodrigohpalmeirim",
			GitHubRepository: "https://github.com/rodrigohpalmeirim/cpa-budget-guard",
			ConfigFields: []configField{
				{Name: "state_file", Type: "string", Description: "Where calibration and accounting state is persisted."},
				{Name: "poll_interval", Type: "string", Description: "How often the usage endpoint of each account is polled (e.g. 3m)."},
				{Name: "request_log_days", Type: "integer", Description: "Days of per-request log to keep next to the state file (default 90, -1 disables)."},
				{Name: "keys", Type: "array", Description: "Budgeted client keys. Edit them on the Pool page or the Budgets page rather than here."},
			},
		},
		Capabilities: capabilities{RequestInterceptor: true, RequestInterceptorOmitBodyBefore: true,
			RequestInterceptorOmitBodyAfter: true, UsagePlugin: true, ManagementAPI: true},
	}
}

func init() { guard.Version = pluginVersion }

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg, err := guard.ParseConfig(req.ConfigYAML)
	if err != nil {
		return err
	}
	engine.Configure(cfg)

	lifeMu.Lock()
	defer lifeMu.Unlock()
	if !loaded || stateFile != cfg.StateFile {
		if err := engine.Load(cfg.StateFile); err != nil {
			logf("warn", "budget-guard: could not load state: %v", err)
		}
		stateFile, loaded = cfg.StateFile, true
	}
	switch {
	case cfg.RequestLogDays < 0:
		if reqLog != nil {
			reqLog.Close()
			reqLog = nil
		}
	case reqLog == nil || reqLog.Dir() != cfg.RequestLogDir():
		if reqLog != nil {
			reqLog.Close()
		}
		reqLog = guard.NewRequestLog(cfg.RequestLogDir(), cfg.RequestLogDays)
	}
	if stopCh == nil {
		stopCh = make(chan struct{})
		go pollLoop(stopCh, cfg.PollInterval)
		go saveLoop(stopCh)
	} else {
		select {
		case pollEvery <- cfg.PollInterval:
		default:
		}
	}
	logf("info", "budget-guard %s configured: %d budgeted keys", pluginVersion, len(cfg.Keys))
	return nil
}

func shutdown() {
	lifeMu.Lock()
	defer lifeMu.Unlock()
	if stopCh != nil {
		close(stopCh)
		stopCh = nil
	}
	if stateFile != "" {
		_ = engine.Save(stateFile)
	}
	if reqLog != nil {
		reqLog.Close()
	}
}

// ---- enforcement ----

func interceptBefore(raw []byte) ([]byte, error) {
	var req interceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	scope := req.CallerScope
	if scope == "" {
		if s, ok := req.Metadata["caller_scope"].(string); ok {
			scope = s
		}
	}
	model := req.RequestedModel
	if model == "" {
		model = req.Model
	}
	d := engine.Check(scope, model)
	if d.Allow {
		return okEnvelope(interceptResponse{})
	}
	now := time.Now()
	logf("info", "budget-guard: rejected %s request: %s", model, d.Message(now))
	status, h, body := rejection(req.SourceFormat, d, now)
	return okEnvelope(interceptResponse{Terminate: true, StatusCode: status, ResponseHeaders: h, ResponseBody: body})
}

// rejection renders an over-budget response in the client's own protocol, carrying the reset time
// in the form each client actually reads, so that agents (e.g. T3 Code) can resume on their own.
func rejection(format string, d guard.Decision, now time.Time) (int, http.Header, []byte) {
	msg := d.Message(now)
	retry := int64(60)
	known := !d.RetryAfter.IsZero() && d.RetryAfter.After(now)
	if known {
		retry = int64(d.RetryAfter.Sub(now).Seconds()) + 1
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Retry-After", fmt.Sprint(retry))
	h.Set("X-Should-Retry", "false") // Anthropic and OpenAI SDKs otherwise retry 429s
	switch strings.ToLower(format) {
	case "claude":
		type apiError struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		inner, _ := json.Marshal(struct {
			Type  string   `json:"type"`
			Error apiError `json:"error"`
		}{"error", apiError{"rate_limit_error", msg}})
		if !known {
			return http.StatusTooManyRequests, h, inner
		}
		// Claude Code ignores rate-limit headers unless it's logged in to a subscription, so a
		// 429 can't tell it when the limit resets. The one form it passes on intact is an
		// upstream-gateway error ("API Error: 503 [gw] [429]: {...} (reset after 1h2m3s)"),
		// which T3 Code parses into a reset time.
		h.Set("Content-Type", "text/plain; charset=utf-8")
		return http.StatusServiceUnavailable, h,
			[]byte(fmt.Sprintf("[budget-guard] [429]: %s (reset after %s)", inner, resetAfter(time.Duration(retry)*time.Second)))
	case "gemini", "gemini-cli":
		raw, _ := json.Marshal(map[string]any{"error": map[string]any{"code": 429, "message": msg, "status": "RESOURCE_EXHAUSTED"}})
		return http.StatusTooManyRequests, h, raw
	}
	e := map[string]any{"message": msg, "type": "rate_limit_error", "code": "budget_exceeded"}
	if d.Provider == "codex" && known {
		// Codex reads its own usage-limit error (with resets_at) and rate-limit headers. A window
		// at 100% in the headers is what makes T3 Code schedule the resume.
		e["type"] = "usage_limit_reached"
		e["resets_at"] = d.RetryAfter.Unix()
		e["resets_in_seconds"] = retry
		slot := "secondary"
		if strings.HasPrefix(d.Window, "5h") {
			slot = "primary"
		}
		h.Set("X-Codex-"+slot+"-Used-Percent", "100")
		h.Set("X-Codex-"+slot+"-Reset-At", fmt.Sprint(d.RetryAfter.Unix()))
		h.Set("X-Codex-"+slot+"-Reset-After-Seconds", fmt.Sprint(retry))
		if m := windowMinutes(d.Window); m > 0 {
			h.Set("X-Codex-"+slot+"-Window-Minutes", fmt.Sprint(m))
		}
	}
	raw, _ := json.Marshal(map[string]any{"error": e})
	return http.StatusTooManyRequests, h, raw
}

// resetAfter formats a duration as "1h2m3s", leaving out zero leading units.
func resetAfter(d time.Duration) string {
	s := int64(d.Seconds())
	out := ""
	if s >= 3600 {
		out += fmt.Sprintf("%dh", s/3600)
	}
	if s >= 60 {
		out += fmt.Sprintf("%dm", s%3600/60)
	}
	return out + fmt.Sprintf("%ds", s%60)
}

// windowMinutes returns the length of a window ID such as "5h", "7d" or "7d:fable".
func windowMinutes(id string) int {
	id, _, _ = strings.Cut(id, ":")
	var n int
	var unit string
	if _, err := fmt.Sscanf(id, "%d%s", &n, &unit); err != nil {
		return 0
	}
	switch unit {
	case "h":
		return n * 60
	case "d":
		return n * 24 * 60
	}
	return 0
}

func handleUsage(raw []byte) {
	var rec usageRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		logf("warn", "budget-guard: bad usage record: %v", err)
		return
	}
	entry, ok := engine.HandleUsage(guard.Usage{
		APIKey: rec.APIKey, AuthID: rec.AuthID, Provider: rec.Provider, Model: rec.Model, Failed: rec.Failed,
		Headers: rec.ResponseHeaders, Session: rec.SessionID, ParentSession: rec.ParentSessionID,
		Tokens: guard.Tokens{Input: rec.Detail.InputTokens, Output: rec.Detail.OutputTokens, Cached: rec.Detail.CachedTokens,
			CacheRead: rec.Detail.CacheReadTokens, CacheCreate: rec.Detail.CacheCreationTokens,
			CacheCreate1h: deref(rec.Detail.CacheCreation1hTokens)},
	})
	lifeMu.Lock()
	l := reqLog
	lifeMu.Unlock()
	if ok && l != nil {
		if err := l.Append(entry); err != nil {
			logf("warn", "budget-guard: request log: %v", err)
		}
	}
}

// statsPeriods are the periods the stats endpoint accepts (?period=).
var statsPeriods = map[string]time.Duration{"1h": time.Hour, "5h": 5 * time.Hour, "24h": 24 * time.Hour,
	"7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "90d": 90 * 24 * time.Hour}

func handleStats(req managementRequest) ([]byte, error) {
	period := req.Query.Get("period")
	if period == "" {
		period = "24h"
	}
	d, ok := statsPeriods[period]
	if !ok {
		return jsonResponse(400, map[string]string{"error": "period must be one of 1h, 5h, 24h, 7d, 30d, 90d"})
	}
	lifeMu.Lock()
	l := reqLog
	lifeMu.Unlock()
	if l == nil {
		return jsonResponse(404, map[string]string{"error": "the request log is disabled (request_log_days: -1)"})
	}
	now := time.Now()
	entries, err := l.Read(now.Add(-d))
	if err != nil {
		return jsonResponse(500, map[string]string{"error": err.Error()})
	}
	st := guard.Summarise(entries, now.Add(-d), now)
	st.LogDir = l.Dir()
	return jsonResponse(200, st)
}

var costsCache struct {
	sync.Mutex
	key  string
	at   time.Time
	body []byte
}

// handleCosts serves the spend breakdown. Reading a long log is slow, so a report is reused for
// a minute.
func handleCosts(req managementRequest) ([]byte, error) {
	period := req.Query.Get("period")
	if period == "" {
		period = "24h"
	}
	d, ok := statsPeriods[period]
	if !ok {
		return jsonResponse(400, map[string]string{"error": "period must be one of 1h, 5h, 24h, 7d, 30d, 90d"})
	}
	step := time.Hour
	if v := req.Query.Get("step"); v != "" {
		n, _ := strconv.Atoi(v)
		switch n {
		case 300, 900, 3600:
			step = time.Duration(n) * time.Second
		default:
			return jsonResponse(400, map[string]string{"error": "step must be 300, 900 or 3600"})
		}
	}
	if d/step > 24*90 {
		return jsonResponse(400, map[string]string{"error": "too many steps: use a longer step"})
	}
	lifeMu.Lock()
	l := reqLog
	lifeMu.Unlock()
	if l == nil {
		return jsonResponse(404, map[string]string{"error": "the request log is disabled (request_log_days: -1)"})
	}
	cacheKey := period + "/" + step.String()
	costsCache.Lock()
	defer costsCache.Unlock()
	if costsCache.key == cacheKey && time.Since(costsCache.at) < time.Minute {
		return costsResponse(costsCache.body)
	}
	labels := map[string]string{}
	for _, a := range engine.Snapshot().Accounts {
		labels[a.ID] = a.Label
	}
	now := time.Now()
	b := guard.NewCostsBuilder(engine.Config().Pricing, now.Add(-d), now, step, labels)
	if err := l.Scan(b.From(), b.Add); err != nil {
		return jsonResponse(500, map[string]string{"error": err.Error()})
	}
	body, err := json.Marshal(b.Finish())
	if err != nil {
		return nil, err
	}
	costsCache.key, costsCache.at, costsCache.body = cacheKey, now, body
	return costsResponse(body)
}

func costsResponse(body []byte) ([]byte, error) {
	return okEnvelope(managementResponse{StatusCode: 200,
		Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body})
}

// handleTimeline charts one provider window: GET ...?provider=claude&window=7d&hours=168.
func handleTimeline(req managementRequest) ([]byte, error) {
	provider, window := strings.ToLower(req.Query.Get("provider")), strings.ToLower(req.Query.Get("window"))
	if provider == "" || window == "" {
		return jsonResponse(400, map[string]string{"error": "provider and window are required"})
	}
	st := engine.Snapshot()
	var length int64
	for _, w := range st.Windows {
		if w.Provider == provider && w.Window == window {
			length = w.LengthSec
		}
	}
	var accounts []guard.TimelineAccount
	for _, a := range st.Accounts {
		if a.Provider != provider || a.Disabled {
			continue
		}
		for _, w := range a.Windows {
			if w.Window != window {
				continue
			}
			ta := guard.TimelineAccount{ID: a.ID, Label: a.Label, Quality: w.Quality, Util: w.Util, ResetsAt: w.ResetsAt}
			if w.Quality == guard.QualityCalibrated || w.Quality == guard.QualityRatio {
				ta.Capacity = w.CapacityUSD
			}
			accounts = append(accounts, ta)
		}
	}
	if len(accounts) == 0 {
		return jsonResponse(404, map[string]string{"error": "no account reports that window"})
	}
	hours, _ := strconv.Atoi(req.Query.Get("hours"))
	if hours <= 0 {
		hours = int(length / 3600)
		if hours <= 0 {
			hours = 24
		}
	}
	hours = min(hours, 24*90)
	span := time.Duration(hours) * time.Hour
	step := span / 240
	for _, s := range []time.Duration{5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour} {
		if step <= s {
			step = s
			break
		}
	}
	now := time.Now()
	start := now.Add(-span).Truncate(step)
	lifeMu.Lock()
	l := reqLog
	lifeMu.Unlock()
	if l == nil {
		return jsonResponse(404, map[string]string{"error": "the request log is disabled (request_log_days: -1)"})
	}
	// Read further back than the chart so the first steps already have a reading.
	entries, err := l.Read(start.Add(-6 * time.Hour))
	if err != nil {
		return jsonResponse(500, map[string]string{"error": err.Error()})
	}
	return jsonResponse(200, guard.BuildTimeline(entries, accounts, provider, window, length, start, now, step))
}

func jsonResponse(status int, v any) ([]byte, error) {
	body, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{StatusCode: status,
		Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body})
}

// ---- management ----

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Resource routes (/v0/resource/plugins/budget-guard/...) are public: only ever serve the static
	// page there. Data is served only on the management route, which requires the management key.
	path := strings.TrimRight(req.Path, "/")
	if strings.HasPrefix(path, "/v0/management/") {
		if _, endpoint, ok := strings.Cut(path, "/budget-guard/hub/"); ok {
			return handleHub(req, endpoint)
		}
	}
	if strings.HasPrefix(path, "/v0/management/") && strings.HasSuffix(path, "/budget-guard/timeline") {
		return handleTimeline(req)
	}
	if strings.HasPrefix(path, "/v0/management/") && strings.HasSuffix(path, "/budget-guard/costs") {
		return handleCosts(req)
	}
	if strings.HasPrefix(path, "/v0/management/") && strings.HasSuffix(path, "/budget-guard/stats") {
		return handleStats(req)
	}
	if strings.HasPrefix(path, "/v0/management/") && strings.HasSuffix(path, "/budget-guard/status") {
		body, err := json.MarshalIndent(engine.Snapshot(), "", " ")
		if err != nil {
			return nil, err
		}
		return okEnvelope(managementResponse{StatusCode: 200,
			Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body})
	}
	return okEnvelope(managementResponse{StatusCode: 200,
		Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: statusHTML})
}

// ---- background work ----

func saveLoop(stop chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			lifeMu.Lock()
			path := stateFile
			lifeMu.Unlock()
			guarded("save", func() {
				if err := engine.Save(path); err != nil {
					logf("warn", "budget-guard: save state: %v", err)
				}
			})
		}
	}
}

func pollLoop(stop chan struct{}, every time.Duration) {
	timer := time.NewTimer(10 * time.Second) // let the host finish starting
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case d := <-pollEvery:
			every = d
		case <-timer.C:
			guarded("poll", pollOnce)
			timer.Reset(every)
		}
	}
}

type authFile struct {
	ID          string `json:"id"`
	AuthIndex   string `json:"auth_index"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Provider    string `json:"provider"`
	Email       string `json:"email"`
	Label       string `json:"label"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
}

func (f authFile) provider() string {
	if f.Provider != "" {
		return strings.ToLower(f.Provider)
	}
	return strings.ToLower(f.Type)
}

func listAuthFiles() ([]authFile, error) {
	var list struct {
		Files []authFile `json:"files"`
	}
	err := callHost("host.auth.list", map[string]any{}, &list)
	return list.Files, err
}

type credential struct {
	AccessToken      string `json:"access_token"`
	AccountID        string `json:"account_id"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
}

func (c credential) accountID() string {
	if c.ChatGPTAccountID != "" {
		return c.ChatGPTAccountID
	}
	return c.AccountID
}

func getCredential(authIndex string) (credential, error) {
	var got struct {
		JSON json.RawMessage `json:"json"`
	}
	var c credential
	if err := callHost("host.auth.get", map[string]any{"auth_index": authIndex}, &got); err != nil {
		return c, err
	}
	if err := json.Unmarshal(got.JSON, &c); err != nil || c.AccessToken == "" {
		return c, fmt.Errorf("no access token in credential")
	}
	return c, nil
}

type hostHTTPRequest struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte `json:",omitempty"`
}

type hostHTTPResponse struct {
	StatusCode int
	Body       []byte
}

func pollOnce() {
	files, err := listAuthFiles()
	if err != nil {
		logf("warn", "budget-guard: auth list: %v", err)
		return
	}
	var infos []guard.AccountInfo
	for _, f := range files {
		provider := f.provider()
		label := f.Email
		if label == "" {
			label = f.Label
		}
		if label == "" {
			label = f.Name
		}
		id := f.ID
		if id == "" {
			id = f.Name
		}
		infos = append(infos, guard.AccountInfo{ID: id, AuthIndex: f.AuthIndex, Provider: provider, Label: label, Disabled: f.Disabled})
	}
	engine.SyncAccounts(infos)

	var models struct {
		Credentials []struct {
			Provider string   `json:"provider"`
			Models   []string `json:"models"`
		} `json:"credentials"`
	}
	if err := callHost("host.auth.models", map[string]any{}, &models); err == nil {
		m := map[string]string{}
		for _, c := range models.Credentials {
			for _, name := range c.Models {
				m[strings.ToLower(name)] = strings.ToLower(c.Provider)
			}
		}
		engine.SetModelProviders(m)
	}

	for _, in := range infos {
		if in.Disabled || (in.Provider != "claude" && in.Provider != "codex") {
			continue
		}
		obs, err := fetchUsage(in)
		if err != nil {
			logf("warn", "budget-guard: usage for %s (%s): %v", in.Label, in.Provider, err)
			continue
		}
		engine.ApplyObservations(in.ID, in.Provider, obs)
		lifeMu.Lock()
		l := reqLog
		lifeMu.Unlock()
		if l != nil && len(obs) > 0 {
			if err := l.Append(guard.PollEntry(in.ID, in.Provider, obs, time.Now())); err != nil {
				logf("warn", "budget-guard: request log: %v", err)
			}
		}
	}
}

func fetchUsage(in guard.AccountInfo) ([]guard.Observation, error) {
	cred, err := getCredential(in.AuthIndex)
	if err != nil {
		return nil, err
	}
	headers := http.Header{"Authorization": {"Bearer " + cred.AccessToken}, "Accept": {"application/json"}}
	url := ""
	switch in.Provider {
	case "claude":
		url = "https://api.anthropic.com/api/oauth/usage"
		headers["Anthropic-Beta"] = []string{"oauth-2025-04-20"}
		headers["User-Agent"] = []string{"claude-cli/2.1.280 (external, cli)"}
	case "codex":
		url = "https://chatgpt.com/backend-api/wham/usage"
		if acct := cred.accountID(); acct != "" {
			headers["Chatgpt-Account-Id"] = []string{acct}
		}
		headers["User-Agent"] = []string{"codex-tui/0.149.1"}
	}
	var resp hostHTTPResponse
	if err := callHost("host.http.do", hostHTTPRequest{Method: "GET", URL: url, Headers: headers}, &resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, resp.Body)
	}
	if in.Provider == "claude" {
		return guard.ParseClaudeUsage(resp.Body)
	}
	return guard.ParseCodexUsage(resp.Body, time.Now())
}

// guarded runs background work, logging instead of crashing the proxy on panic.
func guarded(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			logf("error", "budget-guard: panic in %s: %v", what, r)
		}
	}()
	fn()
}

// logf logs through the host asynchronously, so it never re-enters the host while the host is
// calling into the plugin (e.g. during plugin.register).
func logf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	go func() { _ = callHost("host.log", map[string]any{"level": level, "message": msg}, nil) }()
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
