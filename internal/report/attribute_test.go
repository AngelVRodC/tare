package report

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AngelVRodC/tare/internal/transcript"
)

// tokens is one response's billed counts, spelled out so a fixture reads as
// arithmetic rather than as JSON.
type tokens struct {
	in, out, read, c5m, c1h int64
}

// usageLine writes one assistant response, optionally carrying attribution.
func usageLine(t *testing.T, session, id, model string, tk tokens, attrs map[string]string) string {
	t.Helper()
	line := map[string]any{
		"type":      "assistant",
		"sessionId": session,
		"timestamp": "2026-09-01T10:00:00.000Z",
		"message": map[string]any{
			"id":    id,
			"model": model,
			"usage": map[string]any{
				"input_tokens":                tk.in,
				"output_tokens":               tk.out,
				"cache_read_input_tokens":     tk.read,
				"cache_creation_input_tokens": tk.c5m + tk.c1h,
				"cache_creation": map[string]any{
					"ephemeral_5m_input_tokens": tk.c5m,
					"ephemeral_1h_input_tokens": tk.c1h,
				},
			},
		},
	}
	for k, v := range attrs {
		line[k] = v
	}
	return marshal(t, line)
}

// costLine writes the once-per-session billing record.
func costLine(t *testing.T, session string, total float64, models map[string]transcript.ModelUsage) string {
	t.Helper()
	return marshal(t, map[string]any{
		"type":         "cost-state",
		"sessionId":    session,
		"totalCostUSD": total,
		"modelUsage":   models,
	})
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// attributeCorpus writes one transcript and returns the envelope over it.
func attributeCorpus(t *testing.T, lines ...string) Envelope {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := AttributeEnvelope(dir, "test")
	if err != nil {
		t.Fatalf("AttributeEnvelope: %v", err)
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return env
}

// findMetric returns one row, or nil when it was never emitted. Absence is a
// result here: a session with no bill must have no cost row at all.
func findMetric(env Envelope, name, dimension, key string) *Metric {
	for i, m := range env.Metrics {
		if m.Name == name && m.Dimension == dimension && m.Key == key {
			return &env.Metrics[i]
		}
	}
	return nil
}

func mustMetric(t *testing.T, env Envelope, name, dimension, key string) Metric {
	t.Helper()
	m := findMetric(env, name, dimension, key)
	if m == nil {
		t.Fatalf("metric %s/%s/%s was never emitted", name, dimension, key)
	}
	return *m
}

func mustFloat(t *testing.T, env Envelope, name, dimension, key string) float64 {
	t.Helper()
	v, ok := mustMetric(t, env, name, dimension, key).Value.(float64)
	if !ok {
		t.Fatalf("metric %s/%s/%s is not a float", name, dimension, key)
	}
	return v
}

// TestRebillMultiplier is the phase gate on the differentiator. Cache reads
// are prior context charged again; the multiplier is how many times over. It
// is measured, not derived — the harness records both sides.
func TestRebillMultiplier(t *testing.T) {
	// Fresh = 100 input + 400 5m + 500 1h = 1,000. Rebilled = 23,600.
	env := attributeCorpus(t,
		usageLine(t, "s1", "msg_1", "claude-opus-5",
			tokens{in: 100, out: 50, read: 23600, c5m: 400, c1h: 500},
			map[string]string{"attributionSkill": "desplega:phase-running"}),
		// The same response written back: it must not move the multiplier.
		usageLine(t, "s1", "msg_1", "claude-opus-5",
			tokens{in: 100, out: 50, read: 23600, c5m: 400, c1h: 500},
			map[string]string{"attributionSkill": "desplega:phase-running"}),
	)

	if got := mustMetric(t, env, "fresh_tokens", "corpus", "").Value; got != int64(1000) {
		t.Errorf("fresh_tokens = %v, want 1000", got)
	}
	if got := mustMetric(t, env, "rebilled_tokens", "corpus", "").Value; got != int64(23600) {
		t.Errorf("rebilled_tokens = %v, want 23600", got)
	}
	for _, where := range []struct{ dimension, key string }{
		{"corpus", ""},
		{"attribution_skill", "desplega:phase-running"},
		{"session", "s1"},
	} {
		if got := mustFloat(t, env, "rebill_multiplier", where.dimension, where.key); got != 23.6 {
			t.Errorf("%s/%s multiplier = %v, want 23.6", where.dimension, where.key, got)
		}
	}

	// A dimension no response claimed is a row of its own, never redistributed.
	if got := mustMetric(t, env, "responses", "attribution_plugin", unattributedKey).Value; got != int64(1) {
		t.Errorf("unattributed responses = %v, want 1", got)
	}
}

// TestWeightsMatchBilling validates the five ratios against real billing.
//
// Anthropic's published Sonnet-class prices per million tokens are $3 input,
// $15 output, $0.30 cache read, $3.75 5m cache write, $6.00 1h cache write —
// ratios of 1 : 5 : 0.1 : 1.25 : 2, which are the constants in cost.go. Two
// sessions of the same model with deliberately opposite token mixes are billed
// at those prices here. If a ratio is wrong, the dollars-per-weight-unit each
// session implies diverges; correct ratios make them agree.
func TestWeightsMatchBilling(t *testing.T) {
	const perMTokInput, perMTokOutput = 3.0, 15.0
	const perMTokCacheRead, perMTok5m, perMTok1h = 0.30, 3.75, 6.00
	const model = "claude-sonnet-4-5"

	// Input-only.
	a := tokens{in: 1_000_000}
	costA := perMTokInput
	// Output-, read- and write-heavy: nothing in common with a.
	b := tokens{out: 200_000, read: 5_000_000, c5m: 400_000, c1h: 100_000}
	costB := 200_000*perMTokOutput/1e6 +
		5_000_000*perMTokCacheRead/1e6 +
		400_000*perMTok5m/1e6 +
		100_000*perMTok1h/1e6

	usageOf := func(tk tokens, cost float64) transcript.ModelUsage {
		return transcript.ModelUsage{
			Input: tk.in, Output: tk.out, CacheRead: tk.read,
			CacheCreate: tk.c5m + tk.c1h, CostUSD: cost,
		}
	}
	env := attributeCorpus(t,
		usageLine(t, "sA", "msg_a", model, a, nil),
		costLine(t, "sA", costA, map[string]transcript.ModelUsage{model: usageOf(a, costA)}),
		usageLine(t, "sB", "msg_b", model, b, nil),
		costLine(t, "sB", costB, map[string]transcript.ModelUsage{model: usageOf(b, costB)}),
	)

	// Both sessions must reconcile exactly, or the comparison below compares
	// two different things.
	for _, s := range []string{"sA", "sB"} {
		if got := mustFloat(t, env, "coverage_ratio", "cost_coverage", s+"/"+model); got != 1 {
			t.Fatalf("session %s coverage = %v, want exactly 1", s, got)
		}
	}

	// Weigh the transcript side, not the cost-state record: only the
	// transcript splits the two cache-creation tiers, and session b uses both.
	weigh := func(tk tokens) float64 {
		return responseWeight(&transcript.Usage{
			Input: tk.in, Output: tk.out, CacheRead: tk.read,
			CacheCreate5m: tk.c5m, CacheCreate1h: tk.c1h,
		})
	}
	impliedA := costA / weigh(a)
	impliedB := costB / weigh(b)
	drift := math.Abs(impliedA-impliedB) / impliedA
	if drift > 0.02 {
		t.Errorf("implied price per weight unit differs by %.2f%% (%g vs %g) — a weight ratio is wrong",
			100*drift, impliedA, impliedB)
	}
}

// TestAllocationConserves pins the property that makes the estimate safe: a
// wrong ratio moves dollars between rows and never changes the total, because
// the total is the measured bill.
func TestAllocationConserves(t *testing.T) {
	const model = "claude-opus-5"
	const bill = 12.34
	first := tokens{in: 900, out: 40, read: 120_000, c5m: 3_000}
	second := tokens{in: 12, out: 4_400, read: 8_000, c1h: 50_000}
	total := transcript.ModelUsage{
		Input:       first.in + second.in,
		Output:      first.out + second.out,
		CacheRead:   first.read + second.read,
		CacheCreate: first.c5m + first.c1h + second.c5m + second.c1h,
		CostUSD:     bill,
	}
	env := attributeCorpus(t,
		usageLine(t, "s1", "msg_1", model, first, map[string]string{"attributionSkill": "alpha"}),
		usageLine(t, "s1", "msg_2", model, second, map[string]string{"attributionSkill": "beta"}),
		costLine(t, "s1", bill, map[string]transcript.ModelUsage{model: total}),
	)

	var sum float64
	var rows int
	for _, m := range env.Metrics {
		if m.Name == "cost_usd" && m.Dimension == "attribution_skill" {
			sum += m.Value.(float64)
			rows++
		}
	}
	if rows != 2 {
		t.Fatalf("allocated across %d skill rows, want 2", rows)
	}
	if math.Abs(sum-bill) > 1e-9 {
		t.Errorf("allocated %.12f, want the measured bill %.12f", sum, bill)
	}
	// The split has to be a split, not the bill twice.
	alpha := mustMetric(t, env, "cost_usd", "attribution_skill", "alpha").Value.(float64)
	if alpha <= 0 || alpha >= bill {
		t.Errorf("alpha allocated %v, want a share strictly inside (0, %v)", alpha, bill)
	}
}

// TestNoMeasuredDollars is the derivation contract on money. Not one dollar
// figure is measured — not even a single-tool turn, because the charge carries
// the re-sent prefix and the output tokens, and neither belongs to any tool.
func TestNoMeasuredDollars(t *testing.T) {
	const model = "claude-opus-5"
	tk := tokens{in: 100, out: 20, read: 5_000, c5m: 800}
	env := attributeCorpus(t,
		usageLine(t, "s1", "msg_1", model, tk, map[string]string{
			"attributionSkill":     "alpha",
			"attributionPlugin":    "desplega",
			"attributionMcpServer": "context7",
		}),
		costLine(t, "s1", 1.5, map[string]transcript.ModelUsage{model: {
			Input: tk.in, Output: tk.out, CacheRead: tk.read, CacheCreate: tk.c5m, CostUSD: 1.5,
		}}),
	)

	var dollarRows int
	for i, m := range env.Metrics {
		if m.Unit != "usd" {
			continue
		}
		dollarRows++
		if m.Derivation != Estimated {
			t.Errorf("metric %d (%s/%s/%s) is a dollar row tagged %q", i, m.Name, m.Dimension, m.Key, m.Derivation)
		}
		if m.Method == nil || *m.Method == "" {
			t.Errorf("metric %d (%s/%s/%s) is a dollar row with no method named", i, m.Name, m.Dimension, m.Key)
		}
	}
	if dollarRows == 0 {
		t.Fatal("no dollar rows emitted — the contract was not exercised")
	}
	if got := mustMetric(t, env, "cost_usd", "attribution_plugin", "desplega"); got.Method == nil ||
		*got.Method != AllocationMethod {
		t.Errorf("allocated row names method %v, want %q", got.Method, AllocationMethod)
	}
}

// TestMissingCostState pins the honest half of the coverage rule: most
// sessions have tokens and no dollars, and those must say unavailable rather
// than report zero.
func TestMissingCostState(t *testing.T) {
	const model = "claude-opus-5"
	tk := tokens{in: 50, out: 10, read: 900}
	env := attributeCorpus(t,
		usageLine(t, "billed", "msg_1", model, tk, nil),
		costLine(t, "billed", 0.42, map[string]transcript.ModelUsage{model: {
			Input: tk.in, Output: tk.out, CacheRead: tk.read, CostUSD: 0.42,
		}}),
		usageLine(t, "unbilled", "msg_2", model, tk, nil),
	)

	if m := findMetric(env, "cost_usd", "session", "unbilled"); m != nil {
		t.Errorf("session with no cost-state emitted cost_usd = %v, want no row at all", m.Value)
	}
	if got := mustMetric(t, env, "dollars_available", "session", "unbilled").Value; got != int64(0) {
		t.Errorf("unbilled dollars_available = %v, want 0", got)
	}
	if got := mustMetric(t, env, "dollars_available", "session", "billed").Value; got != int64(1) {
		t.Errorf("billed dollars_available = %v, want 1", got)
	}
	// The token side is still reported for the unbilled session: no cost-state
	// means no dollars, not no measurement.
	if got := mustMetric(t, env, "rebilled_tokens", "session", "unbilled").Value; got != int64(900) {
		t.Errorf("unbilled rebilled_tokens = %v, want 900", got)
	}
	if got := mustMetric(t, env, "sessions_without_cost_state", "corpus", "").Value; got != int64(1) {
		t.Errorf("sessions_without_cost_state = %v, want 1", got)
	}

	var said bool
	for _, w := range env.Warnings {
		if strings.Contains(w, "no cost-state") && strings.Contains(w, "not zero") {
			said = true
		}
	}
	if !said {
		t.Errorf("no warning says dollars are unavailable rather than zero: %v", env.Warnings)
	}
}

// TestAttachmentRollupDimension is the corrected-dimension gate at the report
// level: attachment volume is grouped by attachment.type, and the three types
// that carry the un-redacted names appear with their own keys — none of which
// hookName can reach.
func TestAttachmentRollupDimension(t *testing.T) {
	attach := func(payload string) string {
		return marshal(t, map[string]any{
			"type":       "attachment",
			"sessionId":  "s1",
			"timestamp":  "2026-09-01T10:00:00.000Z",
			"attachment": json.RawMessage(payload),
		})
	}
	env := attributeCorpus(t,
		attach(`{"type":"hook_success","hookName":"boost-awareness"}`),
		attach(`{"type":"skill_listing","names":["ponytail"],"content":"- ponytail: be lazy"}`),
		attach(`{"type":"deferred_tools_delta","addedNames":["mem_save"],"addedLines":["mem_save"]}`),
		attach(`{"type":"mcp_instructions_delta","addedNames":["context7"],"addedBlocks":["docs"]}`),
	)

	for _, want := range []struct{ dimension, key string }{
		{"attachment_type", "hook_success"},
		{"attachment_type", "skill_listing"},
		{"attachment_type", "deferred_tools_delta"},
		{"attachment_type", "mcp_instructions_delta"},
		{transcript.DimHookName, "boost-awareness"},
		{transcript.DimSkill, "ponytail"},
		{transcript.DimMcpTool, "mem_save"},
		{transcript.DimMcpServer, "context7"},
	} {
		if findMetric(env, "attachment_bytes", want.dimension, want.key) == nil {
			t.Errorf("no attachment_bytes row for %s/%s", want.dimension, want.key)
		}
	}
	// hookName is a sub-dimension, never the top level.
	if findMetric(env, "attachment_bytes", "attachment_type", "boost-awareness") != nil {
		t.Error("hookName leaked into the attachment_type dimension")
	}
	if got := mustMetric(t, env, "attachment_events", "corpus", "").Value; got != int64(4) {
		t.Errorf("attachment_events = %v, want 4", got)
	}
}

// TestRenderAttributeReadsEnvelope pins the table to the envelope so the two
// can never report different numbers, and proves an unbilled session prints
// "unavailable" rather than a zero.
func TestRenderAttributeReadsEnvelope(t *testing.T) {
	env := attributeCorpus(t,
		usageLine(t, "s1", "msg_1", "claude-opus-5",
			tokens{in: 100, out: 20, read: 2_360}, map[string]string{"attributionPlugin": "desplega"}),
	)
	var buf strings.Builder
	if err := RenderAttribute(&buf, env); err != nil {
		t.Fatalf("RenderAttribute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"CORPUS", "ATTRIBUTION_PLUGIN", "desplega", "unavailable", "warning:"} {
		if !strings.Contains(out, want) {
			t.Errorf("table is missing %q\n%s", want, out)
		}
	}
}
