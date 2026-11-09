package guard

import "strings"

// Tokens is the token accounting of one request, in CPA's normalized fields.
type Tokens struct {
	Input       int64 // Claude: uncached input; OpenAI/Codex: all input incl. cached
	Output      int64 // includes reasoning/thinking
	Cached      int64 // OpenAI/Codex: cached subset of Input
	CacheRead   int64 // Claude: cache reads (not part of Input)
	CacheCreate int64 // Claude: cache writes (not part of Input)
	// CacheCreate1h is the part of CacheCreate written with a 1-hour TTL, billed at a higher
	// rate. Only known when the host reports the TTL split; otherwise all writes count as 5-minute.
	CacheCreate1h int64
}

// defaultPricing is API list price in USD per million tokens (input, output, cache read,
// cache write), used only as relative weights. First match wins, so specific rules come first.
var defaultPricing = []PriceRule{
	{Provider: "claude", Match: "fable", Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	{Provider: "claude", Match: "mythos", Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	{Provider: "claude", Match: "opus-5-5", Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5},
	{Provider: "claude", Match: "opus", Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	{Provider: "claude", Match: "sonnet-5", Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
	{Provider: "claude", Match: "sonnet", Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	{Provider: "claude", Match: "haiku-5", Input: 0.1, Output: 0.5, CacheRead: 0.01, CacheWrite: 0.125},
	{Provider: "claude", Match: "haiku", Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
	{Provider: "claude", Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5},
	{Provider: "codex", Match: "mini", Input: 0.25, Output: 2, CacheRead: 0.025},
	{Provider: "codex", Input: 1.25, Output: 10, CacheRead: 0.125},
	{Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
}

// cacheWrite1h is the 1-hour cache write price: as configured, else twice the input price
// (Anthropic bills 5-minute writes at 1.25x input and 1-hour writes at 2x).
func (p PriceRule) cacheWrite1h() float64 {
	if p.CacheWrite1h > 0 {
		return p.CacheWrite1h
	}
	return 2 * p.Input
}

func priceFor(rules []PriceRule, provider, model string) PriceRule {
	model = strings.ToLower(model)
	for _, set := range [][]PriceRule{rules, defaultPricing} {
		for _, r := range set {
			if r.Provider != "" && r.Provider != provider {
				continue
			}
			if r.Match != "" && !strings.Contains(model, r.Match) {
				continue
			}
			return r
		}
	}
	return defaultPricing[len(defaultPricing)-1]
}

// Billing types, in the order Breakdown and the costs report list them.
const (
	TypeInput = iota // uncached input
	TypeOutput
	TypeCacheRead
	TypeCacheWrite   // 5-minute cache writes (all writes when the host doesn't report the TTL split)
	TypeCacheWrite1h // 1-hour cache writes
	NumTypes
)

// TypeNames names the billing types for the costs report.
var TypeNames = [NumTypes]string{"input", "output", "cache_read", "cache_write", "cache_write_1h"}

// Breakdown is a request's tokens and cost per billing type, and what it would have cost with no
// prompt caching (every prompt token billed as plain input).
type Breakdown struct {
	Tokens  [NumTypes]int64
	Cost    [NumTypes]float64
	NoCache float64
}

// Total is the sum of the per-type costs.
func (b Breakdown) Total() float64 {
	var s float64
	for _, c := range b.Cost {
		s += c
	}
	return s
}

// Split prices a request per billing type.
func Split(rules []PriceRule, provider, model string, t Tokens) Breakdown {
	p := priceFor(rules, provider, model)
	perTok := func(price float64, n int64) float64 { return price * float64(n) / 1e6 }
	var b Breakdown
	if provider == "claude" {
		long := min(max(t.CacheCreate1h, 0), t.CacheCreate)
		b.Tokens = [NumTypes]int64{t.Input, t.Output, t.CacheRead, t.CacheCreate - long, long}
		b.Cost = [NumTypes]float64{perTok(p.Input, t.Input), perTok(p.Output, t.Output), perTok(p.CacheRead, t.CacheRead),
			perTok(p.CacheWrite, t.CacheCreate-long), perTok(p.cacheWrite1h(), long)}
	} else {
		cached := t.Cached
		if cached == 0 {
			cached = t.CacheRead
		}
		uncached := max(t.Input-cached, 0)
		b.Tokens = [NumTypes]int64{uncached, t.Output, cached, t.CacheCreate, 0}
		b.Cost = [NumTypes]float64{perTok(p.Input, uncached), perTok(p.Output, t.Output), perTok(p.CacheRead, cached),
			perTok(p.CacheWrite, t.CacheCreate), 0}
	}
	prompt := b.Tokens[TypeInput] + b.Tokens[TypeCacheRead] + b.Tokens[TypeCacheWrite] + b.Tokens[TypeCacheWrite1h]
	b.NoCache = perTok(p.Input, prompt) + b.Cost[TypeOutput]
	return b
}

// Cost returns the API-equivalent USD cost of a request, used as the unit of "work".
func Cost(rules []PriceRule, provider, model string, t Tokens) float64 {
	return Split(rules, provider, model, t).Total()
}
