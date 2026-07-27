// Package pricing computes USD cost from token usage using a static fallback
// table. ccxray fetches live LiteLLM prices with a 24h cache; that is deferred —
// the fallback rates here are the same shape and can be refreshed later without
// touching callers.
package pricing

import (
	"strings"

	"go.inndy.tw/goxray/internal/model"
)

// rate is USD per 1M tokens for each usage class. cacheWrite5m and cacheWrite1h
// are separate because Anthropic charges 1.25x input for a 5-minute TTL and 2x
// for an hour.
type rate struct {
	input, output, cacheRead, cacheWrite5m, cacheWrite1h float64
}

// fallbackRates keyed by a model-id prefix, longest match wins.
//
// Longest-match matters for correctness, not just precision: Opus 4.5 through
// 4.8 are priced at a third of the retired Opus 4, so they need their own
// entries or the bare "claude-opus-4" prefix would overcharge them 3x.
var fallbackRates = map[string]rate{
	// Opus 5 and Opus 4.5–4.8 share a tier.
	"claude-opus-5":   {5, 25, 0.50, 6.25, 10},
	"claude-opus-4-8": {5, 25, 0.50, 6.25, 10},
	"claude-opus-4-7": {5, 25, 0.50, 6.25, 10},
	"claude-opus-4-6": {5, 25, 0.50, 6.25, 10},
	"claude-opus-4-5": {5, 25, 0.50, 6.25, 10},
	"claude-fable-5":  {10, 50, 1, 12.50, 20},
	"claude-mythos-5": {10, 50, 1, 12.50, 20},

	// Sonnet 5 is on introductory pricing through 2026-08-31; from 2026-09-01 it
	// moves to the Sonnet 4.6 tier {3, 15, 0.30, 3.75, 6}. Calc has no timestamp
	// to switch on, so update this entry on that date.
	"claude-sonnet-5":   {2, 10, 0.20, 2.50, 4},
	"claude-sonnet-4-6": {3, 15, 0.30, 3.75, 6},

	// Haiku 4.5 is the current Haiku; there is no 4.6 yet, so this is pinned to
	// 4-5 rather than a bare "claude-haiku-4" prefix that would also claim one.
	"claude-haiku-4-5": {1, 5, 0.10, 1.25, 2},
}

// Deliberately absent: Opus 4.1 and older, Sonnet 4.5 and older, and the
// claude-3 family. Nothing in use here predates the 4.6 generation, and a stale
// rate that renders as a confident dollar figure is worse than no rate — an
// unpriced model yields 0 plus an explicit "no price on file" warning.

// Known reports whether a price is on file for the model. Callers use it to
// distinguish "this turn genuinely cost about nothing" from "we have no rate for
// this model", which otherwise both render as $0.00.
func Known(modelID string) bool {
	_, ok := lookup(modelID)
	return ok
}

// Calc returns the USD cost of a usage record for the given model. Returns 0
// for an unknown model (never guesses a price it can't justify).
func Calc(u model.Usage, modelID string) float64 {
	r, ok := lookup(modelID)
	if !ok {
		return 0
	}
	const perM = 1_000_000.0
	// Usage carries one flat cache-creation figure, so the 5m rate is applied.
	// Claude Code does request 1h TTLs, which this therefore underprices; billing
	// the two apart needs usage.cache_creation.{ephemeral_5m,ephemeral_1h} to be
	// captured first.
	return float64(u.InputTokens)/perM*r.input +
		float64(u.OutputTokens)/perM*r.output +
		float64(u.CacheReadTokens)/perM*r.cacheRead +
		float64(u.CacheCreateTokens)/perM*r.cacheWrite5m
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
