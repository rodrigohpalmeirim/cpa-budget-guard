package guard

import (
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func ptr(f float64) *float64 { return &f }

func TestRequestLogAppendReadPrune(t *testing.T) {
	dir := t.TempDir()
	l := NewRequestLog(dir, 2)
	day := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	stale := filepath.Join(dir, "requests-2026-10-01.jsonl")
	if err := os.WriteFile(stale, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := l.Append(Entry{At: day.Add(time.Duration(i) * 24 * time.Hour), Provider: "claude", Account: "a", Cost: 1}); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("file past retention was kept")
	}
	got, err := l.Read(day.Add(24 * time.Hour))
	if err != nil || len(got) != 2 || !got[0].At.Equal(day.Add(24*time.Hour)) {
		t.Fatalf("read %v %v", got, err)
	}
}

func TestAccuracyMatchesTrueCapacity(t *testing.T) {
	// Capacity $200 per 100%: each $4 request adds 2 points. Headers report usage before the request.
	var entries []Entry
	at := time.Unix(1_800_000_000, 0)
	util := 10.0
	for i := 0; i < 20; i++ {
		entries = append(entries, Entry{At: at, Provider: "claude", Account: "a", Cost: 4,
			Windows: map[string]EntryNote{"5h": {Util: ptr(util), Resets: 1_800_018_000, Cap: 200, Booked: true}}})
		util += 2
		at = at.Add(time.Minute)
	}
	st := Summarise(entries, at.Add(-time.Hour), at)
	if len(st.Accuracy) != 1 {
		t.Fatalf("accuracy %+v", st.Accuracy)
	}
	a := st.Accuracy[0]
	if a.Intervals != 19 || math.Abs(a.Explained-1) > 1e-9 || math.Abs(a.Reported-38) > 1e-9 {
		t.Fatalf("got %+v", a)
	}
}

func TestAccuracySkipsResetsAndUncalibratedSpans(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	e := func(u float64, reset int64, cap float64) Entry {
		at = at.Add(time.Minute)
		return Entry{At: at, Provider: "claude", Account: "a", Cost: 2,
			Windows: map[string]EntryNote{"5h": {Util: ptr(u), Resets: reset, Cap: cap, Booked: true}}}
	}
	// Each interval is charged with the requests from its first reading up to the next one.
	entries := []Entry{
		e(50, 1, 100), // 50->52 counted: predicted 2, reported 2
		e(52, 1, 0),   // 52->54 skipped: booked while calibrating
		e(54, 1, 100), // 54->1 skipped: new period
		e(1, 2, 100),  // 1->3 counted: predicted 2, reported 2
		e(3, 2, 100),
	}
	a := Summarise(entries, time.Time{}, at).Accuracy
	if len(a) != 1 || a[0].Intervals != 2 || a[0].Predicted != 4 || a[0].Reported != 4 {
		t.Fatalf("accuracy %+v", a)
	}
}

func TestCacheHitAndWriteShare(t *testing.T) {
	st := Summarise([]Entry{
		{Provider: "claude", Account: "a", Key: "k", Input: 10, CacheRead: 80, CacheWrite: 10, Cost: 1},
		{Provider: "codex", Account: "b", Input: 100, Cached: 60, Cost: 1},
	}, time.Time{}, time.Now())
	for _, g := range st.Accounts {
		switch g.Name {
		case "a":
			if g.CacheHit != 0.8 || g.WriteShare != 0.1 {
				t.Fatalf("claude %+v", g)
			}
		case "b":
			if g.CacheHit != 0.6 {
				t.Fatalf("codex %+v", g)
			}
		}
	}
	if st.Keys[0].Name != "k" && st.Keys[1].Name != "k" {
		t.Fatalf("keys %+v", st.Keys)
	}
}

func TestHandleUsageReturnsEntry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	e := NewEngine(func() time.Time { return now })
	e.Configure(Config{Keys: []KeyConfig{{Name: "laptop", Key: "sk-1"}}})
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1800010000")
	entry, ok := e.HandleUsage(Usage{APIKey: "sk-1", AuthID: "acct", Provider: "claude", Model: "claude-opus-5-5",
		Headers: h, Tokens: Tokens{Input: 1000, Output: 100, CacheRead: 5000}})
	if !ok || entry.Key != "laptop" || entry.Cost <= 0 || entry.CacheRead != 5000 {
		t.Fatalf("entry %+v", entry)
	}
	n := entry.Windows["5h"]
	if n.Util == nil || math.Abs(*n.Util-42) > 1e-9 || n.Resets != 1800010000 || !n.Booked || n.Quality != QualityNone {
		t.Fatalf("5h note %+v", n)
	}
}

func TestAccuracyIgnoresOutOfOrderReadings(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	var entries []Entry
	for _, u := range []float64{50, 49, 51, 52} { // 49 is a late response from before the 50 reading
		at = at.Add(time.Minute)
		entries = append(entries, Entry{At: at, Provider: "claude", Account: "a", Cost: 1,
			Windows: map[string]EntryNote{"5h": {Util: ptr(u), Resets: 9, Cap: 100, Booked: true}}})
	}
	a := Summarise(entries, time.Time{}, at).Accuracy
	if len(a) != 1 || a[0].Predicted != 3 || a[0].Reported != 2 {
		t.Fatalf("accuracy %+v", a)
	}
}
