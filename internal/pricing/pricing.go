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
// for an hour; OpenAI's two write TTLs (30m, 12h) occupy the same two slots.
//
// longAbove and long describe OpenAI's long-context tier: a request whose input
// exceeds longAbove tokens is billed entirely at the long rates, not just the
// part above the line.
type rate struct {
	input, output, cacheRead, cacheWrite5m, cacheWrite1h float64

	longAbove int
	long      *rate
}

// openai builds an OpenAI rate from its published short- and long-context
// rows, each {input, cached input, 30m cache write, 12h cache write, output}.
func openai(short, long [5]float64) rate {
	mk := func(r [5]float64) rate {
		return rate{input: r[0], cacheRead: r[1], cacheWrite5m: r[2], cacheWrite1h: r[3], output: r[4]}
	}
	l := mk(long)
	s := mk(short)
	s.longAbove, s.long = 272_000, &l
	return s
}

// fallbackRates keyed by a model-id prefix, longest match wins.
//
// Longest-match matters for correctness, not just precision: Opus 4.5 through
// 4.8 are priced at a third of the retired Opus 4, so they need their own
// entries or the bare "claude-opus-4" prefix would overcharge them 3x.
var fallbackRates = map[string]rate{
	// Opus 5 and Opus 4.5–4.8 share a tier.
	"claude-opus-5":   {input: 5, output: 25, cacheRead: 0.50, cacheWrite5m: 6.25, cacheWrite1h: 10},
	"claude-opus-4-8": {input: 5, output: 25, cacheRead: 0.50, cacheWrite5m: 6.25, cacheWrite1h: 10},
	"claude-opus-4-7": {input: 5, output: 25, cacheRead: 0.50, cacheWrite5m: 6.25, cacheWrite1h: 10},
	"claude-opus-4-6": {input: 5, output: 25, cacheRead: 0.50, cacheWrite5m: 6.25, cacheWrite1h: 10},
	"claude-opus-4-5": {input: 5, output: 25, cacheRead: 0.50, cacheWrite5m: 6.25, cacheWrite1h: 10},
	"claude-fable-5":  {input: 10, output: 50, cacheRead: 1, cacheWrite5m: 12.50, cacheWrite1h: 20},
	"claude-mythos-5": {input: 10, output: 50, cacheRead: 1, cacheWrite5m: 12.50, cacheWrite1h: 20},

	// Sonnet 5 is on introductory pricing through 2026-08-31; from 2026-09-01 it
	// moves to the Sonnet 4.6 tier {3, 15, 0.30, 3.75, 6}. Calc has no timestamp
	// to switch on, so update this entry on that date.
	"claude-sonnet-5":   {input: 2, output: 10, cacheRead: 0.20, cacheWrite5m: 2.50, cacheWrite1h: 4},
	"claude-sonnet-4-6": {input: 3, output: 15, cacheRead: 0.30, cacheWrite5m: 3.75, cacheWrite1h: 6},

	// Haiku 4.5 is the current Haiku; there is no 4.6 yet, so this is pinned to
	// 4-5 rather than a bare "claude-haiku-4" prefix that would also claim one.
	"claude-haiku-4-5": {input: 1, output: 5, cacheRead: 0.10, cacheWrite5m: 1.25, cacheWrite1h: 2},

	// OpenAI, Standard tier, from developers.openai.com/api/docs/pricing as of
	// 2026-10-27. Short context is <=272K input tokens. Each id is spelled out:
	// "gpt-6-sol" and "gpt-6.1-sol" differ by more than a suffix, and a bare
	// "gpt-6" would claim both. GPT-5.6 Sol's row is promotional "through at
	// least 2026-11-21" — recheck it then.
	"gpt-6-astra":   openai([5]float64{10, 1, 12.50, 25, 50}, [5]float64{20, 2, 25, 50, 75}),
	"gpt-6.1-sol":   openai([5]float64{2, 0.10, 2.50, 5, 10}, [5]float64{4, 0.20, 5, 10, 15}),
	"gpt-6.1-terra": openai([5]float64{1, 0.05, 1.25, 2.50, 5}, [5]float64{2, 0.10, 2.50, 5, 7.50}),
	"gpt-6-sol":     openai([5]float64{2, 0.20, 2.50, 5, 10}, [5]float64{4, 0.40, 5, 10, 15}),
	"gpt-6-luna":    openai([5]float64{0.10, 0.01, 0.125, 0.25, 0.50}, [5]float64{0.20, 0.02, 0.25, 0.50, 0.75}),
	"gpt-5.6-sol":   openai([5]float64{4, 0.40, 5, 10, 20}, [5]float64{8, 0.80, 10, 20, 30}),
	"gpt-5.6-terra": openai([5]float64{2, 0.20, 2.50, 5, 12}, [5]float64{4, 0.40, 5, 10, 18}),
	"gpt-5.6-luna":  openai([5]float64{0.20, 0.02, 0.25, 0.50, 1.20}, [5]float64{0.40, 0.04, 0.50, 1.00, 1.80}),
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

// Price returns the USD cost of a parsed response. It prefers the model id the
// API reported over the alias the request asked for, and scales by service tier.
// This is what callers should use; Calc is the bare arithmetic.
func Price(res model.ResMeta, reqModel string) float64 {
	id := res.Model
	if id == "" {
		id = reqModel
	}
	return Calc(res.Usage, id) * tierMultiplier(res.ServiceTier, id)
}

// tierMultiplier scales the rate by how the request was served. Batch is half
// price. Anthropic's priority tier is negotiated per-account, so it stays at 1
// rather than inventing a premium nobody has verified; OpenAI publishes its
// tiers, and on every model priced here Flex is half of Standard and Fast
// (formerly Priority) double.
func tierMultiplier(tier, modelID string) float64 {
	switch tier {
	case "batch":
		return 0.5
	case "flex":
		if isOpenAI(modelID) {
			return 0.5
		}
	case "priority", "fast":
		if isOpenAI(modelID) {
			return 2
		}
	case "ultrafast":
		if isOpenAI(modelID) {
			return 6
		}
	}
	return 1
}

func isOpenAI(modelID string) bool { return strings.HasPrefix(modelID, "gpt-") }

// Calc returns the USD cost of a usage record for the given model. Returns 0
// for an unknown model (never guesses a price it can't justify).
func Calc(u model.Usage, modelID string) float64 {
	r, ok := lookup(modelID)
	if !ok {
		return 0
	}
	if r.long != nil && u.InputTokens+u.CacheReadTokens+u.CacheCreateTokens > r.longAbove {
		r = *r.long
	}
	const perM = 1_000_000.0
	return float64(u.InputTokens)/perM*r.input +
		float64(u.OutputTokens)/perM*r.output +
		float64(u.CacheReadTokens)/perM*r.cacheRead +
		cacheWriteCost(u, r)
}

// cacheWriteCost bills the TTL split when the capture has it and falls back to
// the flat total at the 5m rate when it does not. The fallback underprices 1h
// writes by 60%, which is why `goxray migrate-usage` exists — it backfills the
// split from the recorded blobs. Keyed on which fields are populated rather
// than on a capture version, so a partial capture still prices correctly.
func cacheWriteCost(u model.Usage, r rate) float64 {
	const perM = 1_000_000.0
	if u.CacheCreate5m > 0 || u.CacheCreate1h > 0 {
		return float64(u.CacheCreate5m)/perM*r.cacheWrite5m +
			float64(u.CacheCreate1h)/perM*r.cacheWrite1h
	}
	return float64(u.CacheCreateTokens) / perM * r.cacheWrite5m
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
