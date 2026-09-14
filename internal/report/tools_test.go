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
	return useAt("2026-09-01T10:00:00.000Z", id, name)
}

// useAt is use() with the event timestamp spelled out, which is what a window
// test has to vary.
func useAt(ts, id, name string) string {
	return `{"type":"assistant","sessionId":"s1","timestamp":"` + ts + `",` +
		`"message":{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `"}]}}`
}

func result(id, content string, isError bool) string {
	return resultAt("2026-09-01T10:00:01.000Z", id, content, isError)
}

func resultAt(ts, id, content string, isError bool) string {
	e := "false"
	if isError {
		e = "true"
	}
	return `{"type":"user","sessionId":"s1","timestamp":"` + ts + `",` +
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

// metricOrNil returns one flat row's value, or nil when the row is absent.
// Absence is the assertion under test for omitted windowed metrics, so it
// must not be fatal the way metricValue is.
func metricOrNil(env Envelope, name, dimension, key string) any {
	for _, m := range env.Metrics {
		if m.Name == name && m.Dimension == dimension && m.Key == key {
			return m.Value
		}
	}
	return nil
}

// attachment is an `attachment` event whose `attachment` object is spelled
// raw, which is what the rent fixtures vary.
func attachment(ts, raw string) string {
	return `{"type":"attachment","sessionId":"s1","timestamp":"` + ts + `","attachment":` + raw + `}`
}

// TestToolsEnvelopeRentJoinsCalls pins the join: rent (attachment bytes an
// installed thing costs just by existing) lands on the same key as its calls.
//
// The fixture exercises every normalization branch at once — a colon rent key
// that matches a call row, one that matches nothing, and a free-form key —
// plus the honest-zero rule: a rent-only entity emits a FULL measured-zero
// stat row (all five metrics; the counter streamed the whole corpus), while a
// called entity with no rent emits no rent row at all (absent is not zero).
func TestToolsEnvelopeRentJoinsCalls(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"),
		[]byte(`{"enabledPlugins":{"sre@acme":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	dir := toolCorpus(t,
		use("t1", "mcp__plugin_sre_grafana-prod__query"),
		result("t1", `"aaaa"`, false),
		use("t2", "Bash"),
		result("t2", `"bb"`, false),
		attachment("2026-09-01T10:00:02.000Z",
			`{"type":"mcp_instructions_delta",`+
				`"addedNames":["plugin:sre:grafana-prod","plugin:sre:never-called","claude.ai Notion"],`+
				`"addedBlocks":["INSTR1","INSTR2","FREEFORM"]}`),
	)
	env, err := ToolsEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("ToolsEnvelope: %v", err)
	}

	// Joined: rent and calls on one key — the normalized segment, the
	// call-side spelling, never the colon form.
	if got := metricValue(t, env, "rent_bytes", "mcp_server", "plugin_sre_grafana-prod"); got != int64(6) {
		t.Errorf("joined rent_bytes = %v, want 6", got)
	}
	if got := metricValue(t, env, "calls", "mcp_server", "plugin_sre_grafana-prod"); got != int64(1) {
		t.Errorf("joined calls = %v, want 1", got)
	}
	for _, colon := range []string{"plugin:sre:grafana-prod", "plugin:sre:never-called"} {
		if got := metricOrNil(env, "rent_bytes", "mcp_server", colon); got != nil {
			t.Errorf("colon-form rent row %q was emitted — rent keys must normalize to the call-side spelling", colon)
		}
	}

	// Rent-only entity: full measured-zero stat row beside its rent.
	if got := metricValue(t, env, "rent_bytes", "mcp_server", "plugin_sre_never-called"); got != int64(6) {
		t.Errorf("rent-only rent_bytes = %v, want 6", got)
	}
	for _, m := range []string{"calls", "context_bytes", "image_bytes", "produced_bytes", "errors"} {
		if got := metricValue(t, env, m, "mcp_server", "plugin_sre_never-called"); got != int64(0) {
			t.Errorf("rent-only %s = %v, want measured 0", m, got)
		}
	}

	// Called without rent: no rent row — absent, never 0.
	if got := metricOrNil(env, "rent_bytes", "mcp_server", "Bash"); got != nil {
		t.Errorf("Bash rent_bytes = %v emitted — called-without-rent must stay absent", got)
	}
	// The rent dimensions are mcp_server, skill and plugin — never tool.
	for _, m := range env.Metrics {
		if m.Name == "rent_bytes" && m.Dimension == "tool" {
			t.Errorf("rent_bytes emitted on the tool dimension")
		}
	}

	// Plugin-level rent lands on the plugin dim under the plugin name, once.
	if got := metricValue(t, env, "rent_bytes", "plugin", "sre"); got != int64(12) {
		t.Errorf("plugin rent_bytes = %v, want 12 (6+6)", got)
	}
	// ...and is not double-counted back onto the server segment.
	if got := metricValue(t, env, "rent_bytes", "mcp_server", "plugin_sre_grafana-prod"); got != int64(6) {
		t.Errorf("joined server rent_bytes = %v, want 6 — plugin rent must not re-land on the server", got)
	}

	// Free-form key: verbatim, rent-only, never joined.
	if got := metricValue(t, env, "rent_bytes", "mcp_server", "claude.ai Notion"); got != int64(8) {
		t.Errorf("free-form rent_bytes = %v, want 8 verbatim", got)
	}
	for _, m := range env.Metrics {
		if m.Dimension == "plugin" && m.Key == "claude.ai Notion" {
			t.Errorf("free-form rent leaked onto the plugin dimension")
		}
	}

	// Call-side rows are byte-identical to pre-change values.
	want := map[[3]string]any{
		{"calls", "tool", "Bash"}:                                        int64(1),
		{"context_bytes", "tool", "Bash"}:                                int64(2),
		{"context_bytes", "tool", "mcp__plugin_sre_grafana-prod__query"}: int64(4),
		{"calls", "corpus", ""}:                                          int64(2),
	}
	for k, exp := range want {
		if got := metricValue(t, env, k[0], k[1], k[2]); got != exp {
			t.Errorf("%s/%s/%s = %v, want %v — call-side rows must not move", k[0], k[1], k[2], got, exp)
		}
	}

	// Authority unavailable: rent prints VERBATIM, no plugin rows, one loud
	// warning — never a zero.
	bare := toolCorpus(t,
		use("t1", "mcp__plugin_sre_grafana-prod__query"),
		result("t1", `"aaaa"`, false),
		attachment("2026-09-01T10:00:02.000Z",
			`{"type":"mcp_instructions_delta","addedNames":["plugin:sre:grafana-prod"],"addedBlocks":["INSTR1"]}`),
	)
	t.Setenv("HOME", t.TempDir()) // no settings.json
	env2, err := ToolsEnvelope(bare, "test", Window{})
	if err != nil {
		t.Fatalf("ToolsEnvelope (no authority): %v", err)
	}
	if got := metricValue(t, env2, "rent_bytes", "mcp_server", "plugin:sre:grafana-prod"); got != int64(6) {
		t.Errorf("unnormalized rent_bytes = %v, want 6 verbatim when the authority is unavailable", got)
	}
	if got := metricOrNil(env2, "rent_bytes", "plugin", "sre"); got != nil {
		t.Errorf("plugin rent row emitted with no authority — pluginRent must be omitted")
	}
	warned := strings.Join(env2.Warnings, "\n")
	if !strings.Contains(warned, "plugin rollup unavailable") {
		t.Errorf("warnings must carry the existing loud branch: %v", env2.Warnings)
	}
}

// skillUse is a tool_use carrying an input object, which only Skill uses are
// ever decoded from.
func skillUse(ts, id, name, input string) string {
	return `{"type":"assistant","sessionId":"s1","timestamp":"` + ts + `",` +
		`"message":{"role":"assistant","content":[{"type":"tool_use","id":"` + id +
		`","name":"` + name + `","input":` + input + `}]}}`
}

// TestToolsEnvelopeSkillCalls pins the use-side half of the join: Skill
// invocations counted off `input.skill`, decoded ONLY for Skill uses.
func TestToolsEnvelopeSkillCalls(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	// A rent-only skill: listed (rent) but never invoked — skill_calls=0.
	dir := toolCorpus(t,
		skillUse("2026-09-01T10:00:00.000Z", "t1", "Skill", `{"skill":"jest-testing"}`),
		result("t1", `"ok"`, false),
		skillUse("2026-09-01T10:00:01.000Z", "t2", "Skill", `{"skill":"jest-testing"}`),
		result("t2", `"ok"`, false),
		skillUse("2026-09-01T10:00:02.000Z", "t3", "Skill", `"not an object"`),
		result("t3", `"ok"`, false),
		skillUse("2026-09-01T10:00:03.000Z", "t4", "Skill", `{}`),
		result("t4", `"ok"`, false),
		skillUse("2026-09-01T10:00:04.000Z", "t5", "Bash", `{"skill":"bash-skill","command":"ls"}`),
		result("t5", `"ok"`, false),
		attachment("2026-09-01T10:00:05.000Z",
			`{"type":"skill_listing","names":["never-listed-skill","jest-testing"],`+
				`"content":"- jest-testing: run tests\n- never-listed-skill: never used"}`),
	)
	env, err := ToolsEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("ToolsEnvelope: %v", err)
	}

	// Skill use with input.skill counted, once per invocation.
	if got := metricValue(t, env, "skill_calls", "skill", "jest-testing"); got != int64(2) {
		t.Errorf("jest-testing skill_calls = %v, want 2", got)
	}
	// A skill listed but never invoked: an honest measured zero.
	if got := metricValue(t, env, "skill_calls", "skill", "never-listed-skill"); got != int64(0) {
		t.Errorf("never-listed-skill skill_calls = %v, want measured 0", got)
	}
	// Its rent is counted too — the join has both halves on one row.
	if got := metricOrNil(env, "rent_bytes", "skill", "never-listed-skill"); got == nil {
		t.Errorf("rent-only skill has no rent_bytes row")
	}

	// Malformed Skill input: counted nowhere, NO warning (unknown fails quietly).
	for _, bad := range []string{"not-a-skill", "(unattributed)"} {
		if got := metricOrNil(env, "skill_calls", "skill", bad); got != nil {
			t.Errorf("malformed Skill input produced a %q row", bad)
		}
	}

	// A non-Skill use carrying a `skill` input field: NO row — pins that the
	// scan never decodes input for any other tool name.
	if got := metricOrNil(env, "skill_calls", "skill", "bash-skill"); got != nil {
		t.Errorf("Bash use with a skill input created a row — non-Skill uses must not be decoded")
	}
	if got := metricValue(t, env, "calls", "tool", "Skill"); got != int64(4) {
		t.Errorf("Skill calls = %v, want 4 — every invocation is still a tool call", got)
	}
	for _, w := range env.Warnings {
		if strings.Contains(w, "skill") && strings.Contains(w, "not a") || strings.Contains(w, "malformed") {
			t.Errorf("malformed skill input must fail quietly: %v", w)
		}
	}
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
	env, err := ToolsEnvelope(dir, "test", Window{})
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
	env, err := ToolsEnvelope(dir, "test", Window{})
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
	env, err := ToolsEnvelope(dir, "test", Window{})
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

// TestMCPServer covers the name classification on its own, including the names
// that carry no server.
func TestMCPServer(t *testing.T) {
	cases := []struct{ name, server string }{
		{"mcp__context7__query-docs", "context7"},
		{"mcp__plugin_sre_jaeger-qa__find-traces", "plugin_sre_jaeger-qa"},
		{"mcp__srv__a__b", "srv"}, // only the first two separators are structural
		{"Bash", ""},
		{"mcp__nosuffix", ""},
		{"mcp____tool", ""},
	}
	for _, tc := range cases {
		if server := mcpServer(tc.name); server != tc.server {
			t.Errorf("mcpServer(%q) = %q, want %q", tc.name, server, tc.server)
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

	env, err := ToolsEnvelope(dir, "test", Window{})
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
	env, err := ToolsEnvelope(dir, "test", Window{})
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

// TestRenderToolsPluginTable pins the third table: every plugin row prints in
// full — tools tables have no --top to hide behind, so a row that goes missing
// is a rendering defect, not a truncation.
func TestRenderToolsPluginTable(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"),
		[]byte(`{"enabledPlugins":{"sre@acme":true,"notes@acme":false}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	dir := toolCorpus(t,
		use("t1", "mcp__plugin_sre_grafana-prod__query"), result("t1", `"aaaa"`, false),
		use("t2", "mcp__plugin_notes_obsidian__search"), result("t2", `"bb"`, true),
		use("t3", "mcp__context7__docs"), result("t3", `"c"`, false),
	)
	env, err := ToolsEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("ToolsEnvelope: %v", err)
	}
	var buf bytes.Buffer
	if err := RenderTools(&buf, env); err != nil {
		t.Fatalf("RenderTools: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"PLUGIN", "sre", "notes"} {
		if !strings.Contains(out, want) {
			t.Errorf("table is missing %q — every plugin row must print in full\n%s", want, out)
		}
	}
	// Tables render after MCP_SERVER, never before: the dimension order is
	// part of what a reader parses off the artifact.
	if strings.Index(out, "MCP_SERVER") > strings.Index(out, "PLUGIN") {
		t.Errorf("PLUGIN table must follow MCP_SERVER\n%s", out)
	}
}

// TestRenderToolsReadsEnvelope pins the table to the envelope, so the two can
// never report different numbers.
func TestRenderToolsReadsEnvelope(t *testing.T) {
	dir := toolCorpus(t,
		use("t1", "Bash"), result("t1", `"hello"`, false),
		use("t2", "mcp__context7__query-docs"), result("t2", `"docs"`, false),
	)
	env, err := ToolsEnvelope(dir, "test", Window{})
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

// TestShareColumnIsIntegerDerived pins the share column to a division of two
// measured int64s rather than an accumulation. Two tools whose results are
// byte-identical are exactly half the corpus context each, so both render
// 50.0% and earn the top concern grade.
func TestShareColumnIsIntegerDerived(t *testing.T) {
	dir := toolCorpus(t,
		use("t1", "Alpha"), result("t1", `"same"`, false),
		use("t2", "Bravo"), result("t2", `"same"`, false),
	)
	env, err := ToolsEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("ToolsEnvelope: %v", err)
	}
	var buf bytes.Buffer
	if err := RenderTools(&buf, env); err != nil {
		t.Fatalf("RenderTools: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"SHARE", "50.0%", "!!"} {
		if !strings.Contains(out, want) {
			t.Errorf("table is missing %q\n%s", want, out)
		}
	}
	if got := strings.Count(out, "50.0%"); got != 2 {
		t.Errorf("want a 50.0%% share on both Alpha and Bravo, got %d\n%s", got, out)
	}
}

// TestConcernThresholds pins the one ladder in this program that was chosen
// rather than measured. The README prints these boundaries as a legend, so a
// change here is a change to documentation too.
func TestConcernThresholds(t *testing.T) {
	cases := []struct {
		share float64
		want  string
	}{
		{0, ""}, {4.9, ""},
		{5, "*"}, {19.9, "*"},
		{20, "**"}, {34.9, "**"},
		{35, "***"}, {49.9, "***"},
		{50, "!!"}, {100, "!!"},
	}
	for _, c := range cases {
		if got := concern(c.share); got != c.want {
			t.Errorf("concern(%v) = %q, want %q", c.share, got, c.want)
		}
	}
}

// TestJoinSurvivesWindowCut is time-window-6's first gate: the tool_use_id →
// name join is built corpus-wide, so a use inside the window whose result lies
// outside still resolves — no unmatched inflation — and the out-of-window
// result's bytes stay out of the tool's rollup.
func TestJoinSurvivesWindowCut(t *testing.T) {
	dir := toolCorpus(t,
		useAt("2026-08-15T10:00:00.000Z", "t1", "Bash"),
		resultAt("2026-08-20T10:00:00.000Z", "t1", `"hello"`, false),
	)
	w, err := NewWindow("2026-08-14", "2026-08-16")
	if err != nil {
		t.Fatal(err)
	}
	env, err := ToolsEnvelope(dir, "test", w)
	if err != nil {
		t.Fatalf("ToolsEnvelope: %v", err)
	}

	if got := metricValue(t, env, "unmatched_results", "corpus", ""); got != int64(0) {
		t.Errorf("unmatched_results = %v, want 0 — a window cut must not sever a join", got)
	}
	// The use is in window and answered corpus-wide, so it is not unanswered:
	// an answer outside the window is still an answer.
	if got := metricOrNil(env, "unanswered_uses", "corpus", ""); got != int64(0) {
		t.Errorf("unanswered_uses = %v, want 0 — a use answered outside the window is answered", got)
	}
	// The only Bash result is out of window, so Bash's rows are omitted —
	// not zeroed — and its bytes exclude the result.
	if got := metricOrNil(env, "context_bytes", "tool", "Bash"); got != nil {
		t.Errorf("Bash context_bytes = %v emitted — the only Bash result is out of window", got)
	}
	// Windowed block counters count only what the window can see.
	if got := metricValue(t, env, "tool_use_blocks", "corpus", ""); got != int64(1) {
		t.Errorf("tool_use_blocks = %v, want 1", got)
	}
	if got := metricValue(t, env, "tool_result_blocks", "corpus", ""); got != int64(0) {
		t.Errorf("tool_result_blocks = %v, want 0 — the only result is out of window", got)
	}
}

// TestUnansweredClassificationUnderWindow pins the unanswered semantics under
// a cut: unanswered_uses counts in-window uses lacking a corpus-wide answer.
// A use answered outside the window is answered; a use outside the window is
// not counted at all.
func TestUnansweredClassificationUnderWindow(t *testing.T) {
	dir := toolCorpus(t,
		useAt("2026-08-15T10:00:00.000Z", "t1", "Bash"), // in-window, never answered
		useAt("2026-08-15T11:00:00.000Z", "t2", "Read"), // in-window, answered outside
		resultAt("2026-08-20T10:00:00.000Z", "t2", `"done"`, false),
		useAt("2026-08-01T10:00:00.000Z", "t3", "Grep"), // out-of-window use
	)
	w, err := NewWindow("2026-08-14", "2026-08-16")
	if err != nil {
		t.Fatal(err)
	}
	env, err := ToolsEnvelope(dir, "test", w)
	if err != nil {
		t.Fatalf("ToolsEnvelope: %v", err)
	}
	if got := metricValue(t, env, "unanswered_uses", "corpus", ""); got != int64(1) {
		t.Errorf("unanswered_uses = %v, want 1 (t1 only — t2 is answered corpus-wide, t3's use is out of window)", got)
	}
	if got := metricOrNil(env, "context_bytes", "tool", "Read"); got != nil {
		t.Errorf("Read context_bytes = %v emitted — the only Read result is out of window", got)
	}
}
