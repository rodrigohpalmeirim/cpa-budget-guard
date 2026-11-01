package guard

import (
	"sort"
	"strings"
	"time"
)

const (
	maxSegments      = 40
	resetTolerance   = 5 * time.Minute
	ratioMinBaseRise = 5.0 // percentage points of the base window needed before trusting a cross-window ratio
	// Headers stay the calibration source while one arrived this recently; otherwise polls take over.
	headerFresh = 15 * time.Minute
)

// Observation sources. They can disagree by a point at the same moment (the usage endpoint runs
// ahead of the response headers), so calibration never mixes them.
const (
	SourceHeader = "header"
	SourcePoll   = "poll"
)

// Observation is one reading of a limit window on one account.
type Observation struct {
	Window    string    // "5h", "7d", "7d:fable", "5h:bengalfox"
	Util      float64   // percent used, 0..100 (may exceed 100)
	ResetsAt  time.Time // zero when the window is idle / not started
	LengthSec int64
	Scope     string // "" (all traffic), "model:<substr>" or "header:<limit id>"
	Source    string // SourceHeader or SourcePoll
}

// Reading is the last utilization one source reported.
type Reading struct {
	Util float64   `json:"util"`
	At   time.Time `json:"at"`
}

// Window tracks one limit window of one account.
type Window struct {
	ID    string `json:"id"`
	Scope string `json:"scope,omitempty"`
	// Models learned for header-scoped windows: requests for these models have hit this limit.
	Models     map[string]bool `json:"models,omitempty"`
	LengthSec  int64           `json:"length_sec,omitempty"`
	Util       float64         `json:"util"`
	ResetsAt   time.Time       `json:"resets_at"`
	ObservedAt time.Time       `json:"observed_at"`

	// Accounting for the current period (since the last reset).
	PeriodStart time.Time          `json:"period_start"`
	ProxyCost   float64            `json:"proxy_cost"`         // all proxy traffic counted in this window
	KeyCost     map[string]float64 `json:"key_cost,omitempty"` // budgeted keys only

	// Calibration: cost that fills 100% of this window on this account. Edges are read from one
	// source at a time (EdgeSource), comparing each reading with that source's previous one.
	Anchor     *Anchor            `json:"anchor,omitempty"`
	Segments   []Segment          `json:"segments,omitempty"`
	EdgeSource string             `json:"edge_source,omitempty"`
	Last       map[string]Reading `json:"last,omitempty"` // per source, current period
}

// Anchor is the start of the open calibration segment.
type Anchor struct {
	Util float64   `json:"util"`
	Cost float64   `json:"cost"`
	At   time.Time `json:"at"`
}

// Segment is a closed calibration interval: proxy cost spent while utilization rose by DUtil.
// External usage (outside the proxy) only adds DUtil, so 100*DCost/DUtil is a lower bound on capacity.
type Segment struct {
	DCost float64   `json:"d_cost"`
	DUtil float64   `json:"d_util"`
	At    time.Time `json:"at"`
}

// Active reports whether the window's current period is still running.
func (w *Window) Active(now time.Time) bool {
	return !w.ResetsAt.IsZero() && now.Before(w.ResetsAt)
}

// EffectiveUtil is the utilization that still counts now (0 once the period has reset).
func (w *Window) EffectiveUtil(now time.Time) float64 {
	if !w.Active(now) {
		return 0
	}
	return w.Util
}

// expire starts a new period when the stored one has run out.
func (w *Window) expire(now time.Time) {
	if !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt) && (w.ProxyCost != 0 || len(w.KeyCost) != 0 || w.Anchor != nil) {
		w.newPeriod(now)
		w.Util = 0
	}
}

func (w *Window) newPeriod(now time.Time) {
	w.PeriodStart = now
	w.ProxyCost = 0
	w.KeyCost = nil
	w.Anchor = nil
	w.Last = nil
}

// AddCost books a request's cost in the current period.
func (w *Window) AddCost(key string, cost float64, now time.Time) {
	w.expire(now)
	if w.PeriodStart.IsZero() {
		w.PeriodStart = now
	}
	w.ProxyCost += cost
	if key != "" {
		if w.KeyCost == nil {
			w.KeyCost = map[string]float64{}
		}
		w.KeyCost[key] += cost
	}
}

// Observe applies a utilization reading, detecting resets and closing calibration segments.
func (w *Window) Observe(o Observation, now time.Time, step float64) {
	w.expire(now)
	if o.LengthSec > 0 {
		w.LengthSec = o.LengthSec
	}
	if o.Scope != "" {
		w.Scope = o.Scope
	}
	prev, seen := w.Last[o.Source]
	samePeriod := !w.ResetsAt.IsZero() && !o.ResetsAt.IsZero() && !o.ResetsAt.After(w.ResetsAt.Add(resetTolerance))
	if seen && samePeriod && o.Util+1.5 < prev.Util {
		return // utilization can't fall within a period: a stray reading
	}
	reset := false
	if !w.ResetsAt.IsZero() && !o.ResetsAt.IsZero() && o.ResetsAt.After(w.ResetsAt.Add(resetTolerance)) {
		reset = true // a later period has begun
	}
	if seen && o.ResetsAt.IsZero() && o.Util+1.5 < prev.Util {
		reset = true // utilization fell and no reset time is given: the window reset
	}
	if reset {
		w.newPeriod(now)
		seen = false
	}
	// Pick the calibration source: headers (one per request) while they keep coming, else polls.
	if w.EdgeSource != o.Source {
		last, ok := w.Last[w.EdgeSource]
		if w.EdgeSource == "" || o.Source == SourceHeader || !ok || now.Sub(last.At) > headerFresh {
			w.EdgeSource = o.Source
			w.Anchor = nil
		}
	}
	if w.Last == nil {
		w.Last = map[string]Reading{}
	}
	w.Last[o.Source] = Reading{Util: o.Util, At: now}
	w.Util = o.Util
	if !o.ResetsAt.IsZero() {
		w.ResetsAt = o.ResetsAt
	}
	w.ObservedAt = now
	if o.Source != w.EdgeSource {
		return
	}
	// Calibrate only at edges: the first reading after the whole-percent value rose. The true
	// utilization has just crossed the shown value there, so the rounding error of both ends is
	// limited to the usage between two consecutive readings.
	if w.Anchor != nil && o.Util < w.Anchor.Util {
		w.Anchor = nil
	}
	if edge := seen && o.Util > prev.Util; edge {
		if w.Anchor == nil {
			w.Anchor = &Anchor{Util: o.Util, Cost: w.ProxyCost, At: now}
		} else if dU := o.Util - w.Anchor.Util; dU >= step {
			if dC := w.ProxyCost - w.Anchor.Cost; dC > 0 {
				w.Segments = append(w.Segments, Segment{DCost: dC, DUtil: dU, At: now})
				if len(w.Segments) > maxSegments {
					w.Segments = w.Segments[len(w.Segments)-maxSegments:]
				}
			}
			w.Anchor = &Anchor{Util: o.Util, Cost: w.ProxyCost, At: now}
		}
	}
}

// Calibration quality levels.
const (
	QualityNone       = "calibrating"
	QualityBootstrap  = "bootstrap"
	QualityRatio      = "ratio"
	QualityCalibrated = "calibrated"
)

// DirectCapacity estimates the cost that fills 100% of this window from its own segments.
//
// Each segment is a lower bound on capacity, because usage outside the proxy only adds to the
// rise. So the estimate leans on the highest bounds, which come from segments with the least
// outside usage. It skips the top fifth to absorb timing noise at the edges. With fewer than 3
// segments the estimate is shown but not enforced.
func (w *Window) DirectCapacity() (float64, string) {
	if len(w.Segments) == 0 {
		return 0, QualityNone
	}
	ratios := make([]float64, 0, len(w.Segments))
	for _, s := range w.Segments {
		ratios = append(ratios, 100*s.DCost/s.DUtil)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(ratios)))
	best := ratios[len(ratios)/5]
	if len(w.Segments) >= 3 {
		return best, QualityCalibrated
	}
	return best, QualityBootstrap
}

// Trusted reports whether a capacity of this quality may be used to block requests.
func Trusted(quality string) bool { return quality == QualityCalibrated || quality == QualityRatio }

// scopeMatches reports whether a request (model + response headers' limit IDs) counts in this window.
func scopeMatches(scope, model string, headerLimits map[string]bool) bool {
	switch {
	case scope == "":
		return true
	case strings.HasPrefix(scope, "model:"):
		return strings.Contains(strings.ToLower(model), strings.TrimPrefix(scope, "model:"))
	case strings.HasPrefix(scope, "header:"):
		return headerLimits[strings.TrimPrefix(scope, "header:")]
	}
	return false
}

// windowIDForLength names a window by its length ("5h", "7d", "90m").
func windowIDForLength(sec int64) string {
	switch {
	case sec <= 0:
		return "window"
	case sec%86400 == 0:
		return itoa(sec/86400) + "d"
	case sec%3600 == 0:
		return itoa(sec/3600) + "h"
	default:
		return itoa((sec+59)/60) + "m"
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
