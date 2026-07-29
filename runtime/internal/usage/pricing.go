package usage

import "strings"

const pricingRevision = "49ca04d8c3ddea336237ce6f3082dbc26d19e944"

var provenance = PricingProvenance{
	Source:   "ccusage pricing semantics",
	Revision: "v20.0.17",
	URL:      "https://github.com/ryoppippi/ccusage/blob/v20.0.17/rust/crates/ccusage/src/pricing.rs",
	Note:     "Matches ccusage v20.0.17 with LiteLLM revision " + pricingRevision[:8] + "; costs are estimates and missing models remain visibly unpriced.",
}

// Rates are USD per million tokens. The explicit table is intentionally small:
// silently guessing for a new model is worse than reporting missing pricing.
type rates struct {
	input, output, cacheCreate, cacheRead                 float64
	longInput, longOutput, longCacheCreate, longCacheRead float64
	longContextThreshold                                  int64
	fastMultiplier                                        float64
}

func standardRates(input, output, cacheCreate, cacheRead float64) rates {
	return rates{input: input, output: output, cacheCreate: cacheCreate, cacheRead: cacheRead}
}

var modelRates = map[string]rates{
	"claude-fable-5":             standardRates(10, 50, 12.5, 1),
	"claude-haiku-4-5":           standardRates(1, 5, 1.25, .1),
	"claude-haiku-4-5-20251001":  standardRates(1, 5, 1.25, .1),
	"claude-opus-4-5":            standardRates(5, 25, 6.25, .5),
	"claude-opus-4-6":            standardRates(5, 25, 6.25, .5),
	"claude-opus-4-7":            standardRates(5, 25, 6.25, .5),
	"claude-opus-4-8":            standardRates(5, 25, 6.25, .5),
	"claude-sonnet-4-5":          standardRates(3, 15, 3.75, .3),
	"claude-sonnet-4-5-20250929": standardRates(3, 15, 3.75, .3),
	"claude-sonnet-4-6":          standardRates(3, 15, 3.75, .3),
	"gpt-5":                      standardRates(1.25, 10, 1.25, .125),
	"gpt-5-codex":                standardRates(1.25, 10, 1.25, .125),
	"gpt-5.1":                    standardRates(1.25, 10, 1.25, .125),
	"gpt-5.1-codex":              standardRates(1.25, 10, 1.25, .125),
	"gpt-5.2":                    standardRates(1.75, 14, 1.75, .175),
	"gpt-5.2-codex":              standardRates(1.75, 14, 1.75, .175),
	"gpt-5.3-codex":              {input: 1.75, output: 14, cacheCreate: 1.75, cacheRead: .175, fastMultiplier: 2},
	"gpt-5.4":                    {input: 2.5, output: 15, cacheCreate: 2.5, cacheRead: .25, longInput: 5, longOutput: 22.5, longCacheCreate: 5, longCacheRead: .5, longContextThreshold: 272_000, fastMultiplier: 2},
	"gpt-5.4-mini":               {.75, 4.5, .75, .075, 0, 0, 0, 0, 0, 1},
	"gpt-5.4-nano":               {.2, 1.25, .2, .02, 0, 0, 0, 0, 0, 1},
	"gpt-5.5":                    {input: 5, output: 30, cacheCreate: 5, cacheRead: .5, longInput: 10, longOutput: 45, longCacheCreate: 10, longCacheRead: 1, longContextThreshold: 272_000, fastMultiplier: 2.5},
	"gpt-5.6-sol":                {input: 5, output: 30, cacheCreate: 6.25, cacheRead: .5, longInput: 10, longOutput: 45, longCacheCreate: 12.5, longCacheRead: 1, longContextThreshold: 272_000, fastMultiplier: 2},
	"gpt-5.6-terra":              {input: 2.5, output: 15, cacheCreate: 3.125, cacheRead: .25, longInput: 5, longOutput: 22.5, longCacheCreate: 6.25, longCacheRead: .5, longContextThreshold: 272_000, fastMultiplier: 2},
	"gpt-5.6-luna":               {input: 1, output: 6, cacheCreate: 1.25, cacheRead: .1, longInput: 2, longOutput: 9, longCacheCreate: 2.5, longCacheRead: .2, longContextThreshold: 272_000, fastMultiplier: 2},
}

func price(model string, tokens Tokens, fast bool) (float64, bool) {
	model = strings.TrimSpace(strings.ToLower(model))
	selected, ok := modelRates[model]
	if !ok {
		return 0, false
	}
	if selected.longContextThreshold > 0 && tokens.Input+tokens.CacheRead > selected.longContextThreshold {
		selected.input = selected.longInput
		selected.output = selected.longOutput
		selected.cacheCreate = selected.longCacheCreate
		selected.cacheRead = selected.longCacheRead
	}
	cost := float64(tokens.Input)*selected.input +
		float64(tokens.Output)*selected.output +
		float64(tokens.CacheCreation)*selected.cacheCreate +
		float64(tokens.CacheRead)*selected.cacheRead
	if fast {
		multiplier := selected.fastMultiplier
		if multiplier == 0 {
			multiplier = 2
		}
		cost *= multiplier
	}
	return cost / 1_000_000, true
}
