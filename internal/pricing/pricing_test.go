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
