package report

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// The OpenCode doctor fixtures mirror doctor_test.go's: trees under
// t.TempDir() with PATH pinned to a fixture bin, and the database half
// injected through openCodeDoctorEnvelope's observation seam — no test here
// shells out to sqlite3 or touches the real HOME.

// openCodeDoctorEnv runs the seam with an injected observation set, and
// returns the envelope already validated — the free contract check every
// doctor test gets.
func openCodeDoctorEnv(t *testing.T, configRoot, projectDir string, obs []openCodeObservation, w Window) Envelope {
	t.Helper()
	env, err := openCodeDoctorEnvelope(configRoot, projectDir, "/oc", "test", obs, openCodeSpan{}, nil, 4096, w)
	if err != nil {
		t.Fatalf("openCodeDoctorEnvelope: %v", err)
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return env
}

func TestOpenCodeDoctorConfigMatrix(t *testing.T) {
	configRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(configRoot, "opencode.json"), `{"mcp":{
		"urlonly":{"type":"remote","url":"https://x"},
		"local":{"type":"local","command":["`+doctorBinName+`","mcp"]},
		"bare":{"command":"`+doctorBinName+`"},
		"ghost":{"command":["totally-missing-binary-xyz"]},
		"nothing":{"type":"local"},
		"weird":{"type":"websocket","url":"wss://x"},
		"strkey":"not-an-object"}}`)

	env := openCodeDoctorEnv(t, configRoot, projectDir, nil, Window{})
	if got := metricValue(t, env, "mcp_servers_checked", "corpus", ""); got != int64(7) {
		t.Errorf("mcp_servers_checked = %v, want 7", got)
	}
	for _, c := range []struct{ key, kind string }{
		{"ghost", "command_not_found"},
		{"nothing", "url_missing"},
		{"weird", "server_type_unknown"},
		{"strkey", "server_type_unknown"},
	} {
		if got := metricValue(t, env, "findings", "mcp_server", c.key); got != int64(1) {
			t.Errorf("findings/mcp_server/%s = %v, want 1", c.key, got)
		}
		if want := "mcp_server " + c.key + ": " + c.kind; !strings.Contains(doctorWarnText(env), want) {
			t.Errorf("warn text is missing %q:\n%s", want, doctorWarnText(env))
		}
	}
	// Remote and command-present-local entries are clean: no findings rows,
	// and no liveness claim was made for the url server.
	for _, key := range []string{"urlonly", "local", "bare"} {
		if got := metricOrNil(env, "findings", "mcp_server", key); got != nil {
			t.Errorf("clean entry %s emitted %v, want no row", key, got)
		}
	}
	// The PATH-scope caveat rides command_not_found with the same words
	// Claude's pass prints — shared helper, not a forked copy.
	if w := doctorWarnText(env); !strings.Contains(w, "PATH of the running tare process") {
		t.Errorf("command_not_found must name its measurement scope:\n%s", w)
	}
}

func TestOpenCodeDoctorProjectOverrideAndCollision(t *testing.T) {
	configRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(configRoot, "opencode.json"), `{"mcp":{
		"shared":{"command":["totally-missing-binary-xyz"]},
		"global-only":{"type":"remote","url":"https://g"}}}`)
	doctorWrite(t, filepath.Join(projectDir, "opencode.json"), `{"mcp":{
		"shared":{"command":["`+doctorBinName+`"]},
		"project-only":{"type":"remote","url":"https://p"}}}`)

	env := openCodeDoctorEnv(t, configRoot, projectDir, nil, Window{})
	if got := metricOrNil(env, "findings", "mcp_server", "shared"); got != nil {
		t.Errorf("the losing global entry was judged: %v", got)
	}
	if got := metricValue(t, env, "mcp_servers_checked", "corpus", ""); got != int64(3) {
		t.Errorf("mcp_servers_checked = %v, want 3 — a collision is one server", got)
	}
	want := `mcp server "shared": the entry in ` + filepath.Join(projectDir, "opencode.json") +
		" overrides the one in " + filepath.Join(configRoot, "opencode.json") +
		" (project config wins on name collision)"
	if !strings.Contains(doctorWarnText(env), want) {
		t.Errorf("collision warn missing:\n%s", doctorWarnText(env))
	}
}

// TestOpenCodeDoctorConfigAbsenceLadder pins absent≠zero in its three opencode
// rungs: no file at all omits the count with a warn naming the unread
// opencode.jsonc; a file that parses with no mcp block is a measured zero;
// a file that cannot be parsed withholds the count as a floor without taking
// the skills block down.
func TestOpenCodeDoctorConfigAbsenceLadder(t *testing.T) {
	t.Run("no config files omit the count", func(t *testing.T) {
		configRoot, projectDir, _ := doctorPaths(t)
		env := openCodeDoctorEnv(t, configRoot, projectDir, nil, Window{})
		if got := metricOrNil(env, "mcp_servers_checked", "corpus", ""); got != nil {
			t.Errorf("mcp_servers_checked = %v, want omitted, not zero", got)
		}
		w := doctorWarnText(env)
		if !strings.Contains(w, "opencode.jsonc") || !strings.Contains(w, "omitted, not zero") {
			t.Errorf("the omission warn must name opencode.jsonc:\n%s", w)
		}
	})
	t.Run("parses-with-no-mcp is a measured zero", func(t *testing.T) {
		configRoot, projectDir, _ := doctorPaths(t)
		doctorWrite(t, filepath.Join(configRoot, "opencode.json"), `{"theme":"dark"}`)
		env := openCodeDoctorEnv(t, configRoot, projectDir, nil, Window{})
		if got := metricValue(t, env, "mcp_servers_checked", "corpus", ""); got != int64(0) {
			t.Errorf("mcp_servers_checked = %v, want a measured 0", got)
		}
	})
	t.Run("broken config withholds the count but not the other blocks", func(t *testing.T) {
		configRoot, projectDir, _ := doctorPaths(t)
		doctorWrite(t, filepath.Join(projectDir, "opencode.json"), `{"mcp": {`)
		doctorSkill(t, filepath.Join(configRoot, "skills"), "ok", strings.ReplaceAll(doctorGoodSkill, "%s", "ok"))
		env := openCodeDoctorEnv(t, configRoot, projectDir, nil, Window{})
		if got := metricOrNil(env, "mcp_servers_checked", "corpus", ""); got != nil {
			t.Errorf("mcp_servers_checked = %v, want omitted over a floor", got)
		}
		if !strings.Contains(doctorWarnText(env), "floor, not a verdict") {
			t.Errorf("the broken source must name the floor:\n%s", doctorWarnText(env))
		}
		if got := metricValue(t, env, "skills_checked", "corpus", ""); got != int64(1) {
			t.Errorf("the broken config must not take the skills block down: %v", got)
		}
	})
}

// TestOpenCodeDoctorSkillsRoots walks all four roots OpenCode reads with the
// shared frontmatter rules: the finding kinds and key shape are exactly
// Claude's — only the root list differs.
func TestOpenCodeDoctorSkillsRoots(t *testing.T) {
	configRoot, projectDir, _ := doctorPaths(t)
	roots := []string{
		filepath.Join(configRoot, "skills"),
		filepath.Join(projectDir, ".opencode", "skills"),
		filepath.Join(projectDir, ".claude", "skills"),
		filepath.Join(projectDir, ".agents", "skills"),
	}
	for _, root := range roots {
		doctorSkill(t, root, "good", strings.ReplaceAll(doctorGoodSkill, "%s", "good"))
	}
	doctorSkill(t, roots[1], "bad", "---\nname: not-bad\ndescription: d\n---\n")

	env := openCodeDoctorEnv(t, configRoot, projectDir, nil, Window{})
	if got := metricValue(t, env, "skills_checked", "corpus", ""); got != int64(5) {
		t.Errorf("skills_checked = %v, want 5", got)
	}
	key := filepath.Join(roots[1], "bad")
	if got := metricValue(t, env, "findings", "skill", key); got != int64(1) {
		t.Errorf("findings/skill/%s = %v, want 1", key, got)
	}
	if want := "skill " + key + ": name_mismatch"; !strings.Contains(doctorWarnText(env), want) {
		t.Errorf("the shared parser's finding must ride the same warn voice:\n%s", doctorWarnText(env))
	}
}

// TestOpenCodeDoctorPluginsOmitted is the absent≠zero rule earning its keep:
// no plugins_checked row, no plugin dimension, and one warn that refuses to
// read like a clean bill of health.
func TestOpenCodeDoctorPluginsOmitted(t *testing.T) {
	configRoot, projectDir, _ := doctorPaths(t)
	env := openCodeDoctorEnv(t, configRoot, projectDir, nil, Window{})
	if got := metricOrNil(env, "plugins_checked", "corpus", ""); got != nil {
		t.Errorf("plugins_checked = %v, want absent — OpenCode has no manifest to count", got)
	}
	if rows := dimRows(env, "plugin"); len(rows) != 0 {
		t.Errorf("plugin dimension rows on the opencode run: %+v", rows)
	}
	w := doctorWarnText(env)
	for _, want := range []string{"plugin checks are claude-only", "not a claim the harness is healthy"} {
		if !strings.Contains(w, want) {
			t.Errorf("plugin warn is missing %q:\n%s", want, w)
		}
	}
}

// TestOpenCodeDoctorJoinRollup is the seam's arithmetic: configured ∪
// observed → the two miss kinds, the tool_sessions denominator, the disabled
// exclusion, and the longest-name-first match that mcpServer can never do.
func TestOpenCodeDoctorJoinRollup(t *testing.T) {
	configRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(configRoot, "opencode.json"), `{"mcp":{
		"server_a":{"command":["`+doctorBinName+`"]},
		"server_a_b":{"command":["`+doctorBinName+`"]},
		"urlonly":{"type":"remote","url":"https://u"},
		"off":{"type":"remote","url":"https://o","enabled":false}}}`)
	obs := []openCodeObservation{
		{Tool: "server_a_tool", Session: "s1"},
		{Tool: "server_a_tool", Session: "s3"},
		{Tool: "server_a_b_x", Session: "s2"}, // longest-first: server_a_b, not server_a
		{Tool: "built_in_thing", Session: "s2"},
		{Tool: "server_a", Session: "s4"}, // bare server name matches no "<server>_" prefix
	}
	env := openCodeDoctorEnv(t, configRoot, projectDir, obs, Window{})

	// s1..s4 all carry a tool call — the denominator is wider than Claude's
	// mcp-only one, and named for what it counts.
	if got := metricValue(t, env, doctorToolSessions, "corpus", ""); got != int64(4) {
		t.Errorf("tool_sessions = %v, want 4", got)
	}
	if got := metricValue(t, env, doctorNeverObserved, "mcp_server", "urlonly"); got != int64(4) {
		t.Errorf("never_observed/urlonly = %v, want the denominator 4", got)
	}
	// server_a_b was observed through its longest-first claim: no never row.
	for _, key := range []string{"server_a", "server_a_b"} {
		if got := metricOrNil(env, doctorNeverObserved, "mcp_server", key); got != nil {
			t.Errorf("%s was called and still earned never_observed: %v", key, got)
		}
	}
	// The disabled server is inert by contract: never_observed is not claimed
	// against it, and the warn names the state it was excluded for.
	if got := metricOrNil(env, doctorNeverObserved, "mcp_server", "off"); got != nil {
		t.Errorf("a disabled server was flagged never_observed: %v", got)
	}
	w := doctorWarnText(env)
	if !strings.Contains(w, "enabled:false") || !strings.Contains(w, "off") {
		t.Errorf("the disabled exclusion must name the state and the server:\n%s", w)
	}
	// Built-in-looking names and the bare server name land in
	// unconfigured_observed, with the name-based caveat in the warn.
	if got := metricValue(t, env, doctorUnconfiguredObserved, "mcp_server", "built_in_thing"); got != int64(1) {
		t.Errorf("unconfigured_observed/built_in_thing = %v, want 1 session", got)
	}
	if got := metricValue(t, env, doctorUnconfiguredObserved, "mcp_server", "server_a"); got != int64(1) {
		t.Errorf("unconfigured_observed/server_a = %v, want 1 — a bare server name matches no server_<underscore> prefix", got)
	}
	for _, want := range []string{
		`mcp_server "built_in_thing": observed in the opencode database (1 tool-using session(s))`,
		"opencode records no attribution fields, so this join is name-based",
		"built-in tools, plugin-provided servers and config scopes tare",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("join warn text is missing %q:\n%s", want, w)
		}
	}
	// Partial config was false here, so the floor caveat must not print.
	if strings.Contains(w, "configured set is partial") {
		t.Errorf("a fully readable config earned the partial-config caveat:\n%s", w)
	}
	for _, m := range dimRows(env, "mcp_server") {
		if m.Derivation != Measured {
			t.Errorf("join row %s/%s is %s, want measured", m.Name, m.Key, m.Derivation)
		}
	}
}

// TestOpenCodeDoctorJoinZeroDenominator mirrors Claude's suppression rule: an
// absence measured against zero tool-using sessions says nothing, so the rows
// ride one warning instead of printing as measured misses.
func TestOpenCodeDoctorJoinZeroDenominator(t *testing.T) {
	configRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(configRoot, "opencode.json"), `{"mcp":{
		"quiet":{"command":["`+doctorBinName+`"]}}}`)
	env := openCodeDoctorEnv(t, configRoot, projectDir, nil, Window{})

	if got := metricOrNil(env, doctorNeverObserved, "mcp_server", "quiet"); got != nil {
		t.Errorf("never_observed claimed against a zero denominator: %v", got)
	}
	if got := metricValue(t, env, doctorToolSessions, "corpus", ""); got != int64(0) {
		t.Errorf("tool_sessions = %v, want a measured 0 — the database was read", got)
	}
	if !strings.Contains(doctorWarnText(env), "never_observed is not claimed") {
		t.Errorf("the withheld rows need their denominator warning:\n%s", doctorWarnText(env))
	}
}

// TestOpenCodeDoctorJoinOmittedWhenDBAbsent keeps the blocks standing alone:
// a database that cannot be read costs the join and its warning — never a
// zero — while the config passes run untouched. Through the real
// OpenCodeDoctorEnvelope with an empty dbDir, so no sqlite3 is shelled out
// against anything but a provably absent file.
func TestOpenCodeDoctorJoinOmittedWhenDBAbsent(t *testing.T) {
	configRoot, projectDir, dbDir := doctorPaths(t)
	doctorWrite(t, filepath.Join(configRoot, "opencode.json"), `{"mcp":{
		"solo":{"command":["`+doctorBinName+`"]}}}`)
	env, err := OpenCodeDoctorEnvelope(configRoot, projectDir, dbDir, "test", Window{})
	if err != nil {
		t.Fatalf("OpenCodeDoctorEnvelope: %v", err)
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := metricOrNil(env, doctorToolSessions, "corpus", ""); got != nil {
		t.Errorf("tool_sessions = %v, want omitted — no database was streamed", got)
	}
	if got := metricOrNil(env, doctorNeverObserved, "mcp_server", "solo"); got != nil {
		t.Errorf("never_observed printed without an observation: %v", got)
	}
	if got := metricValue(t, env, "mcp_servers_checked", "corpus", ""); got != int64(1) {
		t.Errorf("the config pass must stand alone: mcp_servers_checked = %v", got)
	}
	w := doctorWarnText(env)
	if !strings.Contains(w, "cross-join is omitted") || !strings.Contains(w, "opencode.db") {
		t.Errorf("the omitted join must say why:\n%s", w)
	}
	if env.Corpus.Files != 1 {
		t.Errorf("corpus files = %d, want the config file alone (no DB counted)", env.Corpus.Files)
	}
}

// TestOpenCodeDoctorWindowWarnsAndStaysPointInTime pins the window posture:
// the database join is windowed in SQL (verified by the query test), the
// static rows are not, and the envelope says both.
func TestOpenCodeDoctorWindowWarnsAndStaysPointInTime(t *testing.T) {
	configRoot, projectDir, _ := doctorPaths(t)
	doctorSkill(t, filepath.Join(configRoot, "skills"), "ok", strings.ReplaceAll(doctorGoodSkill, "%s", "ok"))
	w, err := NewWindow("2026-09-01", "2026-09-10")
	if err != nil {
		t.Fatal(err)
	}
	env := openCodeDoctorEnv(t, configRoot, projectDir, nil, w)
	if got := metricValue(t, env, "skills_checked", "corpus", ""); got != int64(1) {
		t.Errorf("the window must not gate static rows: skills_checked = %v", got)
	}
	warn := doctorWarnText(env)
	for _, want := range []string{"point-in-time", "this run measures only events with", "window matched no events"} {
		if !strings.Contains(warn, want) {
			t.Errorf("windowed run is missing %q:\n%s", want, warn)
		}
	}
	if env.Corpus.Since != "2026-09-01" || env.Corpus.Until != "2026-09-10" {
		t.Errorf("window header = %q..%q, want the bounds", env.Corpus.Since, env.Corpus.Until)
	}
	// tool_sessions went through addWin with no matched events: omitted, not
	// zeroed — the over-claim Validate accepts unconditionally.
	if got := metricOrNil(env, doctorToolSessions, "corpus", ""); got != nil {
		t.Errorf("tool_sessions = %v under a window that matched nothing, want omitted", got)
	}
}

func TestOpenCodeObserveQueryWindowComposition(t *testing.T) {
	cases := []struct{ since, until, want, absent string }{
		// The absent marker for the unwindowed case is the AND that would
		// open a time predicate — min/max(time_created) ride every query.
		{"", "", "", "'tool' AND time_created"},
		{"2026-09-01", "", "time_created >= strftime('%s','2026-09-01')*1000 + 0", ""},
		{"2026-09-01T12:00:00.500Z", "", "strftime('%s','2026-09-01T12:00:00.500Z')*1000 + 500", ""},
		{"", "2026-09-10", "time_created < strftime('%s','2026-09-10','+1 day')*1000", ""},
		{"", "2026-09-10T08:00:00.000Z", "time_created <= strftime('%s','2026-09-10T08:00:00.000Z')*1000 + 0", ""},
	}
	for _, c := range cases {
		w, err := NewWindow(c.since, c.until)
		if err != nil {
			t.Fatal(err)
		}
		q := openCodeObserveQuery(w)
		if c.want != "" && !strings.Contains(q, c.want) {
			t.Errorf("window %s..%s query is missing %q:\n%s", c.since, c.until, c.want, q)
		}
		if c.absent != "" && strings.Contains(q, c.absent) {
			t.Errorf("unwindowed query carries %q:\n%s", c.absent, q)
		}
	}
}

// TestOpenCodeObservationsDecode pins the decode seam the same way
// TestOpenCodeRowsDecode pins its twin: real wire shape, span folded across
// groups, and silence — not an error — for a zero-row reply.
func TestOpenCodeObservationsDecode(t *testing.T) {
	out := []byte(`[{"tool":"bash","session":"ses_1","min_ms":100,"max_ms":200},
{"tool":"engram_mem_search","session":"ses_2","min_ms":50,"max_ms":500}]`)
	obs, span, err := openCodeObservations(out)
	if err != nil {
		t.Fatalf("openCodeObservations: %v", err)
	}
	if len(obs) != 2 || obs[1].Tool != "engram_mem_search" || obs[1].Session != "ses_2" {
		t.Fatalf("rows = %+v, want the two literal groups", obs)
	}
	if span.MinMs != 50 || span.MaxMs != 500 {
		t.Errorf("span = %+v, want {50 500} folded across groups", span)
	}
	if rows, span, err := openCodeObservations([]byte("")); err != nil || len(rows) != 0 || span != (openCodeSpan{}) {
		t.Errorf("empty output = %v, %+v, %v; want no rows, zero span, no error", rows, span, err)
	}
	if _, _, err := openCodeObservations([]byte("Error: no such table: part")); err == nil {
		t.Error("a non-JSON reply must fail the decode")
	}
}

func TestOpenCodeFirstToken(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`["boost","graph","serve"]`, "boost"},
		{`[" spaced "]`, "spaced"},
		{`"engram"`, "engram"},
		{`[]`, ""},
		{`""`, ""},
		{`{"a":1}`, ""},
		{`123`, ""},
		{``, ""},
	}
	for _, c := range cases {
		if got := openCodeFirstToken(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("openCodeFirstToken(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// TestOpenCodeDoctorTwoRootsUnreadableIsFatal mirrors Claude's one hard
// error: with neither root there is nothing left to report. A missing
// *database* is not among them — the block-stands-alone test above pins that.
func TestOpenCodeDoctorTwoRootsUnreadableIsFatal(t *testing.T) {
	base := t.TempDir()
	_, err := OpenCodeDoctorEnvelope(filepath.Join(base, "no-config"), filepath.Join(base, "no-project"),
		base, "test", Window{})
	if err == nil {
		t.Fatal("both roots missing must be an error")
	}
	for _, want := range []string{"no-config", "no-project"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name both roots: %v", err)
		}
	}
}

// TestOpenCodeDoctorDeterminismAndContract is the standing gate: four runs
// over one fixture tree and one observation set produce byte-identical JSON,
// every row is measured, and the envelope passes Validate — map order never
// reaches the output.
func TestOpenCodeDoctorDeterminismAndContract(t *testing.T) {
	configRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(configRoot, "opencode.json"), `{"mcp":{
		"gone":{"command":["totally-missing-binary-xyz"]},
		"url":{"type":"remote","url":"https://x"},
		"off":{"type":"local","command":["`+doctorBinName+`"],"enabled":false}}}`)
	doctorWrite(t, filepath.Join(projectDir, "opencode.json"), `{"mcp":{
		"gone":{"command":["`+doctorBinName+`"]},
		"proj":{"command":["`+doctorBinName+`"]}}}`)
	doctorSkill(t, filepath.Join(projectDir, ".agents", "skills"), "alpha", "# no frontmatter\n")
	obs := []openCodeObservation{
		{Tool: "proj_tool", Session: "s1"},
		{Tool: "other_thing", Session: "s2"},
		{Tool: "x", Session: "s2"},
	}
	var first []byte
	for run := range 4 {
		env := openCodeDoctorEnv(t, configRoot, projectDir, obs, Window{})
		if env.Command != "doctor" {
			t.Errorf("command = %q, want doctor", env.Command)
		}
		if env.Corpus.Dir != "/oc" {
			t.Errorf("corpus dir = %q, want the database dir", env.Corpus.Dir)
		}
		for _, m := range env.Metrics {
			if m.Derivation != Measured {
				t.Errorf("metric %s/%s/%s is %s, not measured", m.Name, m.Dimension, m.Key, m.Derivation)
			}
		}
		var buf bytes.Buffer
		if err := WriteJSON(&buf, env); err != nil {
			t.Fatalf("WriteJSON: %v", err)
		}
		if run == 0 {
			first = bytes.Clone(buf.Bytes())
			continue
		}
		if !bytes.Equal(first, buf.Bytes()) {
			t.Fatalf("run %d produced different JSON over unchanged input", run)
		}
	}
}

// TestRenderDoctorOpenCodeEnvelope proves the shared renderer needs no
// OpenCode branch: it reads rows and warnings only, so the same function that
// renders Claude's doctor renders this one, the plugin dimension simply absent
// and the opencode join rows in place. The Claude render tests stand untouched
// beside it.
func TestRenderDoctorOpenCodeEnvelope(t *testing.T) {
	configRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(configRoot, "opencode.json"), `{"mcp":{
		"idle":{"type":"remote","url":"https://i"}}}`)
	doctorSkill(t, filepath.Join(configRoot, "skills"), "bad", "---\nname: worse\ndescription: d\n---\n")
	obs := []openCodeObservation{{Tool: "mystery_tool", Session: "s1"}}
	env := openCodeDoctorEnv(t, configRoot, projectDir, obs, Window{})

	var buf bytes.Buffer
	if err := RenderDoctor(&buf, env); err != nil {
		t.Fatalf("RenderDoctor: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"tare doctor — /oc", "SKILL", "MCP_SERVER", "NEVER_OBSERVED",
		"UNCONFIGURED_OBSERVED", "idle", "mystery_tool", "tool_sessions",
		"warning: plugins: plugin checks are claude-only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output is missing %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		// The warn sentence about plugins is fine; a CORPUS table line
		// starting with the row name would mean the omitted block printed.
		if strings.HasPrefix(strings.TrimSpace(line), "plugins_checked") {
			t.Errorf("the omitted plugin block printed a row:\n%s", out)
		}
	}
	if strings.Contains(out, "no configuration issues detected") {
		t.Errorf("a run with a finding and two misses declared itself clean:\n%s", out)
	}
	if strings.Contains(out, "\t") {
		t.Errorf("a flushed tabwriter table must carry no tabs:\n%q", out)
	}
}

// TestOpenCodeDoctorDisabledInObservation is the last honest corner of the
// enabled:false matrix: the count includes the disabled server, the findings
// pass still judges its (broken) definition — a disabled misconfiguration is
// a fact about the configuration — and only never_observed stands down.
func TestOpenCodeDoctorDisabledInObservation(t *testing.T) {
	configRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(configRoot, "opencode.json"), `{"mcp":{
		"off":{"type":"local","command":["totally-missing-binary-xyz"],"enabled":false}}}`)
	env := openCodeDoctorEnv(t, configRoot, projectDir, nil, Window{})
	if got := metricValue(t, env, "mcp_servers_checked", "corpus", ""); got != int64(1) {
		t.Errorf("mcp_servers_checked = %v, want 1 — disabled is still configured", got)
	}
	if got := metricValue(t, env, "findings", "mcp_server", "off"); got != int64(1) {
		t.Errorf("a disabled server's broken command earned %v, want its finding", got)
	}
	if got := metricOrNil(env, doctorNeverObserved, "mcp_server", "off"); got != nil {
		t.Errorf("never_observed was claimed against a disabled server: %v", got)
	}
}
