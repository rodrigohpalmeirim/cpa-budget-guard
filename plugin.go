package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
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
}

type usageRecord struct {
	Provider        string
	Model           string
	APIKey          string
	AuthID          string
	AuthIndex       string
	Failed          bool
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
	Method string
	Path   string
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
			"routes": []managementRoute{{Method: "GET", Path: "/budget-guard/status",
				Description: "Per-key budget usage and per-account limit windows (JSON)."}},
			"resources": []managementRoute{{Path: "/status", Menu: "Budgets",
				Description: "Per-key budget usage against the subscription pool's limits."}},
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
				{Name: "poll_interval", Type: "string", Description: "How often each account's usage endpoint is polled (e.g. 3m)."},
				{Name: "keys", Type: "array", Description: "Budgeted client keys: name, key, budgets.<provider>.own_share / pool_ceiling (window -> percent)."},
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
	msg := d.Message(now)
	logf("info", "budget-guard: rejected %s request: %s", model, msg)
	retry := int64(60)
	if !d.RetryAfter.IsZero() {
		retry = int64(d.RetryAfter.Sub(now).Seconds()) + 1
		if retry < 1 {
			retry = 1
		}
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Retry-After", fmt.Sprint(retry))
	return okEnvelope(interceptResponse{Terminate: true, StatusCode: http.StatusTooManyRequests,
		ResponseHeaders: h, ResponseBody: errorBody(req.SourceFormat, msg)})
}

// errorBody renders a 429 in the client's own protocol so its error handling shows the message.
func errorBody(format, msg string) []byte {
	var v any
	switch strings.ToLower(format) {
	case "claude":
		v = map[string]any{"type": "error", "error": map[string]any{"type": "rate_limit_error", "message": msg}}
	case "gemini", "gemini-cli":
		v = map[string]any{"error": map[string]any{"code": 429, "message": msg, "status": "RESOURCE_EXHAUSTED"}}
	default:
		v = map[string]any{"error": map[string]any{"message": msg, "type": "rate_limit_error", "code": "budget_exceeded"}}
	}
	raw, _ := json.Marshal(v)
	return raw
}

func handleUsage(raw []byte) {
	var rec usageRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		logf("warn", "budget-guard: bad usage record: %v", err)
		return
	}
	engine.HandleUsage(guard.Usage{
		APIKey: rec.APIKey, AuthID: rec.AuthID, Provider: rec.Provider, Model: rec.Model, Failed: rec.Failed,
		Headers: rec.ResponseHeaders,
		Tokens: guard.Tokens{Input: rec.Detail.InputTokens, Output: rec.Detail.OutputTokens, Cached: rec.Detail.CachedTokens,
			CacheRead: rec.Detail.CacheReadTokens, CacheCreate: rec.Detail.CacheCreationTokens},
	})
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

type hostHTTPResponse struct {
	StatusCode int
	Body       []byte
}

func pollOnce() {
	var list struct {
		Files []authFile `json:"files"`
	}
	if err := callHost("host.auth.list", map[string]any{}, &list); err != nil {
		logf("warn", "budget-guard: auth list: %v", err)
		return
	}
	var infos []guard.AccountInfo
	for _, f := range list.Files {
		provider := strings.ToLower(f.Provider)
		if provider == "" {
			provider = strings.ToLower(f.Type)
		}
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
	}
}

func fetchUsage(in guard.AccountInfo) ([]guard.Observation, error) {
	var got struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := callHost("host.auth.get", map[string]any{"auth_index": in.AuthIndex}, &got); err != nil {
		return nil, err
	}
	var cred struct {
		AccessToken      string `json:"access_token"`
		AccountID        string `json:"account_id"`
		ChatGPTAccountID string `json:"chatgpt_account_id"`
	}
	if err := json.Unmarshal(got.JSON, &cred); err != nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("no access token in credential")
	}
	headers := map[string][]string{"Authorization": {"Bearer " + cred.AccessToken}, "Accept": {"application/json"}}
	url := ""
	switch in.Provider {
	case "claude":
		url = "https://api.anthropic.com/api/oauth/usage"
		headers["Anthropic-Beta"] = []string{"oauth-2025-04-20"}
		headers["User-Agent"] = []string{"claude-cli/2.1.280 (external, cli)"}
	case "codex":
		url = "https://chatgpt.com/backend-api/wham/usage"
		acct := cred.ChatGPTAccountID
		if acct == "" {
			acct = cred.AccountID
		}
		if acct != "" {
			headers["Chatgpt-Account-Id"] = []string{acct}
		}
		headers["User-Agent"] = []string{"codex-tui/0.149.1"}
	}
	var resp hostHTTPResponse
	if err := callHost("host.http.do", map[string]any{"method": "GET", "url": url, "headers": headers}, &resp); err != nil {
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
