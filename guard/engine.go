package guard

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Account is one upstream credential (one subscription).
type Account struct {
	ID        string             `json:"id"`
	AuthIndex string             `json:"auth_index,omitempty"`
	Provider  string             `json:"provider"`
	Label     string             `json:"label,omitempty"`
	Disabled  bool               `json:"disabled,omitempty"`
	Windows   map[string]*Window `json:"windows"`
	// Ratios relate each unscoped window's capacity to the account's base (shortest) window,
	// learned from how both utilizations rise together (valid even with usage outside the proxy).
	Ratios    map[string]*Ratio `json:"ratios,omitempty"`
	LastPairs map[string]*Pair  `json:"last_pairs,omitempty"` // per observation source
}

// Ratio accumulates simultaneous utilization rises of the base window and another window.
type Ratio struct {
	Base  string  `json:"base"`
	BaseU float64 `json:"base_u"`
	OthU  float64 `json:"oth_u"`
}

// Pair is a simultaneous reading of all unscoped windows.
type Pair struct {
	Util   map[string]float64   `json:"util"`
	Resets map[string]time.Time `json:"resets"`
}

// stateVersion 2: calibration from before single-source edges (v0.3.0 and older) is discarded,
// since mixing header and poll readings biased it low.
const stateVersion = 2

type persisted struct {
	Version  int                 `json:"version"`
	SavedAt  time.Time           `json:"saved_at"`
	Accounts map[string]*Account `json:"accounts"`
}

// Engine holds all state. All methods are safe for concurrent use.
type Engine struct {
	mu        sync.Mutex
	cfg       Config
	byScope   map[string]*KeyConfig // caller scope -> key
	byKey     map[string]*KeyConfig // raw key -> key
	accounts  map[string]*Account   // by account ID
	modelProv map[string]string     // model -> provider (from host.auth.models)
	dirty     bool
	now       func() time.Time
}

// NewEngine creates an engine with the given clock (nil = time.Now).
func NewEngine(now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{accounts: map[string]*Account{}, modelProv: map[string]string{}, now: now,
		byScope: map[string]*KeyConfig{}, byKey: map[string]*KeyConfig{}}
}

// Configure installs a new config (keeps learned state).
func (e *Engine) Configure(cfg Config) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg = cfg
	e.byScope = map[string]*KeyConfig{}
	e.byKey = map[string]*KeyConfig{}
	for i := range e.cfg.Keys {
		k := &e.cfg.Keys[i]
		e.byScope[CallerScope(k.Key)] = k
		e.byKey[k.Key] = k
	}
}

// Config returns the active config.
func (e *Engine) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// KeyName returns the name of the configured key with this raw value.
func (e *Engine) KeyName(raw string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	k := e.byKey[strings.TrimSpace(raw)]
	if k == nil || raw == "" {
		return "", false
	}
	return k.Name, true
}

func (e *Engine) account(id, provider string) *Account {
	a := e.accounts[id]
	if a == nil {
		a = &Account{ID: id, Provider: provider, Windows: map[string]*Window{}}
		e.accounts[id] = a
	}
	if a.Provider == "" {
		a.Provider = provider
	}
	return a
}

func (a *Account) window(o Observation) *Window {
	w := a.Windows[o.Window]
	if w == nil {
		w = &Window{ID: o.Window, Scope: o.Scope, LengthSec: o.LengthSec}
		a.Windows[o.Window] = w
	}
	return w
}

// AccountInfo is the host's view of a credential.
type AccountInfo struct {
	ID, AuthIndex, Provider, Label string
	Disabled                       bool
}

// SyncAccounts updates account metadata from host.auth.list.
func (e *Engine) SyncAccounts(infos []AccountInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, in := range infos {
		a := e.account(in.ID, in.Provider)
		a.AuthIndex, a.Label, a.Disabled = in.AuthIndex, in.Label, in.Disabled
	}
	e.dirty = true
}

// SetModelProviders installs the model -> provider map.
func (e *Engine) SetModelProviders(m map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.modelProv = m
}

// ProviderForModel resolves which pool serves a model.
func (e *Engine) ProviderForModel(model string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.providerForModelLocked(model)
}

func (e *Engine) providerForModelLocked(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if p, ok := e.modelProv[m]; ok {
		return p
	}
	for _, s := range []string{"claude", "opus", "sonnet", "haiku", "fable", "mythos"} {
		if strings.Contains(m, s) {
			return "claude"
		}
	}
	for _, s := range []string{"gpt", "codex", "o3", "o4"} {
		if strings.Contains(m, s) {
			return "codex"
		}
	}
	return ""
}

// ApplyObservations records readings polled from an account's usage endpoint.
func (e *Engine) ApplyObservations(accountID, provider string, obs []Observation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.applyLocked(e.account(accountID, provider), obs, SourcePoll, e.now())
}

func (e *Engine) applyLocked(a *Account, obs []Observation, source string, now time.Time) {
	if len(obs) == 0 {
		return
	}
	for i := range obs {
		obs[i].Source = source
		if a.window(obs[i]).Observe(obs[i], now, e.cfg.CalibrationStep) {
			// A plan change moves every window of the account, including ones idle right now.
			a.Ratios, a.LastPairs = nil, nil
			for _, w := range a.Windows {
				w.Anchor, w.Segments = nil, nil
			}
		}
	}
	e.updateRatiosLocked(a, obs, source)
	e.dirty = true
}

// baseWindow is the shortest unscoped window an account has.
func (a *Account) baseWindow() string {
	best, bestLen := "", int64(math.MaxInt64)
	for id, w := range a.Windows {
		if w.Scope == "" && w.LengthSec > 0 && w.LengthSec < bestLen {
			best, bestLen = id, w.LengthSec
		}
	}
	return best
}

// updateRatiosLocked compares a reading with the previous one from the same source only.
func (e *Engine) updateRatiosLocked(a *Account, obs []Observation, source string) {
	base := a.baseWindow()
	cur := &Pair{Util: map[string]float64{}, Resets: map[string]time.Time{}}
	for _, o := range obs {
		if o.Scope == "" {
			cur.Util[o.Window] = o.Util
			cur.Resets[o.Window] = a.Windows[o.Window].ResetsAt
		}
	}
	if _, ok := cur.Util[base]; !ok || len(cur.Util) < 2 {
		return
	}
	if prev := a.LastPairs[source]; prev != nil {
		samePeriod := func(id string) bool {
			p, ok := prev.Resets[id]
			return ok && math.Abs(p.Sub(cur.Resets[id]).Seconds()) < resetTolerance.Seconds()
		}
		dBase := cur.Util[base] - prev.Util[base]
		if _, ok := prev.Util[base]; ok && samePeriod(base) && dBase >= 0 {
			for id, u := range cur.Util {
				if id == base {
					continue
				}
				pu, ok := prev.Util[id]
				if !ok || !samePeriod(id) || u < pu {
					continue
				}
				if a.Ratios == nil {
					a.Ratios = map[string]*Ratio{}
				}
				r := a.Ratios[id]
				if r == nil || r.Base != base {
					r = &Ratio{Base: base}
					a.Ratios[id] = r
				}
				r.BaseU += dBase
				r.OthU += u - pu
			}
		}
	}
	if a.LastPairs == nil {
		a.LastPairs = map[string]*Pair{}
	}
	a.LastPairs[source] = cur
}

// Capacity returns the estimated cost that fills 100% of window id on account a.
func (a *Account) Capacity(id string) (float64, string) {
	w := a.Windows[id]
	if w == nil {
		return 0, QualityNone
	}
	direct, q := w.DirectCapacity()
	if q == QualityCalibrated {
		return direct, q
	}
	// Derive from the base window: capacity_w = capacity_base * (base rise / w rise).
	if r := a.Ratios[id]; r != nil && w.Scope == "" && r.BaseU >= ratioMinBaseRise && r.OthU > 0 {
		if bc, bq := a.Windows[r.Base].DirectCapacity(); bq == QualityCalibrated {
			return bc * r.BaseU / r.OthU, QualityRatio
		}
	}
	return direct, q
}

// Usage is one finished request as reported by CPA.
type Usage struct {
	APIKey   string
	AuthID   string
	Provider string
	Model    string
	Tokens   Tokens
	Headers  http.Header
	Failed   bool
}

// HandleUsage books a finished request: cost per window, and the headers' utilization readings.
// It returns the request as booked, for the request log (ok is false when nothing was booked).
func (e *Engine) HandleUsage(u Usage) (entry Entry, ok bool) {
	if u.AuthID == "" {
		return entry, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	provider := strings.ToLower(u.Provider)
	if provider == "" {
		provider = e.providerForModelLocked(u.Model)
	}
	a := e.account(u.AuthID, provider)
	// Rate-limit headers arrive before the body, so they reflect usage *before* this request:
	// apply them first, then book the request's cost.
	obs, limits := ParseHeaders(a.Provider, u.Headers, now)
	e.applyLocked(a, obs, SourceHeader, now)
	keyName := ""
	if k := e.byKey[strings.TrimSpace(u.APIKey)]; k != nil {
		keyName = k.Name
	} else if k := e.byScope[CallerScope(u.APIKey)]; k != nil {
		keyName = k.Name
	}
	cost := Cost(e.cfg.Pricing, a.Provider, u.Model, u.Tokens)
	entry = Entry{At: now, Key: keyName, Provider: a.Provider, Account: a.ID, Model: u.Model, Failed: u.Failed,
		Input: u.Tokens.Input, Output: u.Tokens.Output, Cached: u.Tokens.Cached, CacheRead: u.Tokens.CacheRead,
		CacheWrite: u.Tokens.CacheCreate, Cost: cost, Windows: map[string]EntryNote{}}
	for _, o := range obs {
		u := o.Util
		n := entry.Windows[o.Window]
		n.Util = &u
		if !o.ResetsAt.IsZero() {
			n.Resets = o.ResetsAt.Unix()
		}
		entry.Windows[o.Window] = n
	}
	for id, w := range a.Windows {
		if scopeMatches(w.Scope, u.Model, limits) {
			n := entry.Windows[id]
			n.Booked = cost > 0
			c, q := a.Capacity(id)
			if q == QualityCalibrated || q == QualityRatio {
				n.Cap = c
			}
			n.Quality = q
			entry.Windows[id] = n
		}
	}
	if cost > 0 {
		for _, w := range a.Windows {
			if scopeMatches(w.Scope, u.Model, limits) {
				w.AddCost(keyName, cost, now)
				if strings.HasPrefix(w.Scope, "header:") && u.Model != "" {
					if w.Models == nil {
						w.Models = map[string]bool{}
					}
					w.Models[strings.ToLower(u.Model)] = true
				}
			}
		}
		e.dirty = true
	}
	return entry, true
}

// Decision is the outcome of a budget check.
type Decision struct {
	Allow      bool
	Key        string
	Provider   string
	Window     string
	Kind       string // "own_share" or "pool_ceiling"
	Used       float64
	Limit      float64
	RetryAfter time.Time
}

// Message renders a human-readable rejection.
func (d Decision) Message(now time.Time) string {
	what := "own share of the pool"
	if d.Kind == "pool_ceiling" {
		what = "pool usage ceiling"
	}
	msg := fmt.Sprintf("Budget for key %q exhausted: %s %s window %s is at %.1f%% (limit %.0f%%).",
		d.Key, d.Provider, d.Window, what, d.Used, d.Limit)
	if !d.RetryAfter.IsZero() && d.RetryAfter.After(now) {
		msg += fmt.Sprintf(" Frees up at %s (in %s).", d.RetryAfter.UTC().Format("2006-01-02 15:04 UTC"), humanDuration(d.RetryAfter.Sub(now)))
	}
	return msg
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// WindowUsage is a key's standing in one pool window.
type WindowUsage struct {
	Window       string    `json:"window"`
	OwnShare     float64   `json:"own_share_pct"`
	OwnKnown     bool      `json:"own_share_known"`
	OwnFreesAt   time.Time `json:"own_share_frees_at,omitempty"`
	PoolUtil     float64   `json:"pool_util_pct"`
	PoolFreesAt  time.Time `json:"pool_frees_at,omitempty"`
	OwnCostUSD   float64   `json:"own_cost_usd"`
	PoolCapacity float64   `json:"pool_capacity_usd"`
	Quality      string    `json:"calibration"`
}

// poolWindowLocked computes a key's own share and the pool utilization of one window.
func (e *Engine) poolWindowLocked(provider, window, key string, now time.Time) (WindowUsage, bool) {
	type acc struct {
		w   *Window
		cap float64
		q   string
	}
	var accs []acc
	var knownCap float64
	var nKnown int
	for _, a := range e.accounts {
		if a.Provider != provider || a.Disabled {
			continue
		}
		w := a.Windows[window]
		if w == nil {
			continue
		}
		w.expire(now)
		c, q := a.Capacity(window)
		if !Trusted(q) {
			c = 0 // rough early estimates are shown per account but never used to block
		}
		accs = append(accs, acc{w, c, q})
		if c > 0 {
			knownCap += c
			nKnown++
		}
	}
	if len(accs) == 0 {
		return WindowUsage{}, false
	}
	wu := WindowUsage{Window: window, Quality: QualityCalibrated}
	rank := map[string]int{QualityNone: 0, QualityBootstrap: 1, QualityRatio: 2, QualityCalibrated: 3}
	meanCap := 0.0
	if nKnown > 0 {
		meanCap = knownCap / float64(nKnown)
	}
	var totalCap, weightedUtil, ownCost float64
	for _, x := range accs {
		if rank[x.q] < rank[wu.Quality] {
			wu.Quality = x.q
		}
		c := x.cap
		if c <= 0 {
			c = meanCap // uncalibrated account: assume an average-sized plan
		}
		weight := c
		if nKnown == 0 {
			weight = 1
		}
		totalCap += c
		u := x.w.EffectiveUtil(now)
		weightedUtil += u * weight
		if u > 0 && x.w.Active(now) && (wu.PoolFreesAt.IsZero() || x.w.ResetsAt.Before(wu.PoolFreesAt)) {
			wu.PoolFreesAt = x.w.ResetsAt
		}
		if kc := x.w.KeyCost[key]; kc > 0 && x.w.Active(now) {
			ownCost += kc
			if wu.OwnFreesAt.IsZero() || x.w.ResetsAt.Before(wu.OwnFreesAt) {
				wu.OwnFreesAt = x.w.ResetsAt
			}
		}
	}
	if nKnown == 0 {
		wu.PoolUtil = weightedUtil / float64(len(accs))
	} else {
		wu.PoolUtil = weightedUtil / totalCap
	}
	wu.OwnCostUSD, wu.PoolCapacity = ownCost, totalCap
	if totalCap > 0 {
		wu.OwnShare, wu.OwnKnown = 100*ownCost/totalCap, true
	}
	return wu, true
}

// windowsLocked lists the window IDs present in a provider's pool.
func (e *Engine) windowsLocked(provider string) []string {
	set := map[string]bool{}
	for _, a := range e.accounts {
		if a.Provider == provider && !a.Disabled {
			for id := range a.Windows {
				set[id] = true
			}
		}
	}
	return sortedKeys(set)
}

// appliesLocked reports whether a pool window limits requests for model: model-scoped windows by
// name, header-scoped ones (Codex additional limits) by the models seen hitting them.
func (e *Engine) appliesLocked(provider, window, model string) bool {
	model = strings.ToLower(model)
	for _, a := range e.accounts {
		w := a.Windows[window]
		if a.Provider != provider || w == nil {
			continue
		}
		switch {
		case w.Scope == "":
			return true
		case strings.HasPrefix(w.Scope, "model:"):
			return scopeMatches(w.Scope, model, nil)
		case w.Models[model]:
			return true
		}
	}
	return false
}

// Check decides whether a request from callerScope for model may proceed.
func (e *Engine) Check(callerScope, model string) Decision {
	e.mu.Lock()
	defer e.mu.Unlock()
	k := e.byScope[callerScope]
	if k == nil {
		return Decision{Allow: true}
	}
	provider := e.providerForModelLocked(model)
	b, ok := k.Budgets[provider]
	if !ok {
		return Decision{Allow: true, Key: k.Name, Provider: provider}
	}
	now := e.now()
	for _, id := range e.windowsLocked(provider) {
		if !e.appliesLocked(provider, id, model) {
			continue // e.g. the Fable weekly limit doesn't apply to an Opus request
		}
		ownLimit, hasOwn := limitFor(b.OwnShare, id)
		ceiling, hasCeil := limitFor(b.PoolCeiling, id)
		if !hasOwn && !hasCeil {
			continue
		}
		wu, ok := e.poolWindowLocked(provider, id, k.Name, now)
		if !ok {
			continue
		}
		if hasOwn && wu.OwnKnown && wu.OwnShare >= ownLimit {
			return Decision{Key: k.Name, Provider: provider, Window: id, Kind: "own_share", Used: wu.OwnShare, Limit: ownLimit, RetryAfter: wu.OwnFreesAt}
		}
		if hasCeil && wu.PoolUtil >= ceiling {
			return Decision{Key: k.Name, Provider: provider, Window: id, Kind: "pool_ceiling", Used: wu.PoolUtil, Limit: ceiling, RetryAfter: wu.PoolFreesAt}
		}
	}
	return Decision{Allow: true, Key: k.Name, Provider: provider}
}

// Status is the JSON served by the status endpoint.
type Status struct {
	Now      time.Time       `json:"now"`
	Version  string          `json:"version"`
	Windows  []PoolWindow    `json:"windows"` // every window per provider, for building budget editors
	Pricing  []PriceRule     `json:"pricing_defaults"`
	Keys     []KeyStatus     `json:"keys"`
	Accounts []AccountStatus `json:"accounts"`
}

// PoolWindow describes a window that exists in a provider's pool.
type PoolWindow struct {
	Provider  string `json:"provider"`
	Window    string `json:"window"`
	Scope     string `json:"scope,omitempty"`
	LengthSec int64  `json:"length_sec,omitempty"`
}

// Version is reported in the status (set by the plugin wrapper).
var Version = "dev"

type KeyStatus struct {
	Name      string              `json:"name"`
	Providers []KeyProviderStatus `json:"providers"`
}

type KeyProviderStatus struct {
	Provider string            `json:"provider"`
	Windows  []KeyWindowStatus `json:"windows"`
}

type KeyWindowStatus struct {
	WindowUsage
	OwnLimit    *float64 `json:"own_share_limit,omitempty"`
	PoolCeiling *float64 `json:"pool_ceiling,omitempty"`
	Blocked     bool     `json:"blocked"`
}

type AccountStatus struct {
	ID       string               `json:"id"`
	Label    string               `json:"label,omitempty"`
	Provider string               `json:"provider"`
	Disabled bool                 `json:"disabled,omitempty"`
	Windows  []AccountWindowState `json:"windows"`
}

type AccountWindowState struct {
	Window      string    `json:"window"`
	Scope       string    `json:"scope,omitempty"`
	Util        float64   `json:"util_pct"`
	ResetsAt    time.Time `json:"resets_at,omitempty"`
	ObservedAt  time.Time `json:"observed_at"`
	ProxyCost   float64   `json:"proxy_cost_usd"`
	CapacityUSD float64   `json:"capacity_usd"`
	Quality     string    `json:"calibration"`
	Segments    int       `json:"segments"`
}

// Snapshot builds the status report.
func (e *Engine) Snapshot() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	st := Status{Now: now, Version: Version, Pricing: defaultPricing}
	providers := map[string]bool{}
	for _, a := range e.accounts {
		providers[a.Provider] = true
	}
	for _, provider := range sortedKeys(providers) {
		for _, id := range e.windowsLocked(provider) {
			pw := PoolWindow{Provider: provider, Window: id}
			for _, a := range e.accounts {
				if w := a.Windows[id]; a.Provider == provider && w != nil {
					pw.Scope, pw.LengthSec = w.Scope, w.LengthSec
					break
				}
			}
			st.Windows = append(st.Windows, pw)
		}
	}
	for _, k := range e.cfg.Keys {
		ks := KeyStatus{Name: k.Name}
		for _, provider := range sortedKeys(k.Budgets) {
			b := k.Budgets[provider]
			ps := KeyProviderStatus{Provider: provider}
			for _, id := range e.windowsLocked(provider) {
				wu, ok := e.poolWindowLocked(provider, id, k.Name, now)
				if !ok {
					continue
				}
				ws := KeyWindowStatus{WindowUsage: wu}
				if v, ok := limitFor(b.OwnShare, id); ok {
					ws.OwnLimit = &v
					ws.Blocked = ws.Blocked || (wu.OwnKnown && wu.OwnShare >= v)
				}
				if v, ok := limitFor(b.PoolCeiling, id); ok {
					ws.PoolCeiling = &v
					ws.Blocked = ws.Blocked || wu.PoolUtil >= v
				}
				ps.Windows = append(ps.Windows, ws)
			}
			ks.Providers = append(ks.Providers, ps)
		}
		st.Keys = append(st.Keys, ks)
	}
	for _, id := range sortedKeys(e.accounts) {
		a := e.accounts[id]
		as := AccountStatus{ID: a.ID, Label: a.Label, Provider: a.Provider, Disabled: a.Disabled}
		for _, wid := range sortedKeys(a.Windows) {
			w := a.Windows[wid]
			w.expire(now)
			c, q := a.Capacity(wid)
			as.Windows = append(as.Windows, AccountWindowState{Window: wid, Scope: w.Scope, Util: w.EffectiveUtil(now),
				ResetsAt: w.ResetsAt, ObservedAt: w.ObservedAt, ProxyCost: w.ProxyCost, CapacityUSD: c, Quality: q, Segments: len(w.Segments)})
		}
		st.Accounts = append(st.Accounts, as)
	}
	sort.SliceStable(st.Accounts, func(i, j int) bool { return st.Accounts[i].Provider < st.Accounts[j].Provider })
	return st
}

// Load reads persisted state (missing file is fine).
func (e *Engine) Load(path string) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if p.Accounts != nil {
		for _, a := range p.Accounts {
			if a.Windows == nil {
				a.Windows = map[string]*Window{}
			}
			if p.Version < stateVersion {
				a.Ratios = nil
				for _, w := range a.Windows {
					w.Anchor, w.Segments = nil, nil
				}
			}
		}
		e.accounts = p.Accounts
	}
	return nil
}

// Save writes state atomically when it changed.
func (e *Engine) Save(path string) error {
	e.mu.Lock()
	if !e.dirty {
		e.mu.Unlock()
		return nil
	}
	raw, err := json.MarshalIndent(persisted{Version: stateVersion, SavedAt: e.now(), Accounts: e.accounts}, "", " ")
	e.dirty = false
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
