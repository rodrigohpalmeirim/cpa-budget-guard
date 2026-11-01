package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
	"time"

	"budgetguard/guard"
)

// t3GatewayLimit is T3 Code's parser for a gateway rate limit in Claude Code's result text
// (providerFailureFromResult); Claude Code prefixes the body with "API Error: 503 ".
var t3GatewayLimit = regexp.MustCompile(`^API Error: 503 \[[^\]\r\n]+\] \[429\]: \{"type":"error","error":\{"type":"rate_limit_error",[^\r\n]*\} \(reset after (?:(\d+)h ?)?(?:(\d+)m ?)?(?:(\d+)s)?\)`)

func TestRejectionClaudeCarriesResetForT3(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	d := guard.Decision{Key: "laptop", Provider: "claude", Window: "5h", Kind: "own_share", Used: 51, Limit: 50,
		RetryAfter: now.Add(time.Hour + 2*time.Minute + 2*time.Second)}
	status, h, body := rejection("claude", d, now)
	if status != http.StatusServiceUnavailable || h.Get("X-Should-Retry") != "false" {
		t.Fatalf("status %d, headers %v", status, h)
	}
	m := t3GatewayLimit.FindStringSubmatch("API Error: 503 " + string(body) + ". This is a server-side issue")
	if m == nil || m[1] != "1" || m[2] != "2" || m[3] != "3" {
		t.Fatalf("T3 would not parse %q: %v", body, m)
	}

	d.RetryAfter = time.Time{}
	status, _, body = rejection("claude", d, now)
	var v struct {
		Error struct{ Type, Message string }
	}
	if status != http.StatusTooManyRequests || json.Unmarshal(body, &v) != nil || v.Error.Type != "rate_limit_error" {
		t.Fatalf("without a reset time: %d %s", status, body)
	}
}

func TestRejectionCodexUsageLimit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(3 * 24 * time.Hour)
	d := guard.Decision{Key: "ci", Provider: "codex", Window: "7d", Kind: "pool_ceiling", Used: 90, Limit: 90, RetryAfter: reset}
	status, h, body := rejection("openai-response", d, now)
	var v struct {
		Error struct {
			Type     string `json:"type"`
			ResetsAt int64  `json:"resets_at"`
		} `json:"error"`
	}
	if status != http.StatusTooManyRequests || json.Unmarshal(body, &v) != nil {
		t.Fatalf("%d %s", status, body)
	}
	if v.Error.Type != "usage_limit_reached" || v.Error.ResetsAt != reset.Unix() {
		t.Fatalf("body %s", body)
	}
	if h.Get("X-Codex-Secondary-Used-Percent") != "100" || h.Get("X-Codex-Secondary-Window-Minutes") != "10080" ||
		h.Get("X-Codex-Secondary-Reset-At") != "1800259200" || h.Get("X-Codex-Primary-Used-Percent") != "" {
		t.Fatalf("headers %v", h)
	}
}

func TestResetAfterAndWindowMinutes(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Second: "5s", 61 * time.Second: "1m1s", 2 * time.Hour: "2h0m0s"} {
		if got := resetAfter(d); got != want {
			t.Errorf("resetAfter(%v) = %q, want %q", d, got, want)
		}
	}
	for id, want := range map[string]int{"5h": 300, "7d": 10080, "7d:fable": 10080, "x": 0} {
		if got := windowMinutes(id); got != want {
			t.Errorf("windowMinutes(%q) = %d, want %d", id, got, want)
		}
	}
}

func TestHubRejectsUnknownKeys(t *testing.T) {
	engine.Configure(guard.Config{Keys: []guard.KeyConfig{{Name: "laptop", Key: "sk-good"}}})
	for _, key := range []string{"", "Bearer ", "Bearer sk-bad", "sk-bad"} {
		raw, err := handleHub(managementRequest{Method: "GET", Headers: http.Header{hubKeyHeader: {key}}}, "auth-files")
		var env struct {
			Result managementResponse `json:"result"`
		}
		if err != nil || json.Unmarshal(raw, &env) != nil || env.Result.StatusCode != http.StatusUnauthorized {
			t.Fatalf("key %q: %s %v", key, raw, err)
		}
	}
	if _, ok := engine.KeyName("Bearer sk-good"); ok {
		t.Fatal("KeyName must take the bare key")
	}
	if name, ok := engine.KeyName("sk-good"); !ok || name != "laptop" {
		t.Fatal("known key not found")
	}
}
