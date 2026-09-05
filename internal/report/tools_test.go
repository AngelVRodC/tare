package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// toolCorpus writes one transcript and returns its directory.
func toolCorpus(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func use(id, name string) string {
	return `{"type":"assistant","sessionId":"s1","timestamp":"2026-09-01T10:00:00.000Z",` +
		`"message":{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `"}]}}`
}

func result(id, content string, isError bool) string {
	e := "false"
	if isError {
		e = "true"
	}
	return `{"type":"user","sessionId":"s1","timestamp":"2026-09-01T10:00:01.000Z",` +
		`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id +
		`","content":` + content + `,"is_error":` + e + `}]}}`
}

// metricValue pulls one flat row back out of the envelope.
func metricValue(t *testing.T, env Envelope, name, dimension, key string) any {
	t.Helper()
	for _, m := range env.Metrics {
		if m.Name == name && m.Dimension == dimension && m.Key == key {
			return m.Value
		}
	}
	t.Fatalf("metric %s/%s/%s was never emitted", name, dimension, key)
	return nil
}

// TestUnmatchedJoin is the phase gate on the loud half of the contract: a
// tool_result whose tool_use_id matches nothing is reported, never silently
// dropped into a bucket or added to a tool that did not produce it. The
// measured baseline on the real corpus is zero, so any count is a finding.
func TestUnmatchedJoin(t *testing.T) {
	dir := toolCorpus(t,
		use("toolu_1", "Bash"),
		result("toolu_1", `"hello"`, false),
		result("toolu_ghost", `"orphaned result"`, false),
	)
	env, err := ToolsEnvelope(dir, "test")
	if err != nil {
		t.Fatalf("ToolsEnvelope: %v", err)
	}

	if got := metricValue(t, env, "unmatched_results", "corpus", ""); got != int64(1) {
		t.Errorf("unmatched_results = %v, want 1", got)
	}
	if got := metricValue(t, env, "calls", "corpus", ""); got != int64(1) {
		t.Errorf("calls = %v, want 1 — the unmatched result must not be counted as a call", got)
	}
	if got := metricValue(t, env, "context_bytes", "tool", "Bash"); got != int64(5) {
		t.Errorf("Bash context_bytes = %v, want 5 — the orphan must not be attributed to Bash", got)
	}
	var named bool
	for _, w := range env.Warnings {
		if strings.Contains(w, "toolu_ghost") {
			named = true
		}
	}
	if !named {
		t.Errorf("warnings = %v, want one naming toolu_ghost", env.Warnings)
	}
}

// TestUnansweredUse keeps the other direction separate: a tool_use with no
// result is the live tail of a running session, not a broken join.
func TestUnansweredUse(t *testing.T) {
	dir := toolCorpus(t,
		use("toolu_1", "Bash"),
		result("toolu_1", `"done"`, false),
		use("toolu_2", "Read"),
	)
	env, err := ToolsEnvelope(dir, "test")
	if err != nil {
		t.Fatalf("ToolsEnvelope: %v", err)
	}
	if got := metricValue(t, env, "unanswered_uses", "corpus", ""); got != int64(1) {
		t.Errorf("unanswered_uses = %v, want 1", got)
	}
	if got := metricValue(t, env, "unmatched_results", "corpus", ""); got != int64(0) {
		t.Errorf("unmatched_results = %v, want 0", got)
	}
}

// TestRollupAndMCPSplit checks the aggregation arithmetic and the
// mcp__<server>__<tool> classification the phase requires.
func TestRollupAndMCPSplit(t *testing.T) {
	const mcpName = "mcp__plugin_sre_grafana-prod__query_loki_logs"
	dir := toolCorpus(t,
		use("t1", "Bash"), result("t1", `"aaaa"`, false),
		use("t2", "Bash"), result("t2", `"bb"`, true),
		use("t3", mcpName), result("t3", `[{"type":"text","text":"1234567"}]`, false),
		use("t4", "mcp__context7__query-docs"), result("t4", `"xyz"`, false),
	)
	env, err := ToolsEnvelope(dir, "test")
	if err != nil {
		t.Fatalf("ToolsEnvelope: %v", err)
	}

	want := map[[3]string]any{
		{"calls", "tool", "Bash"}:                                  int64(2),
		{"context_bytes", "tool", "Bash"}:                          int64(6),
		{"produced_bytes", "tool", "Bash"}:                         int64(6),
		{"errors", "tool", "Bash"}:                                 int64(1),
		{"context_bytes", "tool", mcpName}:                         int64(7),
		{"calls", "mcp_server", "plugin_sre_grafana-prod"}:         int64(1),
		{"context_bytes", "mcp_server", "plugin_sre_grafana-prod"}: int64(7),
		{"context_bytes", "mcp_server", "context7"}:                int64(3),
		{"calls", "corpus", ""}:                                    int64(4),
		{"context_bytes", "corpus", ""}:                            int64(16),
		{"distinct_tools", "corpus", ""}:                           3,
	}
	for k, exp := range want {
		if got := metricValue(t, env, k[0], k[1], k[2]); got != exp {
			t.Errorf("%s/%s/%s = %v, want %v", k[0], k[1], k[2], got, exp)
		}
	}

	// A non-MCP tool must not land in the server dimension.
	for _, m := range env.Metrics {
		if m.Dimension == "mcp_server" && m.Key == "Bash" {
			t.Errorf("Bash was classified as an MCP server")
		}
	}
	// Heaviest context first, so the table needs no second sort.
	var order []string
	for _, m := range env.Metrics {
		if m.Dimension == "tool" && m.Name == "calls" {
			order = append(order, m.Key)
		}
	}
	if len(order) != 3 || order[0] != mcpName {
		t.Errorf("tool order = %v, want the heaviest context first", order)
	}
}

// TestSplitMCP covers the name classification on its own, including the names
// that must not be split.
func TestSplitMCP(t *testing.T) {
	cases := []struct {
		name, server, tool string
		ok                 bool
	}{
		{"mcp__context7__query-docs", "context7", "query-docs", true},
		{"mcp__plugin_sre_jaeger-qa__find-traces", "plugin_sre_jaeger-qa", "find-traces", true},
		{"mcp__srv__a__b", "srv", "a__b", true}, // only the first two separators are structural
		{"Bash", "", "", false},
		{"mcp__nosuffix", "", "", false},
		{"mcp____tool", "", "", false},
	}
	for _, tc := range cases {
		server, tool, ok := splitMCP(tc.name)
		if ok != tc.ok || server != tc.server || tool != tc.tool {
			t.Errorf("splitMCP(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.name, server, tool, ok, tc.server, tc.tool, tc.ok)
		}
	}
}

// TestExternalisedRollup checks that the produced-vs-context split is reported
// and that a side file disagreeing with persistedOutputSize is a stated
// defect, since the measured baseline is 39 of 39 byte-exact.
func TestExternalisedRollup(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.txt")
	bad := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(good, bytes.Repeat([]byte("g"), 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, bytes.Repeat([]byte("b"), 10), 0o644); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(dir, "gone.txt")

	ext := func(id, path string, size int) string {
		return `{"type":"user","sessionId":"s1","timestamp":"2026-09-01T10:00:01.000Z",` +
			`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id +
			`","content":"<persisted-output>\nsaved"}]},` +
			`"toolUseResult":{"stdout":"","persistedOutputPath":"` + path +
			`","persistedOutputSize":` + strconv.Itoa(size) + `}}`
	}
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte(strings.Join([]string{
		use("t1", "Bash"), ext("t1", good, 5000),
		use("t2", "Bash"), ext("t2", bad, 9999),
		use("t3", "Bash"), ext("t3", gone, 1234),
		use("t4", "Bash"), result("t4", `"inline"`, false),
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env, err := ToolsEnvelope(dir, "test")
	if err != nil {
		t.Fatalf("ToolsEnvelope: %v", err)
	}

	if got := metricValue(t, env, "externalised_results", "corpus", ""); got != int64(3) {
		t.Errorf("externalised_results = %v, want 3", got)
	}
	if got := metricValue(t, env, "externalised_produced_bytes", "corpus", ""); got != int64(5000+9999+1234) {
		t.Errorf("externalised_produced_bytes = %v, want %d", got, 5000+9999+1234)
	}
	// The placeholder is 24 bytes ("<persisted-output>\nsaved") on each of three.
	if got := metricValue(t, env, "externalised_context_bytes", "corpus", ""); got != int64(72) {
		t.Errorf("externalised_context_bytes = %v, want 72", got)
	}
	// Inline result adds its own 6 bytes to Bash produced; externalised ones
	// contribute their side-file sizes instead.
	if got := metricValue(t, env, "produced_bytes", "tool", "Bash"); got != int64(5000+9999+1234+6) {
		t.Errorf("Bash produced_bytes = %v, want %d", got, 5000+9999+1234+6)
	}

	joined := strings.Join(env.Warnings, "\n")
	if !strings.Contains(joined, "size mismatch") || !strings.Contains(joined, "bad.txt") {
		t.Errorf("warnings must name the size mismatch: %v", env.Warnings)
	}
	if !strings.Contains(joined, "gone.txt") {
		t.Errorf("warnings must name the missing side file: %v", env.Warnings)
	}
}

// TestToolsEnvelopeContract keeps the phase-1 envelope guarantee true for this
// command too: valid JSON, a derivation on every row.
func TestToolsEnvelopeContract(t *testing.T) {
	dir := toolCorpus(t, use("t1", "Bash"), result("t1", `"hi"`, false))
	env, err := ToolsEnvelope(dir, "test")
	if err != nil {
		t.Fatalf("ToolsEnvelope: %v", err)
	}
	if env.Command != "tools" {
		t.Errorf("command = %q, want tools", env.Command)
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

// TestRenderToolsReadsEnvelope pins the table to the envelope, so the two can
// never report different numbers.
func TestRenderToolsReadsEnvelope(t *testing.T) {
	dir := toolCorpus(t,
		use("t1", "Bash"), result("t1", `"hello"`, false),
		use("t2", "mcp__context7__query-docs"), result("t2", `"docs"`, false),
	)
	env, err := ToolsEnvelope(dir, "test")
	if err != nil {
		t.Fatalf("ToolsEnvelope: %v", err)
	}
	var buf bytes.Buffer
	if err := RenderTools(&buf, env); err != nil {
		t.Fatalf("RenderTools: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"CORPUS", "TOOL", "CONTEXT", "PRODUCED", "Bash", "MCP_SERVER", "context7", "measured"} {
		if !strings.Contains(out, want) {
			t.Errorf("table is missing %q\n%s", want, out)
		}
	}
}
