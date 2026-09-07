package report

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestTruncationMarkers is the phase gate on the transcript-only signal: a
// marker a tool wrote is found and attributed to that tool, and clean output
// of the same shape produces nothing. Claude Code externalises rather than
// truncates, so a marker is always the tool's doing.
func TestTruncationMarkers(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // hermetic: this gate is transcript-only
	dir := toolCorpus(t,
		use("t1", "Bash"), result("t1", `"line one\nline two [truncated]"`, false),
		use("t2", "Bash"), result("t2", `"clean output, nothing removed"`, false),
		use("t3", "mcp__codegraph__codegraph_explore"),
		result("t3", `[{"type":"text","text":"func Foo() {}\n(truncated; call codegraph_explore for the rest)"}]`, false),
		use("t4", "WebFetch"), result("t4", `"<response clipped>"`, false),
		use("t5", "Read"), result("t5", `"a file that merely discusses truncation as a topic"`, false),
	)
	env, err := CorruptionEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("CorruptionEnvelope: %v", err)
	}

	if got := metricValue(t, env, "truncated_results", "corpus", ""); got != int64(3) {
		t.Errorf("truncated_results = %v, want 3", got)
	}
	if got := metricValue(t, env, "truncated_results", "tool", "Bash"); got != int64(1) {
		t.Errorf("Bash truncated_results = %v, want 1 — the clean result must not fire", got)
	}
	// Prose about truncation is not a marker. This is the false-positive gate.
	if got := metricValue(t, env, "truncated_results", "tool", "Read"); got != int64(0) {
		t.Errorf("Read truncated_results = %v, want 0 — prose is not a marker", got)
	}
	for _, marker := range []string{"[truncated]", "(truncated; call ", "<response clipped>"} {
		if got := metricValue(t, env, "truncated_results", "truncation_marker", marker); got != int64(1) {
			t.Errorf("marker %q = %v, want 1", marker, got)
		}
	}
	if got := metricValue(t, env, "truncation_marker_kinds", "corpus", ""); got != int64(3) {
		t.Errorf("truncation_marker_kinds = %v, want 3", got)
	}
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "truncation marker") {
		t.Errorf("warnings must state the markers are substring matches: %v", env.Warnings)
	}
}

// resultDenied is result() plus the top-level toolDenialKind Claude Code
// writes on a blocked call. Measured across 41 transcripts and 1,045 tool
// calls: present on 110 events, every one carrying an is_error result and
// never a clean one. Values seen — permission-rule (68), user-rejected (42).
func resultDenied(id, content, kind string) string {
	return resultDeniedAt("2026-09-01T10:00:01.000Z", id, content, kind)
}

// resultDeniedAt is resultDenied with the event timestamp spelled out, which
// is what a window test has to vary.
func resultDeniedAt(ts, id, content, kind string) string {
	return `{"type":"user","sessionId":"s1","timestamp":"` + ts + `",` +
		`"toolDenialKind":"` + kind + `",` +
		`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id +
		`","content":` + content + `,"is_error":true}]}}`
}

// resultPairDenied is one user event carrying two errored tool_result blocks
// under a single toolDenialKind — the shape the ambiguity warning covers.
//
// toolDenialKind is per event and is_error is per block, so this is the one
// transcript shape where the two are not one-to-one. It was measured zero
// times across 1,045 tool calls, which is why it is a fixture rather than a
// corpus figure.
func resultPairDenied(idA, idB, kind string) string {
	return `{"type":"user","sessionId":"s1","timestamp":"2026-09-01T10:00:02.000Z",` +
		`"toolDenialKind":"` + kind + `",` +
		`"message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"` + idA + `","content":"blocked","is_error":true},` +
		`{"type":"tool_result","tool_use_id":"` + idB + `","content":"blocked","is_error":true}]}}`
}

// TestAmbiguousDenialIsAnUpperBound covers the multi-result path.
//
// One event's toolDenialKind is attributed to every errored result in it, so
// those rows are an upper bound and the warning has to say so. The invariant
// that keeps the bound honest is denied + failures == errors: the split is a
// partition of the error count, never an addition to it, which is also what
// keeps `errors` agreeing with the value `tools` emits.
func TestAmbiguousDenialIsAnUpperBound(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // hermetic: this gate is transcript-only

	t.Run("two errored results under one kind", func(t *testing.T) {
		dir := toolCorpus(t,
			use("t1", "Bash"), use("t2", "Grep"),
			resultPairDenied("t1", "t2", "permission-rule"),
			use("t3", "Read"), resultDenied("t3", `"blocked"`, "user-rejected"),
		)
		env, err := CorruptionEnvelope(dir, "test", Window{})
		if err != nil {
			t.Fatalf("CorruptionEnvelope: %v", err)
		}

		for _, c := range []struct {
			name, dim, key string
			want           any
		}{
			{"errors", "corpus", "", int64(3)},
			{"denied", "corpus", "", int64(3)},
			{"failures", "corpus", "", int64(0)},
			// The one denial kind is charged to both tools it blocked.
			{"denied", "tool", "Bash", int64(1)},
			{"denied", "tool", "Grep", int64(1)},
			{"denied", "denial_kind", "permission-rule", int64(2)},
			{"denied", "denial_kind", "user-rejected", int64(1)},
		} {
			if got := metricValue(t, env, c.name, c.dim, c.key); got != c.want {
				t.Errorf("%s/%s/%s = %v, want %v", c.name, c.dim, c.key, got, c.want)
			}
		}

		// denied + failures == errors, exactly. Not merely <=: every errored
		// result increments one side or the other and never both or neither.
		errs := metricValue(t, env, "errors", "corpus", "").(int64)
		den := metricValue(t, env, "denied", "corpus", "").(int64)
		fail := metricValue(t, env, "failures", "corpus", "").(int64)
		if den+fail != errs {
			t.Errorf("denied(%d) + failures(%d) = %d, want errors(%d) exactly", den, fail, den+fail, errs)
		}

		if !strings.Contains(strings.Join(env.Warnings, "\n"), "upper bound") {
			t.Errorf("the ambiguous event must be reported as an upper bound: %v", env.Warnings)
		}
	})

	// The false-positive gate: one result per event is the measured shape, and
	// it must not draw the ambiguity warning.
	t.Run("one result per event stays quiet", func(t *testing.T) {
		dir := toolCorpus(t,
			use("t1", "Bash"), resultDenied("t1", `"blocked"`, "permission-rule"),
			use("t2", "Bash"), result("t2", `"Exit code 1"`, true),
		)
		env, err := CorruptionEnvelope(dir, "test", Window{})
		if err != nil {
			t.Fatalf("CorruptionEnvelope: %v", err)
		}
		if strings.Contains(strings.Join(env.Warnings, "\n"), "upper bound") {
			t.Errorf("unambiguous events must not warn: %v", env.Warnings)
		}
	})
}

// TestRankingSortsFailuresBeforeErrors pins the row order the split changed.
//
// Read errors three times and every one is a denial; Bash errors once and it
// is a real failure. Ranking on errors puts Read first, which is the same
// conflation the split removes, so failures outrank errors.
func TestRankingSortsFailuresBeforeErrors(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // hermetic: this gate is transcript-only
	dir := toolCorpus(t,
		use("r1", "Read"), resultDenied("r1", `"blocked"`, "user-rejected"),
		use("r2", "Read"), resultDenied("r2", `"blocked"`, "user-rejected"),
		use("r3", "Read"), resultDenied("r3", `"blocked"`, "user-rejected"),
		use("b1", "Bash"), result("b1", `"Exit code 1"`, true),
	)
	env, err := CorruptionEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("CorruptionEnvelope: %v", err)
	}
	rows := groupRows(env.Metrics, "tool")
	if len(rows) != 2 {
		t.Fatalf("tool rows = %d, want 2", len(rows))
	}
	if rows[0].key != "Bash" {
		t.Errorf("first row = %q, want Bash — 1 failure outranks 3 denials", rows[0].key)
	}
	// And the premise: Read really does carry more errors than Bash, so this
	// would fail if the sort still keyed on errors.
	if got := metricValue(t, env, "errors", "tool", "Read"); got != int64(3) {
		t.Fatalf("Read errors = %v, want 3 — the fixture no longer tests the ordering", got)
	}
}

// TestDenialIsNotAFailure is the regression test for issue #4.
//
// This command measures evidence that a tool altered an answer. A policy
// denial is not that: the tool never ran. Counting both as one error rate put
// four tools on the author's corpus at a non-zero rate whose real rate was
// zero, and inflated Bash from 11.5% to 26.6%.
//
// errors is asserted whole on purpose. `tools` emits it too, and merge()
// treats a disagreement between two commands as a defect in this tool.
func TestDenialIsNotAFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // hermetic: this gate is transcript-only
	dir := toolCorpus(t,
		use("t1", "Bash"), resultDenied("t1", `"This command requires approval"`, "permission-rule"),
		use("t2", "Bash"), resultDenied("t2", `"The user doesn't want to proceed with this tool use"`, "user-rejected"),
		use("t3", "Bash"), result("t3", `"Exit code 1"`, true),
		use("t4", "Read"), resultDenied("t4", `"The user doesn't want to proceed with this tool use"`, "user-rejected"),
		use("t5", "Grep"), result("t5", `"clean"`, false),
	)
	env, err := CorruptionEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("CorruptionEnvelope: %v", err)
	}

	for _, c := range []struct {
		name, dim, key string
		want           any
	}{
		{"errors", "corpus", "", int64(4)},
		{"denied", "corpus", "", int64(3)},
		{"failures", "corpus", "", int64(1)},
		{"errors", "tool", "Bash", int64(3)},
		{"denied", "tool", "Bash", int64(2)},
		{"failures", "tool", "Bash", int64(1)},
		// Read's only error was a denial, so its failure rate is a true zero.
		{"errors", "tool", "Read", int64(1)},
		{"failures", "tool", "Read", int64(0)},
		{"denied", "denial_kind", "permission-rule", int64(1)},
		{"denied", "denial_kind", "user-rejected", int64(2)},
	} {
		if got := metricValue(t, env, c.name, c.dim, c.key); got != c.want {
			t.Errorf("%s/%s/%s = %v, want %v", c.name, c.dim, c.key, got, c.want)
		}
	}
	if got := metricValue(t, env, "failure_rate_percent", "corpus", ""); got != 20.0 {
		t.Errorf("failure_rate_percent = %v, want 20 — 1 failure in 5 calls", got)
	}
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "toolDenialKind") {
		t.Errorf("warnings must name the field the split reads: %v", env.Warnings)
	}
}

// TestErrorAndEmptyRates covers the other two transcript-only signals, and the
// one case that must NOT be called empty: an externalised result is empty in
// context on purpose and losslessly.
func TestErrorAndEmptyRates(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // hermetic: this gate is transcript-only
	ext := `{"type":"user","sessionId":"s1","timestamp":"2026-09-01T10:00:01.000Z",` +
		`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t4","content":""}]},` +
		`"toolUseResult":{"persistedOutputPath":"/tmp/x.txt","persistedOutputSize":` + strconv.Itoa(9000) + `}}`
	dir := toolCorpus(t,
		use("t1", "Bash"), result("t1", `"ok"`, false),
		use("t2", "Bash"), result("t2", `"boom"`, true),
		use("t3", "Bash"), result("t3", `""`, false),
		use("t4", "Bash"), ext,
	)
	env, err := CorruptionEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("CorruptionEnvelope: %v", err)
	}
	want := map[[3]string]any{
		{"calls", "tool", "Bash"}:              int64(4),
		{"errors", "tool", "Bash"}:             int64(1),
		{"error_rate_percent", "tool", "Bash"}: 25.0,
		{"empty_results", "tool", "Bash"}:      int64(1),
		{"empty_results", "corpus", ""}:        int64(1),
	}
	for k, exp := range want {
		if got := metricValue(t, env, k[0], k[1], k[2]); got != exp {
			t.Errorf("%s/%s/%s = %v, want %v", k[0], k[1], k[2], got, exp)
		}
	}
}

// TestCorruptionEnvelopeContract keeps the phase-1 envelope guarantee true for
// this command too: valid JSON, a derivation on every row.
func TestCorruptionEnvelopeContract(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // keep the shell-outs out of a contract test
	dir := toolCorpus(t, use("t1", "Bash"), result("t1", `"hi"`, false))
	env, err := CorruptionEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("CorruptionEnvelope: %v", err)
	}
	if env.Command != "corruption" {
		t.Errorf("command = %q, want corruption", env.Command)
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, env); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var decoded struct {
		Metrics []map[string]any `json:"metrics"`
	}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	for i, m := range decoded.Metrics {
		if d, _ := m["derivation"].(string); d == "" {
			t.Errorf("metric %d (%v) has no derivation", i, m["name"])
		}
	}
}

// TestRenderCorruptionReadsEnvelope pins the table to the envelope, so the two
// can never report different numbers.
func TestRenderCorruptionReadsEnvelope(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := toolCorpus(t,
		use("t1", "Bash"), result("t1", `"boom [truncated]"`, true),
		use("t2", "Read"), result("t2", `"fine"`, false),
	)
	env, err := CorruptionEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("CorruptionEnvelope: %v", err)
	}
	var buf bytes.Buffer
	if err := RenderCorruption(&buf, env, 15); err != nil {
		t.Fatalf("RenderCorruption: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"CORPUS", "TOOL", "ERROR %", "TRUNCATED", "Bash", "Read",
		"TRUNCATION_MARKER", "[truncated]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table is missing %q\n%s", want, out)
		}
	}
}

// TestCorruptionKeyedStatsWindowed is time-window-6 for the corruption
// command: the join map is built corpus-wide, per-key stats (tool,
// denial_kind, truncation_marker) aggregate only in-window results, and the
// unmatched counter stays whole-corpus so the join's integrity audit cannot
// be deflated by a window cut.
func TestCorruptionKeyedStatsWindowed(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // hermetic: this gate is transcript-only
	dir := toolCorpus(t,
		useAt("2026-08-15T10:00:00.000Z", "t1", "Bash"), // in-window denial
		resultDeniedAt("2026-08-15T10:00:01.000Z", "t1", `"blocked"`, "user-rejected"),
		useAt("2026-08-15T11:00:00.000Z", "t2", "Read"), // in-window use...
		// ...whose truncated result lies outside the window.
		resultAt("2026-08-20T10:00:00.000Z", "t2", `"[truncated]"`, false),
		useAt("2026-08-01T10:00:00.000Z", "t3", "Bash"), // out-of-window denial
		resultDeniedAt("2026-08-01T10:00:01.000Z", "t3", `"blocked"`, "permission-rule"),
		resultAt("2026-08-20T11:00:00.000Z", "ghost", `"orphaned"`, false), // out-of-window, unmatched
	)
	w, err := NewWindow("2026-08-14", "2026-08-16")
	if err != nil {
		t.Fatal(err)
	}
	env, err := CorruptionEnvelope(dir, "test", w)
	if err != nil {
		t.Fatalf("CorruptionEnvelope: %v", err)
	}

	// The in-window denial is attributed to its tool and its kind.
	if got := metricValue(t, env, "denied", "tool", "Bash"); got != int64(1) {
		t.Errorf("denied/tool/Bash = %v, want 1", got)
	}
	if got := metricValue(t, env, "denied", "denial_kind", "user-rejected"); got != int64(1) {
		t.Errorf("denied/denial_kind/user-rejected = %v, want 1", got)
	}
	// t3's denial is out of window: the kind is absent, not zero.
	if got := metricOrNil(env, "denied", "denial_kind", "permission-rule"); got != nil {
		t.Errorf("denied/denial_kind/permission-rule = %v emitted — that denial is out of window", got)
	}
	// Read's result is out of window: its truncation is absent, not zero.
	if got := metricOrNil(env, "truncated_results", "tool", "Read"); got != nil {
		t.Errorf("truncated_results/tool/Read = %v emitted — that result is out of window", got)
	}
	if got := metricOrNil(env, "truncated_results", "truncation_marker", "[truncated]"); got != nil {
		t.Errorf("marker [truncated] = %v emitted — that result is out of window", got)
	}
	// Only Bash was rolled up: Read never produced an in-window result.
	if got := metricValue(t, env, "distinct_tools", "corpus", ""); got != int64(1) {
		t.Errorf("distinct_tools = %v, want 1 — only tools with in-window results are rolled up", got)
	}
	// The unmatched counter is whole-corpus: the ghost result is out of
	// window but still audited.
	if got := metricValue(t, env, "unmatched_results", "corpus", ""); got != int64(1) {
		t.Errorf("unmatched_results = %v, want 1 — join-integrity counters stay corpus-wide", got)
	}
	// The denial warning is defect detection: it still fires from the
	// windowed totals it describes.
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "policy denials") {
		t.Errorf("warnings = %v, want the denial warning to still fire", env.Warnings)
	}
}
