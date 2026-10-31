package guard

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParseHeaders extracts limit windows from upstream response headers. It also returns the set
// of scoped limit IDs present (used to attribute Codex "additional" limits to requests).
func ParseHeaders(provider string, h http.Header, now time.Time) ([]Observation, map[string]bool) {
	switch provider {
	case "claude":
		return parseClaudeHeaders(h), nil
	case "codex":
		return parseCodexHeaders(h, now)
	}
	return nil, nil
}

func headerFloat(h http.Header, name string) (float64, bool) {
	v := strings.TrimSpace(h.Get(name))
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	return f, err == nil
}

func unixTime(f float64) time.Time {
	if f <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(f), 0).UTC()
}

// Anthropic: anthropic-ratelimit-unified-{5h,7d}-utilization (fraction 0..1) and -reset (unix seconds).
func parseClaudeHeaders(h http.Header) []Observation {
	var out []Observation
	for _, w := range []struct {
		hdr string
		id  string
		sec int64
	}{{"5h", "5h", 5 * 3600}, {"7d", "7d", 7 * 86400}} {
		util, ok := headerFloat(h, "Anthropic-Ratelimit-Unified-"+w.hdr+"-Utilization")
		if !ok {
			continue
		}
		reset, _ := headerFloat(h, "Anthropic-Ratelimit-Unified-"+w.hdr+"-Reset")
		out = append(out, Observation{Window: w.id, Util: util * 100, ResetsAt: unixTime(reset), LengthSec: w.sec})
	}
	return out
}

// OpenAI Codex: x-codex-[<limit>-]{primary,secondary}-{used-percent,window-minutes,reset-at,reset-after-seconds}.
var codexHeaderRe = regexp.MustCompile(`^x-codex-(?:([a-z0-9_]+)-)?(primary|secondary)-(used-percent|window-minutes|reset-at|reset-after-seconds)$`)

func parseCodexHeaders(h http.Header, now time.Time) ([]Observation, map[string]bool) {
	type win struct {
		used, minutes, resetAt, resetAfter float64
		hasUsed                            bool
	}
	wins := map[[2]string]*win{}
	for name := range h {
		m := codexHeaderRe.FindStringSubmatch(strings.ToLower(name))
		if m == nil {
			continue
		}
		f, ok := headerFloat(h, name)
		if !ok {
			continue
		}
		k := [2]string{m[1], m[2]}
		w := wins[k]
		if w == nil {
			w = &win{}
			wins[k] = w
		}
		switch m[3] {
		case "used-percent":
			w.used, w.hasUsed = f, true
		case "window-minutes":
			w.minutes = f
		case "reset-at":
			w.resetAt = f
		case "reset-after-seconds":
			w.resetAfter = f
		}
	}
	limits := map[string]bool{}
	var out []Observation
	for k, w := range wins {
		if !w.hasUsed {
			continue
		}
		reset := unixTime(w.resetAt)
		if reset.IsZero() && w.resetAfter > 0 {
			reset = now.Add(time.Duration(w.resetAfter) * time.Second).Truncate(time.Minute)
		}
		sec := int64(w.minutes * 60)
		if sec == 0 {
			sec = map[string]int64{"primary": 5 * 3600, "secondary": 7 * 86400}[k[1]]
		}
		o := Observation{Window: windowIDForLength(sec), Util: w.used, ResetsAt: reset, LengthSec: sec}
		if k[0] != "" {
			limits[k[0]] = true
			o.Window += ":" + k[0]
			o.Scope = "header:" + k[0]
		}
		out = append(out, o)
	}
	return out, limits
}

// ParseClaudeUsage parses https://api.anthropic.com/api/oauth/usage.
func ParseClaudeUsage(body []byte) ([]Observation, error) {
	var doc struct {
		Limits []struct {
			Kind     string       `json:"kind"`
			Group    string       `json:"group"`
			Percent  *float64     `json:"percent"`
			ResetsAt string       `json:"resets_at"`
			Scope    *claudeScope `json:"scope"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	var out []Observation
	for _, l := range doc.Limits {
		if l.Percent == nil {
			continue
		}
		o := Observation{Util: *l.Percent, ResetsAt: parseTime(l.ResetsAt)}
		switch l.Kind {
		case "session":
			o.Window, o.LengthSec = "5h", 5*3600
		case "weekly_all":
			o.Window, o.LengthSec = "7d", 7*86400
		case "weekly_scoped":
			name := scopeName(l.Scope)
			if name == "" {
				continue
			}
			o.Window, o.LengthSec, o.Scope = "7d:"+name, 7*86400, "model:"+name
		default:
			if l.Group == "weekly" {
				o.Window, o.LengthSec = "7d:"+sanitize(l.Kind), 7*86400
			} else {
				continue // unknown limit kinds are not attributable
			}
		}
		out = append(out, o)
	}
	if len(out) > 0 {
		return out, nil
	}
	// Older response shape: named windows with utilization in percent.
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(body, &legacy); err != nil {
		return nil, err
	}
	for field, spec := range map[string]struct {
		id, scope string
		sec       int64
	}{
		"five_hour":        {"5h", "", 5 * 3600},
		"seven_day":        {"7d", "", 7 * 86400},
		"seven_day_opus":   {"7d:opus", "model:opus", 7 * 86400},
		"seven_day_sonnet": {"7d:sonnet", "model:sonnet", 7 * 86400},
		"iguana_necktie":   {"7d:fable", "model:fable", 7 * 86400},
	} {
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		}
		if raw, ok := legacy[field]; !ok || json.Unmarshal(raw, &w) != nil || w.Utilization == nil {
			continue
		}
		out = append(out, Observation{Window: spec.id, Scope: spec.scope, Util: *w.Utilization, ResetsAt: parseTime(w.ResetsAt), LengthSec: spec.sec})
	}
	return out, nil
}

type claudeScope struct {
	Model *struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Surface any `json:"surface"`
}

func scopeName(scope *claudeScope) string {
	if scope == nil {
		return ""
	}
	if scope.Model != nil {
		if scope.Model.DisplayName != "" {
			return sanitize(scope.Model.DisplayName)
		}
		if scope.Model.ID != "" {
			return sanitize(scope.Model.ID)
		}
	}
	if s, ok := scope.Surface.(string); ok && s != "" {
		return sanitize(s)
	}
	return ""
}

// ParseCodexUsage parses https://chatgpt.com/backend-api/wham/usage.
func ParseCodexUsage(body []byte, now time.Time) ([]Observation, error) {
	type window struct {
		UsedPercent       *float64 `json:"used_percent"`
		LimitWindowSecs   int64    `json:"limit_window_seconds"`
		ResetAfterSeconds int64    `json:"reset_after_seconds"`
		ResetAt           int64    `json:"reset_at"`
	}
	type rateLimit struct {
		Primary   *window `json:"primary_window"`
		Secondary *window `json:"secondary_window"`
	}
	var doc struct {
		RateLimit  *rateLimit `json:"rate_limit"`
		Additional []struct {
			LimitName      string     `json:"limit_name"`
			MeteredFeature string     `json:"metered_feature"`
			RateLimit      *rateLimit `json:"rate_limit"`
		} `json:"additional_rate_limits"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	var out []Observation
	add := func(w *window, suffix, scope string) {
		if w == nil || w.UsedPercent == nil {
			return
		}
		reset := unixTime(float64(w.ResetAt))
		if reset.IsZero() && w.ResetAfterSeconds > 0 {
			reset = now.Add(time.Duration(w.ResetAfterSeconds) * time.Second).Truncate(time.Minute)
		}
		out = append(out, Observation{Window: windowIDForLength(w.LimitWindowSecs) + suffix, Util: *w.UsedPercent,
			ResetsAt: reset, LengthSec: w.LimitWindowSecs, Scope: scope})
	}
	if doc.RateLimit != nil {
		add(doc.RateLimit.Primary, "", "")
		add(doc.RateLimit.Secondary, "", "")
	}
	for _, a := range doc.Additional {
		name := sanitize(a.MeteredFeature)
		if name == "" {
			name = sanitize(a.LimitName)
		}
		if name == "" || a.RateLimit == nil {
			continue
		}
		add(a.RateLimit.Primary, ":"+name, "header:"+name)
		add(a.RateLimit.Secondary, ":"+name, "header:"+name)
	}
	return out, nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

var nonID = regexp.MustCompile(`[^a-z0-9_]+`)

func sanitize(s string) string {
	return strings.Trim(nonID.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "_"), "_")
}
