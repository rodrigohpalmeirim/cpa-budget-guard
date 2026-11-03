package guard

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s = %.3f, want %.3f ± %.3f", name, got, want, tol)
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
poll_interval: 2m
keys:
  - name: laptop
    key: sk-1
    budgets:
      Claude:
        own_share: {5h: 30, "*": 20}
        pool_ceiling: {7d: 90}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval != 2*time.Minute || cfg.CalibrationStep != 3 {
		t.Fatalf("defaults: %+v", cfg)
	}
	b := cfg.Keys[0].Budgets["claude"]
	if v, _ := limitFor(b.OwnShare, "5h"); v != 30 {
		t.Fatal("explicit window")
	}
	if v, _ := limitFor(b.OwnShare, "7d:fable"); v != 20 {
		t.Fatal("wildcard window")
	}
	if _, err := ParseConfig([]byte("keys: [{name: a, key: k, budgets: {claude: {own_share: {5h: 120}}}}]")); err == nil {
		t.Fatal("expected range error")
	}
}

func TestParseClaude(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.37")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1793473089")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.69")
	obs, _ := ParseHeaders("claude", h, t0)
	if len(obs) != 2 || obs[0].Window != "5h" || obs[0].Util != 37 || obs[0].ResetsAt.Unix() != 1793473089 || obs[1].Util != 69 {
		t.Fatalf("headers: %+v", obs)
	}
	body := []byte(`{"five_hour":{"utilization":6.0},"limits":[
	 {"kind":"session","group":"session","percent":6,"resets_at":"2026-10-31T15:30:00.174283+00:00","scope":null},
	 {"kind":"weekly_all","group":"weekly","percent":30,"resets_at":"2026-11-01T07:00:00.174299+00:00","scope":null},
	 {"kind":"weekly_scoped","group":"weekly","percent":0,"resets_at":"2026-11-01T07:00:00+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null}}]}`)
	obs, err := ParseClaudeUsage(body)
	if err != nil || len(obs) != 3 {
		t.Fatalf("usage: %v %+v", err, obs)
	}
	if obs[2].Window != "7d:fable" || obs[2].Scope != "model:fable" || obs[1].Util != 30 || obs[0].ResetsAt.Hour() != 15 {
		t.Fatalf("usage windows: %+v", obs)
	}
}

func TestParseCodex(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "12")
	h.Set("X-Codex-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Primary-Reset-At", "1793473089")
	h.Set("X-Codex-Secondary-Used-Percent", "40")
	h.Set("X-Codex-Secondary-Window-Minutes", "10080")
	h.Set("X-Codex-Bengalfox-Primary-Used-Percent", "5")
	h.Set("X-Codex-Bengalfox-Primary-Window-Minutes", "300")
	obs, limits := ParseHeaders("codex", h, t0)
	got := map[string]Observation{}
	for _, o := range obs {
		got[o.Window] = o
	}
	if got["5h"].Util != 12 || got["7d"].Util != 40 || got["5h:bengalfox"].Scope != "header:bengalfox" || !limits["bengalfox"] {
		t.Fatalf("codex headers: %+v %v", obs, limits)
	}
	body := []byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":12922,"reset_at":1793473089},
	 "secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":351365,"reset_at":1793811532}},"additional_rate_limits":null}`)
	obs, err := ParseCodexUsage(body, t0)
	if err != nil || len(obs) != 2 || obs[0].Window != "5h" || obs[1].Window != "7d" || obs[1].Util != 10 {
		t.Fatalf("codex usage: %v %+v", err, obs)
	}
}

func TestCost(t *testing.T) {
	// Claude: input excludes cache; Opus 5.5 = $4 in / $20 out / $0.20 read / $5 write.
	c := Cost(nil, "claude", "claude-opus-5-5", Tokens{Input: 1e6, Output: 1e6, CacheRead: 1e6, CacheCreate: 1e6})
	near(t, "opus cost", c, 4+20+0.2+5, 1e-9)
	if Cost(nil, "claude", "claude-haiku-5-5", Tokens{Output: 1e6}) >= Cost(nil, "claude", "claude-sonnet-5-5", Tokens{Output: 1e6}) {
		t.Fatal("haiku should weigh less than sonnet")
	}
	// 1-hour writes (when the host reports the split) cost 2x input; the rest stay 5-minute.
	c = Cost(nil, "claude", "claude-opus-5-5", Tokens{CacheCreate: 3e6, CacheCreate1h: 2e6})
	near(t, "opus 1h writes", c, 5+2*8, 1e-9)
	c = Cost([]PriceRule{{Match: "opus", Input: 4, CacheWrite: 5, CacheWrite1h: 9}}, "claude", "claude-opus-5-5", Tokens{CacheCreate: 1e6, CacheCreate1h: 5e6})
	near(t, "configured 1h price, split clamped to the total", c, 9, 1e-9)
	// Codex: cached is a subset of input.
	near(t, "codex cost", Cost(nil, "codex", "gpt-6", Tokens{Input: 2e6, Cached: 1e6, Output: 1e6}), 1.25+0.125+10, 1e-9)
}

// sim drives an engine with one Claude account whose true capacity is capUSD per 100% of 5h
// (and 10x that for 7d), with optional external (non-proxy) usage.
type sim struct {
	e       *Engine
	c       *clock
	id      string
	cap5h   float64
	used    float64 // true total cost in the period (proxy + external)
	reset5h time.Time
	reset7d time.Time
	base7d  float64
}

func newSim(e *Engine, c *clock, id string, cap5h float64) *sim {
	return &sim{e: e, c: c, id: id, cap5h: cap5h, reset5h: c.t.Add(5 * time.Hour), reset7d: c.t.Add(6 * 24 * time.Hour)}
}

func (s *sim) headers() http.Header {
	h := http.Header{}
	q := func(v float64) string { return fmt.Sprintf("%.2f", math.Floor(v*100)/100) } // 1% resolution like Anthropic
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", q(s.used/s.cap5h))
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", fmt.Sprint(s.reset5h.Unix()))
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", q(s.base7d+s.used/(10*s.cap5h)))
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", fmt.Sprint(s.reset7d.Unix()))
	return h
}

// request books one proxy request costing $1 (1M output tokens on a $1/M pricing rule).
// Like Anthropic, the headers reflect usage before the request itself is counted.
func (s *sim) request(key string) {
	h := s.headers()
	s.used += 1
	s.e.HandleUsage(Usage{APIKey: key, AuthID: s.id, Provider: "claude", Model: "claude-test", Tokens: Tokens{Output: 1e6}, Headers: h})
}

func testEngine(t *testing.T, yaml string) (*Engine, *clock) {
	t.Helper()
	cfg, err := ParseConfig([]byte(yaml + "\npricing: [{provider: claude, match: claude-test, input: 1, output: 1}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: t0}
	e := NewEngine(c.now)
	e.Configure(cfg)
	return e, c
}

func TestCalibrationWithExternalUsage(t *testing.T) {
	e, c := testEngine(t, "keys: [{name: laptop, key: sk-laptop}]")
	s := newSim(e, c, "acct-1", 200) // $200 fills the 5h window
	for i := 0; i < 120; i++ {
		s.request("sk-laptop")
		if i%10 == 0 {
			s.used += 3 // external usage the proxy never sees
		}
		c.add(time.Minute)
	}
	a := e.accounts["acct-1"]
	cap5, q5 := a.Capacity("5h")
	if q5 != QualityCalibrated {
		t.Fatalf("5h quality %s", q5)
	}
	near(t, "5h capacity", cap5, 200, 20) // outside usage is ignored: the estimate tracks the cleanest segments
	cap7, q7 := a.Capacity("7d")
	if q7 != QualityRatio && q7 != QualityCalibrated {
		t.Fatalf("7d quality %s", q7)
	}
	near(t, "7d capacity", cap7, 2000, 500)
}

func TestOwnShareBlocksAndFrees(t *testing.T) {
	e, c := testEngine(t, `keys:
  - {name: laptop, key: sk-laptop, budgets: {claude: {own_share: {5h: 30}}}}
  - {name: ci, key: sk-ci, budgets: {claude: {pool_ceiling: {5h: 50}}}}`)
	s := newSim(e, c, "acct-1", 100)
	laptop, ci := CallerScope("sk-laptop"), CallerScope("sk-ci")
	blockedAt := -1
	for i := 0; i < 60; i++ {
		if d := e.Check(laptop, "claude-opus-5-5"); !d.Allow {
			blockedAt = i
			if d.Kind != "own_share" || d.Window != "5h" || !d.RetryAfter.Equal(s.reset5h) {
				t.Fatalf("decision %+v", d)
			}
			break
		}
		s.request("sk-laptop")
		c.add(time.Minute)
	}
	if blockedAt < 25 || blockedAt > 36 {
		t.Fatalf("laptop blocked after %d requests, want ~30", blockedAt)
	}
	// Unknown keys are never limited.
	if !e.Check(CallerScope("sk-admin"), "claude-opus-5-5").Allow {
		t.Fatal("unbudgeted key blocked")
	}
	// Pool at ~30%: ci (ceiling 50%) still allowed; push the pool past 50% with external usage.
	if !e.Check(ci, "claude-opus-5-5").Allow {
		t.Fatal("ci blocked too early")
	}
	s.used += 25
	s.request("")
	if d := e.Check(ci, "claude-opus-5-5"); d.Allow || d.Kind != "pool_ceiling" {
		t.Fatalf("ci should hit the ceiling: %+v", d)
	}
	// After the 5h reset both are free again.
	c.t = s.reset5h.Add(time.Minute)
	if !e.Check(laptop, "claude-opus-5-5").Allow || !e.Check(ci, "claude-opus-5-5").Allow {
		t.Fatal("still blocked after reset")
	}
}

func TestPoolWeightsByCapacity(t *testing.T) {
	e, c := testEngine(t, "keys: [{name: laptop, key: sk-laptop, budgets: {claude: {own_share: {5h: 100}}}}]")
	big := newSim(e, c, "big", 400)
	small := newSim(e, c, "small", 100)
	for i := 0; i < 60; i++ {
		big.request("")
		small.request("")
		c.add(time.Minute)
	}
	e.mu.Lock()
	wu, _ := e.poolWindowLocked("claude", "5h", "laptop", c.t)
	e.mu.Unlock()
	// big at 15%, small at 60%; weighted by capacity 4:1 -> (15*4+60)/5 = 24%.
	near(t, "pool util", wu.PoolUtil, 24, 4)
	near(t, "pool capacity", wu.PoolCapacity, 500, 100)
}

func TestScopedWindowOnlyCountsMatchingModels(t *testing.T) {
	e, c := testEngine(t, "keys: [{name: laptop, key: sk-laptop, budgets: {claude: {own_share: {7d:fable: 10}}}}]")
	e.ApplyObservations("acct-1", "claude", []Observation{
		{Window: "7d:fable", Scope: "model:fable", Util: 0, ResetsAt: t0.Add(48 * time.Hour), LengthSec: 7 * 86400}})
	e.HandleUsage(Usage{APIKey: "sk-laptop", AuthID: "acct-1", Provider: "claude", Model: "claude-opus-5-5", Tokens: Tokens{Output: 1e6}})
	if got := e.accounts["acct-1"].Windows["7d:fable"].KeyCost["laptop"]; got != 0 {
		t.Fatalf("opus traffic counted in fable window: %v", got)
	}
	e.HandleUsage(Usage{APIKey: "sk-laptop", AuthID: "acct-1", Provider: "claude", Model: "claude-fable-5-1", Tokens: Tokens{Output: 1e6}})
	if got := e.accounts["acct-1"].Windows["7d:fable"].KeyCost["laptop"]; got != 50 {
		t.Fatalf("fable traffic not counted: %v", got)
	}
	e.mu.Lock()
	applies := e.appliesLocked("claude", "7d:fable", "claude-opus-5-5")
	e.mu.Unlock()
	if applies {
		t.Fatal("fable limit should not apply to opus")
	}
	_ = c
}

func TestPersistence(t *testing.T) {
	e, c := testEngine(t, "keys: [{name: laptop, key: sk-laptop}]")
	s := newSim(e, c, "acct-1", 100)
	for i := 0; i < 20; i++ {
		s.request("sk-laptop")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := e.Save(path); err != nil {
		t.Fatal(err)
	}
	e2, _ := testEngine(t, "keys: [{name: laptop, key: sk-laptop}]")
	if err := e2.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := e2.accounts["acct-1"].Windows["5h"].KeyCost["laptop"]; got != 20 {
		t.Fatalf("key cost after reload: %v", got)
	}
}

func TestRoughEstimateNeverBlocks(t *testing.T) {
	e, c := testEngine(t, "keys: [{name: laptop, key: sk-laptop, budgets: {claude: {own_share: {5h: 10}}}}]")
	s := newSim(e, c, "acct-1", 1000)
	s.request("sk-laptop")
	s.used += 40 // heavy use outside the proxy: one segment with a tiny cost/rise ratio
	s.request("sk-laptop")
	s.request("sk-laptop")
	if _, q := e.accounts["acct-1"].Capacity("5h"); Trusted(q) {
		t.Fatalf("quality %s should not be trusted yet", q)
	}
	if d := e.Check(CallerScope("sk-laptop"), "claude-opus-5-5"); !d.Allow {
		t.Fatalf("blocked on a rough estimate: %+v", d)
	}
	e.mu.Lock()
	wu, _ := e.poolWindowLocked("claude", "5h", "laptop", c.t)
	e.mu.Unlock()
	if wu.OwnKnown {
		t.Fatalf("own share reported as known before calibration: %+v", wu)
	}
}

// poll applies a usage-endpoint reading. Live, the endpoint runs about a point ahead of the
// headers (e.g. 70 vs 0.69 at the same moment); rounding instead of flooring reproduces that.
func (s *sim) poll() {
	s.e.ApplyObservations(s.id, "claude", []Observation{
		{Window: "5h", Util: math.Round(100 * s.used / s.cap5h), ResetsAt: s.reset5h, LengthSec: 5 * 3600},
		{Window: "7d", Util: math.Round(100 * (s.base7d/100 + s.used/(10*s.cap5h))), ResetsAt: s.reset7d, LengthSec: 7 * 86400},
	})
}

func TestMixedSourcesDoNotBiasCalibration(t *testing.T) {
	e, c := testEngine(t, "keys: [{name: laptop, key: sk-laptop}]")
	s := newSim(e, c, "acct-1", 200)
	for i := 0; i < 180; i++ {
		s.request("sk-laptop")
		if i%3 == 0 {
			s.poll()
		}
		c.add(time.Minute)
	}
	a := e.accounts["acct-1"]
	cap5, q5 := a.Capacity("5h")
	if q5 != QualityCalibrated {
		t.Fatalf("5h quality %s", q5)
	}
	near(t, "5h capacity", cap5, 200, 20)
	cap7, q7 := a.Capacity("7d")
	if q7 == QualityNone {
		t.Fatalf("7d quality %s", q7)
	}
	near(t, "7d capacity", cap7, 2000, 300)
}

func TestStrayLowReadingIsNotAReset(t *testing.T) {
	e, c := testEngine(t, "keys: [{name: laptop, key: sk-laptop}]")
	s := newSim(e, c, "acct-1", 200)
	for i := 0; i < 30; i++ {
		s.request("sk-laptop")
		c.add(time.Minute)
	}
	w := e.accounts["acct-1"].Windows["5h"]
	start, cost := w.PeriodStart, w.ProxyCost
	// Another account's headers booked here by mistake: same reset time, much lower utilization.
	e.HandleUsage(Usage{AuthID: "acct-1", Provider: "claude", Headers: http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.02"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {fmt.Sprint(s.reset5h.Unix())},
	}})
	if !w.PeriodStart.Equal(start) || w.ProxyCost != cost || w.Util < 14 {
		t.Fatalf("stray reading reset the window: start %v, cost %v, util %v", w.PeriodStart, w.ProxyCost, w.Util)
	}
}

func TestOldCalibrationIsDiscarded(t *testing.T) {
	path := t.TempDir() + "/state.json"
	old := `{"version": 1, "accounts": {"acct-1": {"id": "acct-1", "provider": "claude",
		"ratios": {"7d": {"base": "5h", "base_u": 100, "oth_u": 25}},
		"windows": {"5h": {"id": "5h", "util": 40, "proxy_cost": 12.5,
			"segments": [{"d_cost": 3, "d_util": 3}, {"d_cost": 3, "d_util": 3}, {"d_cost": 3, "d_util": 3}]}}}}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	e, _ := testEngine(t, "")
	if err := e.Load(path); err != nil {
		t.Fatal(err)
	}
	a := e.accounts["acct-1"]
	if w := a.Windows["5h"]; len(w.Segments) != 0 || a.Ratios != nil || w.ProxyCost != 12.5 {
		t.Fatalf("segments %v, ratios %v, cost %v", w.Segments, a.Ratios, w.ProxyCost)
	}
}

func TestPlanUpgradeIsAcceptedAndRecalibrates(t *testing.T) {
	e, c := testEngine(t, "keys: [{name: laptop, key: sk-laptop}]")
	s := newSim(e, c, "acct-1", 200)
	for i := 0; i < 120; i++ {
		s.request("sk-laptop")
		c.add(time.Minute)
	}
	a := e.accounts["acct-1"]
	if _, q := a.Capacity("5h"); q != QualityCalibrated {
		t.Fatalf("not calibrated before the upgrade: %s", q)
	}
	// The plan grows 5x: the same usage now reads as a fifth, within the same periods.
	s.cap5h *= 5
	s.request("sk-laptop")
	c.add(time.Minute)
	if w := a.Windows["5h"]; w.Util < 50 {
		t.Fatalf("a single lower reading was accepted: %v", w.Util)
	}
	s.request("sk-laptop")
	c.add(time.Minute)
	if w := a.Windows["5h"]; w.Util > 20 {
		t.Fatalf("confirmed drop not accepted: util %v", w.Util)
	}
	if _, q := a.Capacity("5h"); q == QualityCalibrated {
		t.Fatal("old calibration kept after the limit changed")
	}
	if a.Ratios != nil || len(a.Windows["7d"].Segments) != 0 {
		t.Fatal("other windows kept their calibration")
	}
	for i := 0; i < 240; i++ {
		s.request("sk-laptop")
		c.add(time.Minute)
	}
	cap5, q5 := a.Capacity("5h")
	if q5 != QualityCalibrated {
		t.Fatalf("not recalibrated: %s", q5)
	}
	near(t, "5h capacity after upgrade", cap5, 1000, 100)
}
