package pricing

import (
	"math"
	"testing"

	"go.inndy.tw/goxray/internal/model"
)

func TestLookupPrefersLongestPrefix(t *testing.T) {
	// The retired Opus 4 costs 3x Opus 4.8. A bare-prefix match would silently
	// overcharge every 4.5–4.8 turn, so this is a correctness guard, not a nicety.
	for _, id := range []string{"claude-opus-4-8", "claude-opus-4-6-20251101", "claude-opus-5"} {
		r, ok := lookup(id)
		if !ok {
			t.Fatalf("%s: no rate found", id)
		}
		if r.input != 5 || r.output != 25 {
			t.Errorf("%s: got input=%v output=%v, want 5/25", id, r.input, r.output)
		}
	}
}

// Models older than the 4.6 generation are intentionally unpriced rather than
// carrying a rate nobody has verified.
func TestRetiredModelsAreUnpriced(t *testing.T) {
	for _, id := range []string{
		"claude-opus-4-1-20250805",
		"claude-opus-4-20250514",
		"claude-3-5-sonnet-20241022",
		"claude-3-opus-20240229",
	} {
		if Known(id) {
			t.Errorf("Known(%q) = true; expected no rate on file", id)
		}
	}
}

func TestLookupStripsExtendedContextMarker(t *testing.T) {
	a, okA := lookup("claude-opus-5[1m]")
	b, okB := lookup("claude-opus-5")
	if !okA || !okB || a != b {
		t.Errorf("[1m] marker changed the rate: %+v vs %+v", a, b)
	}
}

func TestKnown(t *testing.T) {
	for _, id := range []string{"claude-opus-5", "claude-fable-5", "claude-sonnet-5"} {
		if !Known(id) {
			t.Errorf("Known(%q) = false", id)
		}
	}
	if Known("gpt-4") {
		t.Error("Known(gpt-4) = true")
	}
	if Known("") {
		t.Error("Known(empty) = true")
	}
}

func TestCalc(t *testing.T) {
	// 1M of each class on Opus 5: 5 + 25 + 0.50 + 6.25.
	u := model.Usage{
		InputTokens:       1_000_000,
		OutputTokens:      1_000_000,
		CacheReadTokens:   1_000_000,
		CacheCreateTokens: 1_000_000,
	}
	got := Calc(u, "claude-opus-5")
	want := 5 + 25 + 0.50 + 6.25
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("Calc = %v, want %v", got, want)
	}
}

// The split is what makes 1h writes billable at their real rate; without it the
// flat total is charged at the 5m rate and a 1h-heavy turn is underpriced.
func TestCalcBillsCacheWriteSplitByTTL(t *testing.T) {
	split := model.Usage{
		CacheCreateTokens: 1_000_000,
		CacheCreate5m:     400_000,
		CacheCreate1h:     600_000,
	}
	got := Calc(split, "claude-opus-5")
	want := 0.4*6.25 + 0.6*10
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("Calc = %v, want %v", got, want)
	}

	flat := model.Usage{CacheCreateTokens: 1_000_000}
	if flatCost := Calc(flat, "claude-opus-5"); flatCost >= got {
		t.Errorf("flat fallback %v should be cheaper than the 1h-heavy split %v", flatCost, got)
	}
}

// Captures taken before the split was plumbed through have only the flat total.
// They must still price, at the 5m rate, rather than falling to zero.
func TestCalcFallsBackToFlatCacheTotal(t *testing.T) {
	u := model.Usage{CacheCreateTokens: 1_000_000}
	if got, want := Calc(u, "claude-opus-5"), 6.25; math.Abs(got-want) > 1e-9 {
		t.Errorf("Calc = %v, want %v", got, want)
	}
}

func TestPricePrefersResolvedModelOverRequestAlias(t *testing.T) {
	res := model.ResMeta{
		Usage: model.Usage{OutputTokens: 1_000_000},
		Model: "claude-haiku-4-5-20251001",
	}
	if got, want := Price(res, "claude-opus-5"), 5.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("Price = %v, want the served model's %v", got, want)
	}

	res.Model = ""
	if got, want := Price(res, "claude-opus-5"), 25.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("Price = %v, want the request alias's %v", got, want)
	}
}

func TestPriceHalvesBatchTier(t *testing.T) {
	res := model.ResMeta{
		Usage:       model.Usage{OutputTokens: 1_000_000},
		Model:       "claude-opus-5",
		ServiceTier: "batch",
	}
	if got, want := Price(res, ""), 12.5; math.Abs(got-want) > 1e-9 {
		t.Errorf("Price = %v, want %v", got, want)
	}

	// An unrecognised or absent tier must not silently discount.
	for _, tier := range []string{"", "standard", "priority"} {
		res.ServiceTier = tier
		if got, want := Price(res, ""), 25.0; math.Abs(got-want) > 1e-9 {
			t.Errorf("tier %q: Price = %v, want %v", tier, got, want)
		}
	}
}

func TestCalcUnknownModelIsZeroNotAGuess(t *testing.T) {
	u := model.Usage{InputTokens: 1_000_000}
	if got := Calc(u, "some-other-vendor-model"); got != 0 {
		t.Errorf("Calc = %v, want 0 for an unpriced model", got)
	}
}

func TestCacheWrite1hCostsMoreThan5m(t *testing.T) {
	for id, r := range fallbackRates {
		if r.cacheWrite1h <= r.cacheWrite5m {
			t.Errorf("%s: 1h write %v should exceed 5m write %v", id, r.cacheWrite1h, r.cacheWrite5m)
		}
		if r.cacheRead >= r.input {
			t.Errorf("%s: cache read %v should be below input %v", id, r.cacheRead, r.input)
		}
	}
}

func TestOpenAIIdsDoNotShadowEachOther(t *testing.T) {
	// gpt-6-sol and gpt-6.1-sol share input and output rates but not the cached
	// rate; a loose prefix would bill one at the other's.
	a, _ := lookup("gpt-6.1-sol")
	b, _ := lookup("gpt-6-sol")
	if a.cacheRead != 0.10 || b.cacheRead != 0.20 {
		t.Fatalf("cached rates: 6.1-sol %v, 6-sol %v", a.cacheRead, b.cacheRead)
	}
	if Known("gpt-6") || Known("gpt-daybreak-blue-latest") || Known("codex-auto-review") {
		t.Error("an unpriced OpenAI id resolved to a rate")
	}
}

func TestOpenAILongContextBillsWholeRequest(t *testing.T) {
	short := model.Usage{InputTokens: 200_000, CacheReadTokens: 72_000, OutputTokens: 1_000}
	long := model.Usage{InputTokens: 200_000, CacheReadTokens: 72_001, OutputTokens: 1_000}
	// 200K*$2 + 72K*$0.10 + 1K*$10, then the same counts at $4/$0.20/$15.
	if got, want := Calc(short, "gpt-6.1-sol"), 0.4+0.0072+0.01; math.Abs(got-want) > 1e-9 {
		t.Errorf("short context: %v, want %v", got, want)
	}
	if got, want := Calc(long, "gpt-6.1-sol"), 0.8+0.0144002+0.015; math.Abs(got-want) > 1e-9 {
		t.Errorf("long context: %v, want %v", got, want)
	}
}

func TestOpenAITiers(t *testing.T) {
	u := model.Usage{InputTokens: 100_000} // short context: $0.20 at Standard
	for tier, want := range map[string]float64{"default": 0.2, "flex": 0.1, "priority": 0.4, "fast": 0.4, "ultrafast": 1.2} {
		if got := Price(model.ResMeta{Usage: u, Model: "gpt-6.1-sol", ServiceTier: tier}, ""); math.Abs(got-want) > 1e-9 {
			t.Errorf("%s: %v, want %v", tier, got, want)
		}
	}
	// Anthropic's priority stays unscaled.
	if got := Price(model.ResMeta{Usage: u, Model: "claude-opus-5", ServiceTier: "priority"}, ""); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("claude priority: %v, want 0.5", got)
	}
}

// The worked example from OpenAI's prompt-caching guide, on a model whose rates
// match it ($1 input): 1,000 plain, 4,000 cached, 3,000 written for 30 minutes
// and 4,000 for 12 hours.
func TestOpenAICacheWritesBillByLifetime(t *testing.T) {
	u := model.Usage{InputTokens: 1000, CacheReadTokens: 4000, CacheCreateTokens: 7000, CacheCreate5m: 3000, CacheCreate1h: 4000}
	// 1000×1 + 4000×0.05 + 3000×1.25 + 4000×2.5, per million.
	if got, want := Calc(u, "gpt-6.1-terra"), 0.01495; math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost %v, want %v", got, want)
	}
}
