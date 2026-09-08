package report

import (
	"encoding/json"
	"fmt"
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
	env, err := AttributeEnvelope(dir, "test", Window{})
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
// the total is the measured bill. Money is integer microdollars, so each row
// rounds to the nearest micro; the summed rows may drift from the rounded bill
// by up to one micro per row, never more.
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
	// The split has to be a split, not the bill twice.
	alpha := mustMetric(t, env, "cost_usd", "attribution_skill", "alpha").Value.(int64)
	if alpha <= 0 || alpha >= billed {
		t.Errorf("alpha allocated %v, want a share strictly inside (0, %v)", alpha, billed)
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
		if m.Unit != "microdollars" {
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

// TestMoneyRowsAreIntegerMicrodollars pins the wire shape of money
// (microdollar-cost-1): every money row carries an integer value under the
// `microdollars` unit, and where coverage is complete the identity
// allocated + unallocated = billed holds in integers (microdollar-cost-2).
func TestMoneyRowsAreIntegerMicrodollars(t *testing.T) {
	const model = "claude-opus-5"
	const bill = 12.34
	env := attributeCorpus(t,
		usageLine(t, "s1", "msg_1", model,
			tokens{in: 900, out: 40, read: 120_000, c5m: 3_000},
			map[string]string{"attributionSkill": "alpha"}),
		usageLine(t, "s1", "msg_2", model,
			tokens{in: 12, out: 4_400, read: 8_000, c1h: 50_000},
			map[string]string{"attributionSkill": "beta"}),
		costLine(t, "s1", bill, map[string]transcript.ModelUsage{model: {
			Input: 912, Output: 4_440, CacheRead: 128_000, CacheCreate: 53_000, CostUSD: bill,
		}}),
	)

	money := map[string]bool{
		"cost_usd": true, "allocated_cost_usd": true, "unallocated_cost_usd": true,
	}
	var moneyRows int
	for _, m := range env.Metrics {
		if !money[m.Name] {
			continue
		}
		moneyRows++
		if m.Unit != "microdollars" {
			t.Errorf("money row %s/%s carries unit %q, want microdollars", m.Name, m.Key, m.Unit)
		}
		if _, ok := m.Value.(int64); !ok {
			t.Errorf("money row %s/%s value %v is %T, want int64", m.Name, m.Key, m.Value, m.Value)
		}
	}
	if moneyRows == 0 {
		t.Fatal("no money rows emitted — the contract was not exercised")
	}

	// Coverage is complete here (the fixture bills exactly what the transcript
	// records), so nothing is unallocated for lack of events and the two
	// corpus rows must sum to the measured bill in whole microdollars.
	billed := int64(math.Round(bill * 1e6))
	alloc := mustMetric(t, env, "allocated_cost_usd", "corpus", "").Value.(int64)
	unalloc := mustMetric(t, env, "unallocated_cost_usd", "corpus", "").Value.(int64)
	if alloc+unalloc != billed {
		t.Errorf("allocated (%d) + unallocated (%d) = %d, want billed %d", alloc, unalloc, alloc+unalloc, billed)
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

// TestAttachmentBytesMeasurePayload pins the byte unit (defect 5): every
// attachment figure is the attachment object's own raw JSON, never the JSONL
// record line. The fixture's record carries a fat envelope (uuid, timestamp,
// cwd, sessionId, gitBranch, version) and a top-level `rendered` field
// duplicating the content, so the line is strictly fatter than the payload —
// under the old LineBytes rule both figures below over-count the envelope.
func TestAttachmentBytesMeasurePayload(t *testing.T) {
	const payload = `{"type":"output_style","style":"A style whose raw payload is this whole object"}`
	line := marshal(t, map[string]any{
		"type":       "attachment",
		"uuid":       "u1",
		"timestamp":  "2026-09-01T10:00:00.000Z",
		"cwd":        "/tmp/proj",
		"sessionId":  "s1",
		"gitBranch":  "main",
		"version":    "1.2.3",
		"rendered":   payload,
		"attachment": json.RawMessage(payload),
	})
	if int64(len(line)) <= int64(len(payload)) {
		t.Fatalf("fixture is not fat: line %d, payload %d", len(line), len(payload))
	}
	env := attributeCorpus(t, line)

	want := int64(len(payload))
	if got := mustMetric(t, env, "attachment_bytes", "corpus", "").Value; got != want {
		t.Errorf("corpus attachment_bytes = %v, want payload %d", got, want)
	}
	if got := mustMetric(t, env, "attachment_bytes", "attachment_type", "output_style").Value; got != want {
		t.Errorf("output_style attachment_bytes = %v, want payload %d", got, want)
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
	if err := RenderAttribute(&buf, env, 15); err != nil {
		t.Fatalf("RenderAttribute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"CORPUS", "ATTRIBUTION_PLUGIN", "desplega", "unavailable", "warning:"} {
		if !strings.Contains(out, want) {
			t.Errorf("table is missing %q\n%s", want, out)
		}
	}
}

// TestTruncateReportsTotal pins the truncation contract. A cut table has to
// name both counts and the flag that lifts the cut: the line this replaced
// printed "… and 48 more tool rows (see --json)", a count whose escape hatch
// was a flag it did not name and that no other line mentioned either.
func TestTruncateReportsTotal(t *testing.T) {
	rows := make([]row, 20)
	for i := range rows {
		rows[i] = row{key: fmt.Sprintf("k%02d", i), values: map[string]Metric{}}
	}

	shown, total := truncate(rows, 5)
	if len(shown) != 5 || total != 20 {
		t.Errorf("truncate(20 rows, 5) = %d shown / %d total, want 5 / 20", len(shown), total)
	}
	// Zero or less is --all and --top 0 both; a cap at or above the row count
	// withholds nothing either way.
	for _, top := range []int{0, -1, 20, 99} {
		shown, total := truncate(rows, top)
		if len(shown) != 20 || total != 20 {
			t.Errorf("truncate(20 rows, %d) = %d shown / %d total, want 20 / 20", top, len(shown), total)
		}
	}

	var buf strings.Builder
	writeTruncation(&buf, 5, 20, "session")
	if want := "showing top 5 of 20 session rows — use --all"; !strings.Contains(buf.String(), want) {
		t.Errorf("truncation line = %q, want it to contain %q", buf.String(), want)
	}

	// Nothing withheld, nothing to announce — otherwise --all would print a
	// line saying it showed everything.
	buf.Reset()
	writeTruncation(&buf, 20, 20, "session")
	if buf.Len() != 0 {
		t.Errorf("an un-truncated table announced %q, want silence", buf.String())
	}
}

// TestAbsentFieldIsNotThinAttribution separates the two reasons a table comes
// out empty.
//
// Thin attribution means the field exists and most responses did not carry it,
// so the named rows are a floor and more use can raise them. An absent field
// means no event in the corpus carries it at all, so the table cannot improve
// however long it runs — the Claude Code versions that wrote the transcripts
// never recorded it. Reporting the second as the first quotes a floor where
// there is no measurement, which is the one thing this tool exists not to do.
//
// Measured: on a 41-file corpus written by 7 CLI versions, `attributionSkill`
// appears zero times and four of the five dimensions are absent this way.
func TestAbsentFieldIsNotThinAttribution(t *testing.T) {
	const model = "claude-opus-5"
	allFive := map[string]string{
		"attributionSkill":     "alpha",
		"attributionPlugin":    "desplega",
		"attributionAgent":     "phase-running",
		"attributionMcpServer": "context7",
		"attributionMcpTool":   "query-docs",
	}

	t.Run("absent", func(t *testing.T) {
		env := attributeCorpus(t,
			usageLine(t, "s1", "msg_1", model, tokens{in: 10, read: 10_000}, nil),
		)
		joined := strings.Join(env.Warnings, "\n")
		for _, want := range []string{
			"unmeasured, not unclaimed", "5 of 5",
			"attributionSkill", "attributionMcpTool", "cannot improve",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("warnings do not say %q: %v", want, env.Warnings)
			}
		}
		// Naming a share for an absent field would dress the absence up as
		// coverage, so the majority-unclaimed line must stay quiet here.
		if strings.Contains(joined, "are (unattributed)") {
			t.Errorf("majority-unclaimed fired for an absent field: %v", env.Warnings)
		}
	})

	// The false-positive gate: one claimed response makes the field present,
	// and from there thinness is the correct finding.
	t.Run("present but thin", func(t *testing.T) {
		env := attributeCorpus(t,
			usageLine(t, "s1", "msg_1", model, tokens{in: 10, read: 1_000}, allFive),
			usageLine(t, "s1", "msg_2", model, tokens{in: 10, read: 9_000}, nil),
		)
		joined := strings.Join(env.Warnings, "\n")
		if !strings.Contains(joined, "are (unattributed)") {
			t.Errorf("a present-but-thin dimension must still warn: %v", env.Warnings)
		}
		if strings.Contains(joined, "unmeasured, not unclaimed") {
			t.Errorf("absent-field warning fired for a present field: %v", env.Warnings)
		}
	})
}

// TestUnattributedWarnsAboveThreshold pins the confidence bound. Re-billed
// tokens are cache reads and nothing else, so `read` alone sets each share.
//
// The warning is one line for every thin dimension, the same collapse
// TestAbsentModelsCollapse pins below — and it fires on the share, so a corpus
// whose fields are almost all populated must produce none.
func TestUnattributedWarnsAboveThreshold(t *testing.T) {
	const model = "claude-opus-5"
	allFive := map[string]string{
		"attributionSkill":     "alpha",
		"attributionPlugin":    "desplega",
		"attributionAgent":     "phase-running",
		"attributionMcpServer": "context7",
		"attributionMcpTool":   "query-docs",
	}
	unattributedShare := func(t *testing.T, claimed, unclaimed int64) []string {
		t.Helper()
		env := attributeCorpus(t,
			usageLine(t, "s1", "msg_1", model, tokens{in: 10, read: claimed}, allFive),
			usageLine(t, "s1", "msg_2", model, tokens{in: 10, read: unclaimed}, nil),
		)
		var thin []string
		for _, w := range env.Warnings {
			if strings.Contains(w, "are (unattributed)") {
				thin = append(thin, w)
			}
		}
		return thin
	}

	// 6,000 of 10,000 re-billed tokens carry no attribution field at all: 60%
	// unattributed in every one of the five dimensions.
	thin := unattributedShare(t, 4_000, 6_000)
	if len(thin) != 1 {
		t.Fatalf("want exactly one collapsed warning, got %d: %v", len(thin), thin)
	}
	for _, want := range []string{"5 of 5", "attribution_skill 60.0%", "attribution_mcp_tool 60.0%", "floor, not a total"} {
		if !strings.Contains(thin[0], want) {
			t.Errorf("warning %q does not say %q", thin[0], want)
		}
	}

	// 500 of 10,000 — 5%, under the threshold on all five. Silence is the
	// result: the caveat has to mean something when it does appear.
	if quiet := unattributedShare(t, 9_500, 500); len(quiet) != 0 {
		t.Errorf("5%% unattributed warned anyway: %v", quiet)
	}
}

// TestAbsentModelsCollapse pins the warning shape. A model billed in a session
// the transcript never recorded is a real finding, but one line per session
// reached ~50 rows on the real corpus and buried every other warning. One line,
// a pair count, and the distinct model names — the per-session detail stays in
// the coverage_ratio rows.
func TestAbsentModelsCollapse(t *testing.T) {
	env := attributeCorpus(t,
		usageLine(t, "s1", "m1", "opus", tokens{in: 100, out: 10}, nil),
		costLine(t, "s1", 1.0, map[string]transcript.ModelUsage{
			"opus":     {Input: 100, Output: 10, CostUSD: 0.9},
			"haiku-bg": {Input: 10, CostUSD: 0.1}}),
		costLine(t, "s2", 0.5, map[string]transcript.ModelUsage{
			"haiku-bg": {Input: 10, CostUSD: 0.5}}),
		costLine(t, "s3", 0.5, map[string]transcript.ModelUsage{
			"sonnet-bg": {Input: 5, CostUSD: 0.5}}),
	)

	var absent []string
	for _, w := range env.Warnings {
		if strings.Contains(w, "no transcript events") {
			absent = append(absent, w)
		}
	}
	if len(absent) != 1 {
		t.Fatalf("want exactly one collapsed warning, got %d: %v", len(absent), absent)
	}
	for _, want := range []string{"3 billed session-model pairs", "haiku-bg", "sonnet-bg"} {
		if !strings.Contains(absent[0], want) {
			t.Errorf("warning %q does not name %q", absent[0], want)
		}
	}
	if n := strings.Count(absent[0], "haiku-bg"); n != 1 {
		t.Errorf("a model billed in two sessions must be named once, named %d times: %q", n, absent[0])
	}
}

// isZeroValue reports whether a metric value is a numeric zero — the shape a
// window-emptied keyless row would take if it were emitted instead of omitted.
func isZeroValue(v any) bool {
	switch n := v.(type) {
	case int:
		return n == 0
	case int64:
		return n == 0
	case float64:
		return n == 0
	}
	return false
}

// TestAttributeEmptyWindowOmitsNotZeroes extends the time-window-8 gate to the
// attribute command: when the window matches no events, every windowed KEYLESS
// corpus row is omitted — a measured zero the window emptied is an over-claim
// Validate cannot catch — and exactly one warning states the miss.
//
// attribute has no corpus-wide keyless row to keep rendering (files/bytes and
// parse errors belong to scan), so its corpus block is empty by design under a
// no-match window; the C1 warning is what still renders. The estimated money
// pair is gated by the same omit path.
func TestAttributeEmptyWindowOmitsNotZeroes(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		usageLine(t, "s1", "msg_1", "claude-opus-5", tokens{in: 100, out: 10, read: 200, c5m: 30},
			map[string]string{"attributionSkill": "skill-1"}),
		costLine(t, "s1", 0.7, nil),
	}
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := NewWindow("2030-01-01", "2030-01-31")
	if err != nil {
		t.Fatal(err)
	}
	env, err := AttributeEnvelope(dir, "test", w)
	if err != nil {
		t.Fatalf("AttributeEnvelope: %v", err)
	}

	// No windowed keyless row may exist at all, let alone carry a zero.
	for _, m := range env.Metrics {
		if m.Dimension != "corpus" || m.Key != "" || m.Derivation != Measured {
			continue
		}
		if isZeroValue(m.Value) {
			t.Errorf("%s = %v emitted over an empty window — omit, never zero", m.Name, m.Value)
		}
	}
	for _, name := range []string{
		"responses_naive", "responses_distinct", "responses_duplicate",
		"duplicate_rate_percent", "fresh_tokens", "rebilled_tokens",
		"output_tokens", "thinking_tokens", "rebill_multiplier",
		"sessions", "sessions_with_cost_state", "sessions_without_cost_state",
		"cost_state_coverage_percent", "attachment_events", "attachment_bytes",
		"attachment_share_percent", "attachment_types",
	} {
		if m := findMetric(env, name, "corpus", ""); m != nil {
			t.Errorf("%s = %v emitted over an empty window — omit, never zero", name, m.Value)
		}
	}
	// The estimated money pair rides the same omit path.
	for _, name := range []string{"allocated_cost_usd", "unallocated_cost_usd"} {
		if m := findMetric(env, name, "corpus", ""); m != nil {
			t.Errorf("%s = %v emitted over an empty window — omit, never zero", name, m.Value)
		}
	}
	// Exactly one warning states the miss.
	n := 0
	for _, warn := range env.Warnings {
		if strings.Contains(warn, "matched no events") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d no-match warnings, want exactly one: %v", n, env.Warnings)
	}
	// The C1 warning still renders — the run is windowed even when it matched
	// nothing, and the reader is owed the four items before the empty table.
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "a lexical subset of the corpus") {
		t.Errorf("warnings = %v, want the C1 warning", env.Warnings)
	}
}
