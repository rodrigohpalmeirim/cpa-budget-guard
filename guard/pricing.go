package guard

import "strings"

// Tokens is the token accounting of one request, in CPA's normalized fields.
type Tokens struct {
	Input       int64 // Claude: uncached input; OpenAI/Codex: all input incl. cached
	Output      int64 // includes reasoning/thinking
	Cached      int64 // OpenAI/Codex: cached subset of Input
	CacheRead   int64 // Claude: cache reads (not part of Input)
	CacheCreate int64 // Claude: cache writes (not part of Input)
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

// Cost returns the API-equivalent USD cost of a request, used as the unit of "work".
func Cost(rules []PriceRule, provider, model string, t Tokens) float64 {
	p := priceFor(rules, provider, model)
	perTok := func(price float64, n int64) float64 { return price * float64(n) / 1e6 }
	if provider == "claude" {
		return perTok(p.Input, t.Input) + perTok(p.Output, t.Output) +
			perTok(p.CacheRead, t.CacheRead) + perTok(p.CacheWrite, t.CacheCreate)
	}
	cached := t.Cached
	if cached == 0 {
		cached = t.CacheRead
	}
	uncached := t.Input - cached
	if uncached < 0 {
		uncached = 0
	}
	return perTok(p.Input, uncached) + perTok(p.CacheRead, cached) + perTok(p.Output, t.Output) +
		perTok(p.CacheWrite, t.CacheCreate)
}
