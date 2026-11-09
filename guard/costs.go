package guard

import (
	"math"
	"sort"
	"time"
)

// Costs is the request log's spend over a period, broken down for charting: one row per time
// bucket and combination of key, model, account and agent (main thread or subagent), with tokens
// and cost per billing type. Clients filter and regroup the rows themselves.
type Costs struct {
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
	StepSec int64     `json:"step_sec"`
	LogFrom time.Time `json:"log_from,omitempty"` // first request in the period
	Types   []string  `json:"types"`
	// Keys, Models and Accounts are what the rows' indexes point into. Key "" is traffic from
	// keys without a budget.
	Keys     []string      `json:"keys"`
	Models   []CostModel   `json:"models"`
	Accounts []CostAccount `json:"accounts"`
	// Columns names the fields of each row.
	Columns []string    `json:"columns"`
	Rows    [][]float64 `json:"rows"`
	// Previous is each key's totals over the period of the same length before this one, and
	// PreviousFrom the log's first entry in it, polls included (a young log covers only part).
	Previous     map[string]CostTotals `json:"previous"`
	PreviousFrom time.Time             `json:"previous_from,omitempty"`
	// Conversations are the costliest sessions, subagents folded into the session that started them.
	Conversations []Conversation `json:"conversations"`
}

type CostModel struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

type CostAccount struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider"`
}

type CostTotals struct {
	Requests int     `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
	NoCache  float64 `json:"no_cache"`
}

// Conversation is one root session and the subagents it started.
type Conversation struct {
	ID        string             `json:"id"` // session hash
	Key       string             `json:"key"`
	Provider  string             `json:"provider"`
	Model     string             `json:"model"` // the costliest model
	Start     time.Time          `json:"start"`
	End       time.Time          `json:"end"`
	Requests  int                `json:"requests"`
	Subagents int                `json:"subagents"`
	Cost      float64            `json:"cost"`
	Parts     [NumTypes]float64  `json:"parts"`    // cost per billing type
	SubCost   float64            `json:"sub_cost"` // the subagents' part of Cost
	models    map[string]float64 // cost per model
	keys      map[string]float64
	subs      map[string]bool
}

// costColumns are the fields of a row; the per-type groups follow TypeNames' order.
var costColumns = []string{"bucket", "key", "model", "account", "subagent", "requests",
	"tok_input", "tok_output", "tok_cache_read", "tok_cache_write", "tok_cache_write_1h",
	"usd_input", "usd_output", "usd_cache_read", "usd_cache_write", "usd_cache_write_1h", "usd_no_cache"}

// CostsBuilder accumulates entries into a Costs report.
type CostsBuilder struct {
	rules       []PriceRule
	since, prev time.Time
	until       time.Time
	step        time.Duration
	labels      map[string]string
	out         Costs
	keyIdx      map[string]int
	modelIdx    map[CostModel]int
	acctIdx     map[string]int
	rows        map[[5]int][]float64
	parent      map[string]string // session -> parent session
	sessions    map[string]*Conversation
}

// NewCostsBuilder reports on [since, until) in buckets of step; entries in the period of the same
// length before since count towards Previous. labels maps account IDs to display labels.
func NewCostsBuilder(rules []PriceRule, since, until time.Time, step time.Duration, labels map[string]string) *CostsBuilder {
	since = since.Truncate(step)
	b := &CostsBuilder{rules: rules, since: since, prev: since.Add(-until.Sub(since)), until: until, step: step,
		labels: labels, keyIdx: map[string]int{}, modelIdx: map[CostModel]int{}, acctIdx: map[string]int{},
		rows: map[[5]int][]float64{}, parent: map[string]string{}, sessions: map[string]*Conversation{}}
	b.out = Costs{Since: since, Until: until, StepSec: int64(step / time.Second), Types: TypeNames[:],
		Columns: costColumns, Previous: map[string]CostTotals{}}
	return b
}

// From is the earliest entry the builder needs.
func (b *CostsBuilder) From() time.Time { return b.prev }

// breakdown prices an entry per type, scaled so the parts add up to the cost it was booked at
// (pricing may have changed since).
func (b *CostsBuilder) breakdown(e Entry) Breakdown {
	bd := Split(b.rules, e.Provider, e.Model, Tokens{Input: e.Input, Output: e.Output, Cached: e.Cached,
		CacheRead: e.CacheRead, CacheCreate: e.CacheWrite, CacheCreate1h: e.CacheWrite1h})
	total := bd.Total()
	switch {
	case total > 0:
		f := e.Cost / total
		for i := range bd.Cost {
			bd.Cost[i] *= f
		}
		bd.NoCache *= f
	case e.Cost > 0:
		bd.Cost[TypeInput], bd.NoCache = e.Cost, e.Cost
	}
	return bd
}

func index[K comparable](m map[K]int, k K, list *[]K) int {
	if i, ok := m[k]; ok {
		return i
	}
	m[k] = len(*list)
	*list = append(*list, k)
	return m[k]
}

// Add books one log entry. Polls and entries outside the periods are ignored.
func (b *CostsBuilder) Add(e Entry) {
	if e.At.Before(b.prev) || !e.At.Before(b.until) {
		return
	}
	// Polls are logged around the clock, so the first entry of any kind shows how far back the
	// log reaches, idle hours included.
	if e.At.Before(b.since) && (b.out.PreviousFrom.IsZero() || e.At.Before(b.out.PreviousFrom)) {
		b.out.PreviousFrom = e.At
	}
	if e.Poll {
		return
	}
	bd := b.breakdown(e)
	var tokens int64
	for _, n := range bd.Tokens {
		tokens += n
	}
	if e.At.Before(b.since) {
		p := b.out.Previous[e.Key]
		p.Requests++
		p.Tokens += tokens
		p.Cost += e.Cost
		p.NoCache += bd.NoCache
		b.out.Previous[e.Key] = p
		return
	}
	if b.out.LogFrom.IsZero() || e.At.Before(b.out.LogFrom) {
		b.out.LogFrom = e.At
	}
	ki := index(b.keyIdx, e.Key, &b.out.Keys)
	mi := index(b.modelIdx, CostModel{Name: e.Model, Provider: e.Provider}, &b.out.Models)
	if _, ok := b.acctIdx[e.Account]; !ok {
		label := b.labels[e.Account]
		if label == "" {
			label = e.Account
		}
		b.acctIdx[e.Account] = len(b.out.Accounts)
		b.out.Accounts = append(b.out.Accounts, CostAccount{ID: e.Account, Label: label, Provider: e.Provider})
	}
	sub := 0
	if e.Parent != "" {
		sub = 1
	}
	id := [5]int{int(e.At.Sub(b.since) / b.step), ki, mi, b.acctIdx[e.Account], sub}
	row := b.rows[id]
	if row == nil {
		row = make([]float64, len(costColumns))
		for i, v := range id {
			row[i] = float64(v)
		}
		b.rows[id] = row
	}
	row[5]++
	for t := 0; t < NumTypes; t++ {
		row[6+t] += float64(bd.Tokens[t])
		row[6+NumTypes+t] += bd.Cost[t]
	}
	row[6+2*NumTypes] += bd.NoCache
	b.addSession(e, bd)
}

func (b *CostsBuilder) addSession(e Entry, bd Breakdown) {
	if e.Session == "" {
		return
	}
	if e.Parent != "" {
		b.parent[e.Session] = e.Parent
	}
	c := b.sessions[e.Session]
	if c == nil {
		c = &Conversation{ID: e.Session, Provider: e.Provider, Start: e.At, models: map[string]float64{},
			keys: map[string]float64{}, subs: map[string]bool{}}
		b.sessions[e.Session] = c
	}
	if e.At.Before(c.Start) {
		c.Start = e.At
	}
	if e.At.After(c.End) {
		c.End = e.At
	}
	c.Requests++
	c.Cost += e.Cost
	for t := range bd.Cost {
		c.Parts[t] += bd.Cost[t]
	}
	c.models[e.Model] += e.Cost
	c.keys[e.Key] += e.Cost
}

// root follows parent sessions up to the first one without a parent.
func (b *CostsBuilder) root(id string) string {
	for range 16 {
		p, ok := b.parent[id]
		if !ok || p == id {
			break
		}
		id = p
	}
	return id
}

// conversationsPerKey is how many of the costliest conversations are kept for each key.
const conversationsPerKey = 25

// Finish returns the report.
func (b *CostsBuilder) Finish() Costs {
	out := b.out
	out.Rows = make([][]float64, 0, len(b.rows))
	ids := make([][5]int, 0, len(b.rows))
	for id := range b.rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		for k := range ids[i] {
			if ids[i][k] != ids[j][k] {
				return ids[i][k] < ids[j][k]
			}
		}
		return false
	})
	for _, id := range ids {
		row := b.rows[id]
		for i := 6 + NumTypes; i < len(row); i++ {
			row[i] = round6(row[i])
		}
		out.Rows = append(out.Rows, row)
	}
	for k, p := range out.Previous {
		p.Cost, p.NoCache = round6(p.Cost), round6(p.NoCache)
		out.Previous[k] = p
	}

	// Fold subagent sessions into their root conversation.
	roots := map[string]*Conversation{}
	for _, id := range sortedKeys(b.sessions) {
		s := b.sessions[id]
		rid := b.root(id)
		r := roots[rid]
		if r == nil {
			r = &Conversation{ID: rid, Provider: s.Provider, Start: s.Start, End: s.End, models: map[string]float64{},
				keys: map[string]float64{}, subs: map[string]bool{}}
			roots[rid] = r
		}
		if rid != id {
			r.subs[id] = true
			r.SubCost += s.Cost
		}
		if s.Start.Before(r.Start) {
			r.Start = s.Start
		}
		if s.End.After(r.End) {
			r.End = s.End
		}
		r.Requests += s.Requests
		r.Cost += s.Cost
		for t := range s.Parts {
			r.Parts[t] += s.Parts[t]
		}
		for m, c := range s.models {
			r.models[m] += c
		}
		for k, c := range s.keys {
			r.keys[k] += c
		}
	}
	top := func(m map[string]float64) string {
		best, bestC := "", -1.0
		for _, k := range sortedKeys(m) {
			if m[k] > bestC {
				best, bestC = k, m[k]
			}
		}
		return best
	}
	all := make([]Conversation, 0, len(roots))
	for _, r := range roots {
		r.Key, r.Model, r.Subagents = top(r.keys), top(r.models), len(r.subs)
		r.Cost, r.SubCost = round6(r.Cost), round6(r.SubCost)
		for t := range r.Parts {
			r.Parts[t] = round6(r.Parts[t])
		}
		all = append(all, *r)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Cost != all[j].Cost {
			return all[i].Cost > all[j].Cost
		}
		return all[i].ID < all[j].ID
	})
	perKey := map[string]int{}
	out.Conversations = []Conversation{}
	for _, c := range all {
		if perKey[c.Key] < conversationsPerKey {
			perKey[c.Key]++
			out.Conversations = append(out.Conversations, c)
		}
	}
	if out.Keys == nil {
		out.Keys, out.Models, out.Accounts = []string{}, []CostModel{}, []CostAccount{}
	}
	return out
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
