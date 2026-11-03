package guard

import (
	"sort"
	"time"
)

// TimelineAccount is one account of the pool window being charted, as it stands now.
type TimelineAccount struct {
	ID       string    `json:"id"`
	Label    string    `json:"label"`
	Capacity float64   `json:"capacity_usd"` // trusted estimate; 0 while calibrating
	Quality  string    `json:"calibration"`
	Util     float64   `json:"util_pct"`
	ResetsAt time.Time `json:"resets_at"`
	// LastRequest is when the account last served a request through the proxy (zero if not in the log).
	LastRequest time.Time `json:"last_request,omitempty"`
	// Weight is the account's share of the pool used for the chart (capacity, or the mean of
	// the known capacities while this one is calibrating).
	Weight float64 `json:"weight"`
	// Series is the account's utilization at each step (nil before its first reading).
	Series []*float64 `json:"series"`
}

// Timeline is one provider window over time: pool remaining per step and spend per key.
type Timeline struct {
	Provider  string             `json:"provider"`
	Window    string             `json:"window"`
	LengthSec int64              `json:"length_sec"`
	Start     time.Time          `json:"start"`
	StepSec   int64              `json:"step_sec"`
	Now       time.Time          `json:"now"`
	Accounts  []*TimelineAccount `json:"accounts"`
	// Remaining is the pool's remaining percent at each step, weighted by account size
	// (nil where no account had a reading yet).
	Remaining []*float64 `json:"remaining"`
	// Spend is the API-equivalent cost booked to this window per step, by key name
	// ("" = keys without a budget, e.g. the admin key).
	Spend []map[string]float64 `json:"spend"`
	Keys  []string             `json:"keys"`
}

type reading struct {
	at     time.Time
	util   float64
	resets time.Time
}

// BuildTimeline charts provider/window from start to now in steps, from the request log entries
// (requests and polls, oldest first) and the accounts' current standing.
func BuildTimeline(entries []Entry, accounts []TimelineAccount, provider, window string, lengthSec int64,
	start, now time.Time, step time.Duration) Timeline {
	n := int(now.Sub(start)/step) + 1
	if n < 1 {
		n = 1
	}
	tl := Timeline{Provider: provider, Window: window, LengthSec: lengthSec, Start: start, StepSec: int64(step / time.Second),
		Now: now, Remaining: make([]*float64, n), Spend: make([]map[string]float64, n)}

	known, sum := 0, 0.0
	for _, a := range accounts {
		if a.Capacity > 0 {
			known++
			sum += a.Capacity
		}
	}
	idx := map[string]*TimelineAccount{}
	for i := range accounts {
		a := accounts[i]
		switch {
		case a.Capacity > 0:
			a.Weight = a.Capacity
		case known > 0:
			a.Weight = sum / float64(known)
		default:
			a.Weight = 1
		}
		a.Series = make([]*float64, n)
		idx[a.ID] = &a
		tl.Accounts = append(tl.Accounts, &a)
	}

	readings := map[string][]reading{}
	keys := map[string]bool{}
	for _, e := range entries {
		if e.Provider != provider || idx[e.Account] == nil {
			continue
		}
		if a := idx[e.Account]; !e.Poll && e.At.After(a.LastRequest) {
			a.LastRequest = e.At
		}
		note, ok := e.Windows[window]
		if !ok {
			continue
		}
		if note.Util != nil {
			r := reading{at: e.At, util: *note.Util}
			if note.Resets != 0 {
				r.resets = time.Unix(note.Resets, 0)
			}
			readings[e.Account] = append(readings[e.Account], r)
		}
		if !e.Poll && note.Booked && e.Cost > 0 && !e.At.Before(start) {
			i := int(e.At.Sub(start) / step)
			if i >= 0 && i < n {
				if tl.Spend[i] == nil {
					tl.Spend[i] = map[string]float64{}
				}
				tl.Spend[i][e.Key] += e.Cost
				keys[e.Key] = true
			}
		}
	}
	tl.Keys = sortedKeys(keys)

	for id, rs := range readings {
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].at.Before(rs[j].at) })
		a := idx[id]
		j := 0
		var last *reading
		for i := 0; i < n; i++ {
			t := start.Add(time.Duration(i+1) * step)
			if t.After(now) {
				t = now
			}
			for j < len(rs) && !rs[j].at.After(t) {
				last = &rs[j]
				j++
			}
			if last == nil {
				continue
			}
			u := last.util
			if !last.resets.IsZero() && !t.Before(last.resets) {
				u = 0 // the period ended without a newer reading
			}
			a.Series[i] = &u
		}
	}
	// The newest point is the current standing, which may be newer than the log.
	for _, a := range tl.Accounts {
		u := a.Util
		a.Series[n-1] = &u
	}

	for i := 0; i < n; i++ {
		var w, rem float64
		for _, a := range tl.Accounts {
			if a.Series[i] == nil {
				continue
			}
			w += a.Weight
			rem += a.Weight * (100 - min(100, *a.Series[i]))
		}
		if w > 0 {
			v := rem / w
			tl.Remaining[i] = &v
		}
	}
	return tl
}
