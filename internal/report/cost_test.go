package report

import (
	"math"
	"testing"

	"github.com/AngelVRodC/tare/internal/transcript"
)

// TestCacheReadWeightPerModel pins the per-model cache-read weight (defect 1).
// Fable 5.1 and Mythos 5.1 price cache reads at 0.025x base input, a quarter
// of the Sonnet-class 0.1x — hardcoding 0.1x for every model over-weights
// their cache reads 4x. The match is tolerant: a family marker anywhere in the
// id, version suffixes included. Every other id — empty, "<synthetic>",
// unknown — falls back to the default, fail-closed, which is the current
// behaviour for anything this build does not recognize.
func TestCacheReadWeightPerModel(t *testing.T) {
	cases := []struct {
		model string
		want  float64
	}{
		{"claude-fable-5-1", 25.0},
		{"claude-fable-5-1-20260901", 25.0},
		{"claude-mythos-5-1", 25.0},
		{"claude-mythos-5-1-preview", 25.0},
		{"", 100.0},
		{"<synthetic>", 100.0},
		{"claude-opus-4-6", 100.0},
		{"claude-sonnet-4-5", 100.0},
		{"claude-fable-4-0", 100.0}, // wrong family version — no match
	}
	for _, tc := range cases {
		got := responseWeight(&transcript.Usage{Model: tc.model, CacheRead: 1000})
		if got != tc.want {
			t.Errorf("responseWeight(model %q, 1000 cache-read) = %v, want %v",
				tc.model, got, tc.want)
		}
	}
}

// TestFableWeightsMatchBilling validates the 0.025x ratio the same way
// TestWeightsMatchBilling validates the five constants: against billing that
// assumes it. Fable-class prices per million tokens are $5 input, $25 output,
// $0.125 cache read, $6.25 5m cache write, $10 1h cache write — ratios of
// 1 : 5 : 0.025 : 1.25 : 2. If the cache-read weight were still the hardcoded
// 0.1x, the two sessions' implied dollars-per-weight-unit would diverge by 17%.
func TestFableWeightsMatchBilling(t *testing.T) {
	const perMTokInput, perMTokOutput = 5.0, 25.0
	const perMTokCacheRead, perMTok5m, perMTok1h = 0.125, 6.25, 10.00
	const model = "claude-fable-5-1"

	// Input-only.
	a := tokens{in: 1_000_000}
	costA := perMTokInput
	// Output-, read- and write-heavy: nothing in common with a.
	b := tokens{out: 200_000, read: 5_000_000, c5m: 400_000, c1h: 100_000}
	costB := 200_000*perMTokOutput/1e6 +
		5_000_000*perMTokCacheRead/1e6 +
		400_000*perMTok5m/1e6 +
		100_000*perMTok1h/1e6

	env := attributeCorpus(t,
		usageLine(t, "sA", "msg_a", model, a, nil),
		costLine(t, "sA", costA, map[string]transcript.ModelUsage{model: {
			Input: a.in, Output: a.out, CacheRead: a.read,
			CacheCreate: a.c5m + a.c1h, CostUSD: costA,
		}}),
		usageLine(t, "sB", "msg_b", model, b, nil),
		costLine(t, "sB", costB, map[string]transcript.ModelUsage{model: {
			Input: b.in, Output: b.out, CacheRead: b.read,
			CacheCreate: b.c5m + b.c1h, CostUSD: costB,
		}}),
	)

	for _, s := range []string{"sA", "sB"} {
		if got := mustFloat(t, env, "coverage_ratio", "cost_coverage", s+"/"+model); got != 1 {
			t.Fatalf("session %s coverage = %v, want exactly 1", s, got)
		}
	}

	weigh := func(tk tokens) float64 {
		return responseWeight(&transcript.Usage{
			Model: model, Input: tk.in, Output: tk.out, CacheRead: tk.read,
			CacheCreate5m: tk.c5m, CacheCreate1h: tk.c1h,
		})
	}
	impliedA := costA / weigh(a)
	impliedB := costB / weigh(b)
	drift := math.Abs(impliedA-impliedB) / impliedA
	if drift > 0.02 {
		t.Errorf("implied price per weight unit differs by %.2f%% (%g vs %g) — the Fable cache-read weight is wrong",
			100*drift, impliedA, impliedB)
	}
}

// TestFableAllocationPinnedToBill pins defect 1's invariant: the per-model
// weight moves only the split between rows inside a session-model — the
// measured bill is still the ceiling every allocated row sums to, within one
// micro per row of rounding.
func TestFableAllocationPinnedToBill(t *testing.T) {
	const model = "claude-fable-5-1-20260901" // version-suffixed: must still match
	const bill = 7.77
	first := tokens{in: 900, out: 40, read: 120_000, c5m: 3_000}
	second := tokens{in: 12, out: 4_400, read: 8_000, c1h: 50_000}
	env := attributeCorpus(t,
		usageLine(t, "s1", "msg_1", model, first, map[string]string{"attributionSkill": "alpha"}),
		usageLine(t, "s1", "msg_2", model, second, map[string]string{"attributionSkill": "beta"}),
		costLine(t, "s1", bill, map[string]transcript.ModelUsage{model: {
			Input:       first.in + second.in,
			Output:      first.out + second.out,
			CacheRead:   first.read + second.read,
			CacheCreate: first.c5m + first.c1h + second.c5m + second.c1h,
			CostUSD:     bill,
		}}),
	)

	billed := int64(math.Round(bill * 1e6))
	var sum, rows int64
	for _, m := range env.Metrics {
		if m.Name == "cost_usd" && m.Dimension == "attribution_skill" {
			sum += m.Value.(int64)
			rows++
		}
	}
	if rows != 2 {
		t.Fatalf("allocated across %d skill rows, want 2", rows)
	}
	if drift := sum - billed; drift < -rows || drift > rows {
		t.Errorf("allocated %d micro, want the measured bill %d micro within %d (per-row rounding)", sum, billed, rows)
	}
}
