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
	env, err := CorruptionEnvelope(dir, "test")
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
	env, err := CorruptionEnvelope(dir, "test")
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
	env, err := CorruptionEnvelope(dir, "test")
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
	env, err := CorruptionEnvelope(dir, "test")
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
