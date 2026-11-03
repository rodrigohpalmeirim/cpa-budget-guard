package guard

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is one finished request as the engine booked it. The request log keeps one per line, so
// cache use, cost per key and the capacity estimates can be checked after the fact.
type Entry struct {
	At         time.Time            `json:"t"`
	Key        string               `json:"key,omitempty"` // budgeted key name; empty for other keys
	Provider   string               `json:"provider"`
	Account    string               `json:"account"`
	Model      string               `json:"model,omitempty"`
	Failed     bool                 `json:"failed,omitempty"`
	Input      int64                `json:"in,omitempty"`
	Output     int64                `json:"out,omitempty"`
	Cached     int64                `json:"cached,omitempty"` // OpenAI/Codex: cached subset of input
	CacheRead  int64                `json:"cr,omitempty"`
	CacheWrite int64                `json:"cw,omitempty"`
	Cost       float64              `json:"cost"`
	Windows    map[string]EntryNote `json:"w,omitempty"`
	// Poll marks a usage-endpoint reading rather than a request: it carries utilization only,
	// so the history covers idle periods too. Statistics skip these.
	Poll bool `json:"poll,omitempty"`
}

// PollEntry records one usage-endpoint poll of an account.
func PollEntry(accountID, provider string, obs []Observation, now time.Time) Entry {
	e := Entry{At: now, Poll: true, Provider: provider, Account: accountID, Windows: map[string]EntryNote{}}
	for _, o := range obs {
		u := o.Util
		n := EntryNote{Util: &u}
		if !o.ResetsAt.IsZero() {
			n.Resets = o.ResetsAt.Unix()
		}
		e.Windows[o.Window] = n
	}
	return e
}

// EntryNote is one window as seen by a request: the utilization its response headers reported
// (usage before this request), and the capacity estimate the request was booked against.
type EntryNote struct {
	Util    *float64 `json:"u,omitempty"`
	Resets  int64    `json:"r,omitempty"`   // unix seconds of the reported reset
	Cap     float64  `json:"cap,omitempty"` // trusted capacity estimate (USD per 100%), 0 while calibrating
	Booked  bool     `json:"b,omitempty"`   // the request's cost counts towards this window
	Quality string   `json:"q,omitempty"`
}

// RequestLog appends entries to one JSON-lines file per UTC day and drops files past retention.
type RequestLog struct {
	mu   sync.Mutex
	dir  string
	keep int // days
	day  string
	f    *os.File
}

func NewRequestLog(dir string, keepDays int) *RequestLog {
	return &RequestLog{dir: dir, keep: keepDays}
}

func (l *RequestLog) Dir() string { return l.dir }

const logPrefix, logSuffix = "requests-", ".jsonl"

// Append writes one entry.
func (l *RequestLog) Append(e Entry) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	day := e.At.UTC().Format("2006-01-02")
	if l.f == nil || day != l.day {
		if l.f != nil {
			_ = l.f.Close()
			l.f = nil
		}
		if err := os.MkdirAll(l.dir, 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(l.dir, logPrefix+day+logSuffix), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		l.f, l.day = f, day
		l.pruneLocked(e.At)
	}
	_, err = l.f.Write(append(raw, '\n'))
	return err
}

func (l *RequestLog) pruneLocked(now time.Time) {
	if l.keep <= 0 {
		return
	}
	cutoff := now.UTC().AddDate(0, 0, -l.keep).Format("2006-01-02")
	files, _ := filepath.Glob(filepath.Join(l.dir, logPrefix+"*"+logSuffix))
	for _, f := range files {
		day := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), logPrefix), logSuffix)
		if day < cutoff {
			_ = os.Remove(f)
		}
	}
}

// Close closes the current file.
func (l *RequestLog) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

// Read returns the entries at or after since, oldest first.
func (l *RequestLog) Read(since time.Time) ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	files, err := filepath.Glob(filepath.Join(l.dir, logPrefix+"*"+logSuffix))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	first := since.UTC().Format("2006-01-02")
	var out []Entry
	for _, f := range files {
		day := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), logPrefix), logSuffix)
		if day < first {
			continue
		}
		fh, err := os.Open(f)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) == nil && !e.At.Before(since) {
				out = append(out, e)
			}
		}
		_ = fh.Close()
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// ---- statistics ----

// Totals sums a group of requests.
type Totals struct {
	Requests   int     `json:"requests"`
	Failed     int     `json:"failed"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	Cached     int64   `json:"cached"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Cost       float64 `json:"cost"`
	// CacheHit is the share of prompt tokens served from cache (Claude: reads over input+reads+writes;
	// Codex: cached over input). A broken session affinity shows as a low hit rate and many writes.
	CacheHit float64 `json:"cache_hit"`
	// WriteShare is the share of prompt tokens written to cache.
	WriteShare float64 `json:"write_share"`
}

func (t *Totals) add(e Entry) {
	t.Requests++
	if e.Failed {
		t.Failed++
	}
	t.Input += e.Input
	t.Output += e.Output
	t.Cached += e.Cached
	t.CacheRead += e.CacheRead
	t.CacheWrite += e.CacheWrite
	t.Cost += e.Cost
}

func (t *Totals) finish() {
	if prompt := t.Input + t.CacheRead + t.CacheWrite; t.CacheRead+t.CacheWrite > 0 && prompt > 0 {
		t.CacheHit = float64(t.CacheRead) / float64(prompt)
		t.WriteShare = float64(t.CacheWrite) / float64(prompt)
	} else if t.Input > 0 {
		t.CacheHit = float64(t.Cached) / float64(t.Input)
	}
}

// Group is the totals for one key, account or model.
type Group struct {
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
	Totals
}

// Accuracy compares, for one account window, the utilization the capacity estimate predicts for the
// proxy's own traffic with the rise the provider actually reported over the same requests.
// Usage outside the proxy only adds to the reported rise, so Explained <= 1 is expected; well above
// 1 means the capacity is underestimated, well below it means outside usage or an overestimate.
type Accuracy struct {
	Account   string  `json:"account"`
	Provider  string  `json:"provider"`
	Window    string  `json:"window"`
	Predicted float64 `json:"predicted"` // percentage points from proxy cost / capacity
	Reported  float64 `json:"reported"`  // percentage points the headers rose
	Explained float64 `json:"explained"` // Predicted / Reported
	Cost      float64 `json:"cost"`
	Intervals int     `json:"intervals"`
}

// Stats is the request log summarised over a period.
type Stats struct {
	Since    time.Time  `json:"since"`
	Until    time.Time  `json:"until"`
	Total    Totals     `json:"total"`
	Keys     []Group    `json:"keys"`
	Accounts []Group    `json:"accounts"`
	Models   []Group    `json:"models"`
	Accuracy []Accuracy `json:"accuracy"`
	LogDir   string     `json:"log_dir,omitempty"`
	LogFrom  time.Time  `json:"log_from,omitempty"` // oldest entry in the period
}

// Summarise builds Stats from entries (oldest first).
func Summarise(entries []Entry, since, until time.Time) Stats {
	st := Stats{Since: since, Until: until}
	keys, accts, models := map[string]*Group{}, map[string]*Group{}, map[string]*Group{}
	group := func(m map[string]*Group, name, provider string) *Group {
		id := provider + "\x00" + name
		if m[id] == nil {
			m[id] = &Group{Name: name, Provider: provider}
		}
		return m[id]
	}
	byAccount := map[string][]Entry{}
	for _, e := range entries {
		if e.Poll {
			continue
		}
		if st.LogFrom.IsZero() {
			st.LogFrom = e.At
		}
		st.Total.add(e)
		key := e.Key
		if key == "" {
			key = "(unbudgeted)"
		}
		group(keys, key, "").add(e)
		group(accts, e.Account, e.Provider).add(e)
		group(models, e.Model, e.Provider).add(e)
		byAccount[e.Provider+"\x00"+e.Account] = append(byAccount[e.Provider+"\x00"+e.Account], e)
	}
	st.Total.finish()
	flatten := func(m map[string]*Group) []Group {
		out := make([]Group, 0, len(m))
		for _, g := range m {
			g.finish()
			out = append(out, *g)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Cost > out[j].Cost })
		return out
	}
	st.Keys, st.Accounts, st.Models = flatten(keys), flatten(accts), flatten(models)
	for _, id := range sortedKeys(byAccount) {
		st.Accuracy = append(st.Accuracy, accuracy(byAccount[id])...)
	}
	return st
}

// accuracy walks one account's requests window by window. Between two header readings in the same
// period, the requests booked in between (from the first reading up to, not including, the second)
// predict a rise of sum(cost/cap)*100; the readings give the reported rise.
func accuracy(entries []Entry) []Accuracy {
	if len(entries) == 0 {
		return nil
	}
	windows := map[string]bool{}
	for _, e := range entries {
		for w := range e.Windows {
			windows[w] = true
		}
	}
	var out []Accuracy
	for _, w := range sortedKeys(windows) {
		acc := Accuracy{Account: entries[0].Account, Provider: entries[0].Provider, Window: w}
		var lastU float64
		var lastR int64
		have, ok := false, true
		var pending, pendingCost float64
		for _, e := range entries {
			n, seen := e.Windows[w]
			// A lower reading in the same period is a response that started before the previous
			// one finished (concurrent requests): skip it rather than restart the interval from it.
			stale := seen && n.Util != nil && have && n.Resets == lastR && *n.Util < lastU
			if seen && n.Util != nil && !stale {
				samePeriod := have && n.Resets == lastR
				if samePeriod && ok && pendingCost > 0 {
					acc.Predicted += pending
					acc.Reported += *n.Util - lastU
					acc.Cost += pendingCost
					acc.Intervals++
				}
				lastU, lastR, have, ok = *n.Util, n.Resets, true, true
				pending, pendingCost = 0, 0
			}
			if have && seen && n.Booked && e.Cost > 0 {
				if n.Cap > 0 {
					pending += 100 * e.Cost / n.Cap
					pendingCost += e.Cost
				} else {
					ok = false // no trusted estimate for part of the interval
				}
			}
		}
		if acc.Intervals > 0 {
			if acc.Reported > 0 {
				acc.Explained = acc.Predicted / acc.Reported
			}
			out = append(out, acc)
		}
	}
	return out
}
