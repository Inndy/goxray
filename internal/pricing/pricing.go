// Package pricing computes USD cost from token usage using a static fallback
// table. ccxray fetches live LiteLLM prices with a 24h cache; that is deferred —
// the fallback rates here are the same shape and can be refreshed later without
// touching callers.
package pricing

import (
	"strings"

	"go.inndy.tw/goxray/internal/model"
)

// rate is USD per 1M tokens for each usage class.
type rate struct {
	input, output, cacheRead, cacheWrite float64
}

// fallbackRates keyed by a model-id prefix (longest match wins). Cache-read is
// ~0.1x input; 5m cache-write is ~1.25x input (Anthropic pricing shape).
var fallbackRates = map[string]rate{
	"claude-opus-4":     {15, 75, 1.50, 18.75},
	"claude-sonnet-4":   {3, 15, 0.30, 3.75},
	"claude-sonnet-5":   {3, 15, 0.30, 3.75},
	"claude-fable-5":    {3, 15, 0.30, 3.75},
	"claude-haiku-4":    {1, 5, 0.10, 1.25},
	"claude-3-5-sonnet": {3, 15, 0.30, 3.75},
	"claude-3-5-haiku":  {0.80, 4, 0.08, 1},
	"claude-3-opus":     {15, 75, 1.50, 18.75},
	"claude-3-haiku":    {0.25, 1.25, 0.03, 0.30},
}

// Calc returns the USD cost of a usage record for the given model. Returns 0
// for an unknown model (never guesses a price it can't justify).
func Calc(u model.Usage, modelID string) float64 {
	r, ok := lookup(modelID)
	if !ok {
		return 0
	}
	const perM = 1_000_000.0
	return float64(u.InputTokens)/perM*r.input +
		float64(u.OutputTokens)/perM*r.output +
		float64(u.CacheReadTokens)/perM*r.cacheRead +
		float64(u.CacheCreateTokens)/perM*r.cacheWrite
}

func lookup(modelID string) (rate, bool) {
	stripped := modelID
	if i := strings.IndexByte(stripped, '['); i >= 0 { // drop "[1m]" suffix
		stripped = stripped[:i]
	}
	var best string
	for k := range fallbackRates {
		if strings.HasPrefix(stripped, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return rate{}, false
	}
	return fallbackRates[best], true
}
