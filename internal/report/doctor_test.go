package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The doctor fixtures are trees, not transcripts: everything is written under
// t.TempDir() and both roots are passed explicitly, so no test touches the
// real HOME, and PATH is pinned to a fixture bin dir so exec.LookPath is as
// hermetic as the toolCorpus fixtures make the transcript scan.

// doctorPaths builds the claudeRoot/projectDir/bin trio under t.TempDir().
// The bin dir carries one executable, doctorBinName, which clean stdio
// servers point at.
func doctorPaths(t *testing.T) (claudeRoot, projectDir, bin string) {
	t.Helper()
	base := t.TempDir()
	claudeRoot = filepath.Join(base, "claude")
	projectDir = filepath.Join(base, "project")
	bin = filepath.Join(base, "bin")
	for _, d := range []string{claudeRoot, projectDir, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tool := filepath.Join(bin, doctorBinName)
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return claudeRoot, projectDir, bin
}

const doctorBinName = "tare-fake-harness-tool"

// doctorWrite creates the parent dirs and writes content to path.
func doctorWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// doctorSkill writes <skillsRoot>/<name>/SKILL.md.
func doctorSkill(t *testing.T, skillsRoot, name, body string) {
	t.Helper()
	doctorWrite(t, filepath.Join(skillsRoot, name, "SKILL.md"), body)
}

// doctorEnv runs the envelope on the fixture roots, fails on error, and
// returns it already validated — every test gets the contract check for free.
func doctorEnv(t *testing.T, claudeRoot, projectDir string) Envelope {
	t.Helper()
	return doctorEnvWindow(t, claudeRoot, projectDir, Window{})
}

func doctorEnvWindow(t *testing.T, claudeRoot, projectDir string, w Window) Envelope {
	t.Helper()
	env, err := DoctorEnvelope(claudeRoot, projectDir, "test", w)
	if err != nil {
		t.Fatalf("DoctorEnvelope: %v", err)
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return env
}

// doctorJoinEnv is doctorEnvWindow's joined twin: the same free contract
// check, plus the transcript corpus the join streams.
func doctorJoinEnv(t *testing.T, claudeRoot, projectDir, corpusDir string, w Window) Envelope {
	t.Helper()
	env, err := DoctorJoinEnvelope(claudeRoot, projectDir, corpusDir, "test", w)
	if err != nil {
		t.Fatalf("DoctorJoinEnvelope: %v", err)
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return env
}

func doctorWarnText(env Envelope) string { return strings.Join(env.Warnings, "\n") }

const doctorGoodSkill = "---\nname: %s\ndescription: A fixture skill.\nallowed-tools: Read, Bash(go:*), WebFetch\n---\n\n# body\n"

// TestDoctorCleanTree is the zero-findings baseline with every block present:
// three honest checked rows, no findings rows, and no warnings at all — a
// healthy config must be silent, not reassured.
func TestDoctorCleanTree(t *testing.T) {
	claudeRoot, projectDir, bin := doctorPaths(t)
	doctorSkill(t, filepath.Join(claudeRoot, "skills"), "greet",
		strings.ReplaceAll(doctorGoodSkill, "%s", "greet"))
	// A project skill with CRLF line endings and an *empty* allowed-tools:
	// the line parser must accept the fences, and an empty list is absent,
	// not malformed.
	doctorSkill(t, filepath.Join(projectDir, ".claude", "skills"), "quiet",
		"---\r\nname: quiet\r\ndescription: Quoted, \"nested\" quotes.\r\nallowed-tools:\r\n---\r\n# body\r\n")
	doctorWrite(t, filepath.Join(claudeRoot, "plugins", "tare-p", ".claude-plugin", "plugin.json"),
		`{"name":"tare-p","description":"x"}`)
	// A plugin dir without a manifest is a layout, not a defect: counted,
	// no finding.
	if err := os.MkdirAll(filepath.Join(claudeRoot, "plugins", "bare"), 0o755); err != nil {
		t.Fatal(err)
	}
	doctorWrite(t, filepath.Join(claudeRoot, "settings.json"),
		`{"mcpServers":{"good":{"command":"`+doctorBinName+`"},"web":{"type":"http","url":"https://x"}}}`)
	doctorWrite(t, filepath.Join(projectDir, ".mcp.json"),
		`{"mcpServers":{"abs-path":{"command":"`+filepath.Join(bin, doctorBinName)+`","type":"stdio"}}}`)

	env := doctorEnv(t, claudeRoot, projectDir)
	if env.Command != "doctor" {
		t.Errorf("command = %q, want doctor", env.Command)
	}
	if env.Corpus.Dir != claudeRoot {
		t.Errorf("corpus dir = %q, want claudeRoot %q", env.Corpus.Dir, claudeRoot)
	}
	for _, c := range []struct {
		name string
		want any
	}{
		{"skills_checked", int64(2)},
		{"plugins_checked", int64(2)},
		{"mcp_servers_checked", int64(3)},
	} {
		if got := metricValue(t, env, c.name, "corpus", ""); got != c.want {
			t.Errorf("%s = %v, want %v", c.name, got, c.want)
		}
	}
	for _, dim := range []string{"skill", "plugin", "mcp_server"} {
		if rows := dimRows(env, dim); len(rows) != 0 {
			t.Errorf("%s table is not empty on a clean tree: %+v", dim, rows)
		}
	}
	if len(env.Warnings) != 0 {
		t.Errorf("a clean tree must warn about nothing: %v", env.Warnings)
	}
	// The corpus header counts exactly the five config files that were read.
	if env.Corpus.Files != 5 || env.Corpus.Bytes <= 0 {
		t.Errorf("corpus files/bytes = %d/%d, want 5 files of positive size", env.Corpus.Files, env.Corpus.Bytes)
	}
}

// TestDoctorSkillFindings triggers each skill finding kind exactly once and
// pins the warn text: the kind verbatim plus the detail that names the value.
func TestDoctorSkillFindings(t *testing.T) {
	cases := []struct {
		name, skill, body, kind, detail string
	}{
		{"frontmatter missing", "plain", "# just body\n",
			"frontmatter_missing", "does not open"},
		{"frontmatter unterminated", "open", "---\nname: open\ndescription: d\n",
			"frontmatter_unterminated", "never closed"},
		{"name mismatch", "right", "---\nname: wrong\ndescription: d\n---\n",
			"name_mismatch", `"wrong" does not match the directory name "right"`},
		{"name absent", "noname", "---\ndescription: d\n---\n",
			"name_mismatch", "carries no name"},
		{"description missing", "nodesc", "---\nname: nodesc\n---\n",
			"description_missing", "no description"},
		{"allowed-tools malformed", "badtools", "---\nname: badtools\ndescription: d\nallowed-tools: Read && Bash(go:*)\n---\n",
			"allowed_tools_malformed", `"Read && Bash(go:*)"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claudeRoot, projectDir, _ := doctorPaths(t)
			doctorSkill(t, filepath.Join(claudeRoot, "skills"), c.skill, c.body)
			env := doctorEnv(t, claudeRoot, projectDir)
			key := filepath.Join(claudeRoot, "skills", c.skill)
			if got := metricValue(t, env, "findings", "skill", key); got != int64(1) {
				t.Errorf("findings/skill/%s = %v, want 1", key, got)
			}
			w := doctorWarnText(env)
			for _, want := range []string{"skill " + key + ": " + c.kind, c.detail} {
				if !strings.Contains(w, want) {
					t.Errorf("warn text is missing %q:\n%s", want, w)
				}
			}
			// The block still measured its one skill even while reporting it.
			if got := metricValue(t, env, "skills_checked", "corpus", ""); got != int64(1) {
				t.Errorf("skills_checked = %v, want 1", got)
			}
		})
	}
}

func TestDoctorParseFrontmatter(t *testing.T) {
	t.Run("crlf block parses like lf", func(t *testing.T) {
		fields, missing, unterminated := doctorParseFrontmatter(
			[]byte("---\r\nname: x\r\ndescription: \"quoted value\"\r\nmetadata:\r\n  author: y\r\n# a comment\r\nname: dup\r\n---\r\nbody\r\n"))
		if missing || unterminated {
			t.Fatalf("missing=%v unterminated=%v, want a clean block", missing, unterminated)
		}
		if fields["name"] != "x" {
			t.Errorf("name = %q, want x — first occurrence wins", fields["name"])
		}
		if fields["description"] != "quoted value" {
			t.Errorf("description = %q, want the unquoted value", fields["description"])
		}
		if _, ok := fields["author"]; ok {
			t.Errorf("nested metadata key leaked to the top level: %v", fields)
		}
	})
	t.Run("fence errors", func(t *testing.T) {
		if _, missing, _ := doctorParseFrontmatter([]byte("nope\n")); !missing {
			t.Error("a blockless file must report missing")
		}
		if _, _, unterminated := doctorParseFrontmatter([]byte("---\nname: x\n")); !unterminated {
			t.Error("an unclosed block must report unterminated")
		}
	})
}

// TestDoctorAllowedToolsBad is the shape table the envelope test can only
// sample: the two sanctioned list shapes, the empty-value-is-absent rule,
// and the operator junk that must read as malformed.
func TestDoctorAllowedToolsBad(t *testing.T) {
	cases := []struct {
		value string
		bad   bool
	}{
		{"Read Edit", false},
		{"Read, Bash(go:*), WebFetch", false},
		{"[Read, Bash(go:*)]", false},
		{"[]", false},
		{"", false},   // absent
		{`""`, false}, // quoted-empty: still absent
		{" , ", false},
		{"mcp/github", false},
		{"Read && Bash", true},
		{"Read; rm -rf /", true},
		{"[Read, Edit", true},
		{"Bash(go:*) )", true},
		{"`backtick`", true},
	}
	for _, c := range cases {
		if got := doctorAllowedToolsBad(c.value); got != c.bad {
			t.Errorf("doctorAllowedToolsBad(%q) = %v, want %v", c.value, got, c.bad)
		}
	}
}

// TestDoctorPluginFindings pins the two plugin kinds; a valid manifest beside
// them keeps its block counted.
func TestDoctorPluginFindings(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(claudeRoot, "plugins", "broken", ".claude-plugin", "plugin.json"), `{"name":`)
	doctorWrite(t, filepath.Join(claudeRoot, "plugins", "other", ".claude-plugin", "plugin.json"), `{"name":"elsewhere"}`)
	doctorWrite(t, filepath.Join(claudeRoot, "plugins", "soul", ".claude-plugin", "plugin.json"), `{"name":"soul"}`)

	env := doctorEnv(t, claudeRoot, projectDir)
	if got := metricValue(t, env, "findings", "plugin", "broken"); got != int64(1) {
		t.Errorf("findings/plugin/broken = %v, want 1", got)
	}
	if got := metricValue(t, env, "findings", "plugin", "other"); got != int64(1) {
		t.Errorf("findings/plugin/other = %v, want 1", got)
	}
	if got := metricOrNil(env, "findings", "plugin", "soul"); got != nil {
		t.Errorf("the valid plugin emitted %v, want no row", got)
	}
	if got := metricValue(t, env, "plugins_checked", "corpus", ""); got != int64(3) {
		t.Errorf("plugins_checked = %v, want 3", got)
	}
	w := doctorWarnText(env)
	for _, want := range []string{"plugin broken: plugin_json_invalid", `plugin other: plugin_name_mismatch — manifest name "elsewhere" does not match the directory name "other"`} {
		if !strings.Contains(w, want) {
			t.Errorf("warn text is missing %q:\n%s", want, w)
		}
	}
}

// TestDoctorMCPServerFindings pins the three server kinds, the broken-entry
// shape, and that the PATH-scope caveat rides the command_not_found line.
func TestDoctorMCPServerFindings(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(claudeRoot, "settings.json"), `{"mcpServers":{
		"weird":{"type":"websocket","url":"wss://x"},
		"nocmd":{"type":"stdio"},
		"ghost":{"command":"totally-missing-binary-xyz"},
		"stringy":"not-an-object"}}`)
	env := doctorEnv(t, claudeRoot, projectDir)
	for _, c := range []struct {
		key, kind string
	}{
		{"weird", "server_type_unknown"},
		{"nocmd", "command_missing"},
		{"ghost", "command_not_found"},
		{"stringy", "server_type_unknown"},
	} {
		if got := metricValue(t, env, "findings", "mcp_server", c.key); got != int64(1) {
			t.Errorf("findings/mcp_server/%s = %v, want 1", c.key, got)
		}
		if want := "mcp_server " + c.key + ": " + c.kind; !strings.Contains(doctorWarnText(env), want) {
			t.Errorf("warn text is missing %q:\n%s", want, doctorWarnText(env))
		}
	}
	if got := metricValue(t, env, "mcp_servers_checked", "corpus", ""); got != int64(4) {
		t.Errorf("mcp_servers_checked = %v, want 4", got)
	}
	if w := doctorWarnText(env); !strings.Contains(w, "PATH of the running tare process") {
		t.Errorf("command_not_found must name its measurement scope:\n%s", w)
	}
}

// TestDoctorMCPCollision pins the precedence rule: the project entry is the
// one checked (the user's broken twin earns no finding) and the collision is
// said out loud.
func TestDoctorMCPCollision(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(claudeRoot, "settings.json"),
		`{"mcpServers":{"shared":{"command":"totally-missing-binary-xyz"},"user-only":{"type":"http"}}}`)
	doctorWrite(t, filepath.Join(projectDir, ".mcp.json"),
		`{"mcpServers":{"shared":{"command":"`+doctorBinName+`"}}}`)
	env := doctorEnv(t, claudeRoot, projectDir)
	if got := metricOrNil(env, "findings", "mcp_server", "shared"); got != nil {
		t.Errorf("the losing user entry was checked too: %v", got)
	}
	if got := metricValue(t, env, "mcp_servers_checked", "corpus", ""); got != int64(2) {
		t.Errorf("mcp_servers_checked = %v, want 2 — a collision is one server, not two", got)
	}
	settings := filepath.Join(claudeRoot, "settings.json")
	mcp := filepath.Join(projectDir, ".mcp.json")
	want := `mcp server "shared": the entry in ` + mcp + " overrides the one in " + settings +
		" (project config wins on name collision)"
	if !strings.Contains(doctorWarnText(env), want) {
		t.Errorf("collision warn missing:\n%s", doctorWarnText(env))
	}
}

// TestDoctorMissingRoots covers the absence ladder in three rungs: both roots
// gone is the one error; a bare existing project root degrades every block to
// omitted-and-warned; and a config that parses with no servers is a measured
// zero, not an absence.
func TestDoctorMissingRoots(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	t.Run("both roots unreadable", func(t *testing.T) {
		base := t.TempDir()
		_, err := DoctorEnvelope(filepath.Join(base, "no-claude"), filepath.Join(base, "no-project"), "test", Window{})
		if err == nil {
			t.Fatal("both roots missing must be an error")
		}
		for _, want := range []string{"no-claude", "no-project"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error must name both roots: %v", err)
			}
		}
	})

	t.Run("bare roots omit every block", func(t *testing.T) {
		claudeRoot, projectDir, _ := doctorPaths(t)
		env := doctorEnv(t, claudeRoot, projectDir)
		for _, name := range []string{"skills_checked", "plugins_checked", "mcp_servers_checked"} {
			if got := metricOrNil(env, name, "corpus", ""); got != nil {
				t.Errorf("%s = %v, want omitted — nothing was measurable", name, got)
			}
		}
		if len(dimRows(env, "skill"))+len(dimRows(env, "plugin"))+len(dimRows(env, "mcp_server")) != 0 {
			t.Error("no blocks ran, so no keyed rows may exist")
		}
		if n := strings.Count(doctorWarnText(env), "omitted, not zero"); n != 3 {
			t.Errorf("every omitted block must warn: %d omission warns, want 3\n%s", n, doctorWarnText(env))
		}
	})

	t.Run("broken settings.json withholds the count but not the other blocks", func(t *testing.T) {
		claudeRoot, projectDir, _ := doctorPaths(t)
		doctorWrite(t, filepath.Join(claudeRoot, "settings.json"), `{"mcpServers": {`)
		doctorSkill(t, filepath.Join(claudeRoot, "skills"), "ok", strings.ReplaceAll(doctorGoodSkill, "%s", "ok"))
		env := doctorEnv(t, claudeRoot, projectDir)
		if got := metricOrNil(env, "mcp_servers_checked", "corpus", ""); got != nil {
			t.Errorf("mcp_servers_checked = %v, want omitted over a floor", got)
		}
		if !strings.Contains(doctorWarnText(env), "is not valid JSON") {
			t.Errorf("the broken settings file must be named: %s", doctorWarnText(env))
		}
		if got := metricValue(t, env, "skills_checked", "corpus", ""); got != int64(1) {
			t.Errorf("the unreadable mcp source must not take the skills block down: got %v", got)
		}
	})

	t.Run("parses-with-no-servers is a measured zero", func(t *testing.T) {
		claudeRoot, projectDir, _ := doctorPaths(t)
		doctorWrite(t, filepath.Join(claudeRoot, "settings.json"), `{"theme":"dark"}`)
		env := doctorEnv(t, claudeRoot, projectDir)
		if got := metricValue(t, env, "mcp_servers_checked", "corpus", ""); got != int64(0) {
			t.Errorf("mcp_servers_checked = %v, want a measured 0", got)
		}
	})
}

// TestDoctorWindowIsWarned pins the honesty of the ignored global flag: an
// active window costs a warning naming the scope, and the envelope header
// still says it measured against no window, because that is the truth.
func TestDoctorWindowIsWarned(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorSkill(t, filepath.Join(claudeRoot, "skills"), "ok", strings.ReplaceAll(doctorGoodSkill, "%s", "ok"))
	w, err := NewWindow("2026-09-01", "")
	if err != nil {
		t.Fatal(err)
	}
	env := doctorEnvWindow(t, claudeRoot, projectDir, w)
	if !strings.Contains(doctorWarnText(env), "cannot window a static config scan") {
		t.Errorf("an active window must be named as inapplicable: %v", env.Warnings)
	}
	if env.Corpus.Since != "" || env.Corpus.Until != "" {
		t.Errorf("since/until = %q/%q, want empty — the run was not windowed", env.Corpus.Since, env.Corpus.Until)
	}
	if got := metricValue(t, env, "skills_checked", "corpus", ""); got != int64(1) {
		t.Errorf("the window must not gate static rows: skills_checked = %v", got)
	}
}

// TestDoctorEnvelopeContractAndDeterminism is the standing gate for the new
// envelope: command named, every row measured (nothing estimated, so no
// Method is needed), Validate passes, and four runs over one unchanged tree
// produce byte-identical JSON — the TestReportReproducible discipline applied
// here while `report` does not yet merge doctor.
func TestDoctorEnvelopeContractAndDeterminism(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorSkill(t, filepath.Join(claudeRoot, "skills"), "alpha", "# no frontmatter\n")
	doctorSkill(t, filepath.Join(claudeRoot, "skills"), "beta", "---\nname: gamma\ndescription: d\n---\n")
	doctorSkill(t, filepath.Join(projectDir, ".agents", "skills"), "agented", "---\nname: agented\n---\n")
	doctorWrite(t, filepath.Join(claudeRoot, "plugins", "broken", ".claude-plugin", "plugin.json"), `oops{`)
	doctorWrite(t, filepath.Join(claudeRoot, "settings.json"),
		`{"mcpServers":{"gone":{"command":"totally-missing-binary-xyz"},"dup":{"type":"nope"}}}`)
	doctorWrite(t, filepath.Join(projectDir, ".mcp.json"),
		`{"mcpServers":{"dup":{"command":"`+doctorBinName+`"}}}`)

	var first []byte
	for run := range 4 {
		env := doctorEnv(t, claudeRoot, projectDir)
		if env.Command != "doctor" {
			t.Errorf("command = %q, want doctor", env.Command)
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
		if bytes.Contains(buf.Bytes(), []byte("true")) || bytes.Contains(buf.Bytes(), []byte("false")) {
			t.Error("a boolean reached the JSON — doctor rows are integers only")
		}
		if run == 0 {
			first = bytes.Clone(buf.Bytes())
			continue
		}
		if !bytes.Equal(first, buf.Bytes()) {
			t.Fatalf("run %d produced different JSON over an unchanged tree", run)
		}
	}

	// Spot-check the collision survived into this shared fixture: dup resolves
	// to the project's working command, so only `gone` fails, and the shadowed
	// user-side type "nope" earns no finding.
	env := doctorEnv(t, claudeRoot, projectDir)
	if got := metricOrNil(env, "findings", "mcp_server", "dup"); got != nil {
		t.Errorf("the winning project entry was judged by the losing user type: %v", got)
	}
	if got := metricOrNil(env, "findings", "mcp_server", "gone"); got != int64(1) {
		t.Errorf("findings/mcp_server/gone = %v, want 1", got)
	}
}

// TestDoctorJSONRoundTripsThroughValidate mirrors the failures gate: encoded
// and decoded back, the envelope still carries every row through the
// contract, so `--json` and the future table cannot disagree about what
// exists.
func TestDoctorJSONRoundTripsThroughValidate(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorSkill(t, filepath.Join(claudeRoot, "skills"), "bad", "---\nname: worse\ndescription: d\n---\n")
	env := doctorEnv(t, claudeRoot, projectDir)
	var buf bytes.Buffer
	if err := WriteJSON(&buf, env); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var back Envelope
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("decoded envelope: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("decoded Validate: %v", err)
	}
	if len(back.Metrics) != len(env.Metrics) {
		t.Errorf("decoded %d metrics, envelope has %d", len(back.Metrics), len(env.Metrics))
	}
	if got, want := len(dimRows(back, "skill")), 1; got != want {
		t.Errorf("decoded skill rows = %d, want %d", got, want)
	}
}

// The DR-2 join fixtures mirror the failures ones: session ids and tool names
// are all the observation needs (mcp__<server>__<tool> or an
// attributionMcpServer value), so results stay out unless a test reads them.

// TestDoctorJoinNeverAndUnconfigured is the join's headline case: a clean
// server configured and never called earns never_observed against the
// session denominator; the ghost in a tool name and the server named only by
// an attribution field are both observed-but-unconfigured misses.
func TestDoctorJoinNeverAndUnconfigured(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(claudeRoot, "settings.json"),
		`{"mcpServers":{"unused_server":{"command":"`+doctorBinName+`"}}}`)
	corpus := toolCorpus(t,
		useIn(tsAt(1), "s1", "u1", "mcp__ghost__ping", cmdLS, ""),
		useIn(tsAt(2), "s2", "u2", "Bash", cmdLS, `"attributionMcpServer":"attrd",`),
	)
	env := doctorJoinEnv(t, claudeRoot, projectDir, corpus, Window{})

	if got := metricValue(t, env, doctorMCPSessions, "corpus", ""); got != int64(2) {
		t.Errorf("mcp_sessions = %v, want 2", got)
	}
	// never_observed's value is the denominator: two mcp-using sessions
	// named nothing of this server.
	if got := metricValue(t, env, doctorNeverObserved, "mcp_server", "unused_server"); got != int64(2) {
		t.Errorf("never_observed/unused_server = %v, want 2", got)
	}
	if got := metricValue(t, env, doctorUnconfiguredObserved, "mcp_server", "ghost"); got != int64(1) {
		t.Errorf("unconfigured_observed/ghost = %v, want 1", got)
	}
	if got := metricValue(t, env, doctorUnconfiguredObserved, "mcp_server", "attrd"); got != int64(1) {
		t.Errorf("unconfigured_observed/attrd = %v, want 1 — the attribution field is an observation", got)
	}
	if got := metricOrNil(env, doctorNeverObserved, "mcp_server", "ghost"); got != nil {
		t.Errorf("an observed server earned never_observed: %v", got)
	}
	if got := metricOrNil(env, doctorUnconfiguredObserved, "mcp_server", "unused_server"); got != nil {
		t.Errorf("a configured server earned unconfigured_observed: %v", got)
	}
	for _, m := range dimRows(env, "mcp_server") {
		if m.Name == "findings" {
			t.Errorf("a clean config emitted a findings row into the join dimension: %+v", m)
		}
		if m.Derivation != Measured {
			t.Errorf("join row %s/%s is %s, want measured", m.Name, m.Key, m.Derivation)
		}
	}
	w := doctorWarnText(env)
	for _, want := range []string{
		`mcp_server "unused_server": configured but never seen in the corpus — 2 session(s)`,
		"absence here is not a claim the server is broken",
		`mcp_server "ghost": observed in the corpus (1 mcp-using session(s))`,
		`mcp_server "attrd": observed in the corpus`,
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warn text is missing %q:\n%s", want, w)
		}
	}
}

// TestDoctorJoinWindowFiltersSessions pins that the corpus scan respects
// --since: sessions entirely outside the window leave the denominator, the
// rows' values and the observed set, while every static row stays
// point-in-time beside the warning that says so.
func TestDoctorJoinWindowFiltersSessions(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(claudeRoot, "settings.json"),
		`{"mcpServers":{"unused_server":{"command":"`+doctorBinName+`"}}}`)
	doctorSkill(t, filepath.Join(claudeRoot, "skills"), "ok", strings.ReplaceAll(doctorGoodSkill, "%s", "ok"))
	corpus := toolCorpus(t,
		useIn("2026-08-10T10:00:00.000Z", "s-old", "u1", "mcp__ghost__ping", cmdLS, ""),
		useIn("2026-09-10T10:00:00.000Z", "s-new", "u2", "mcp__ghost__ping", cmdLS, ""),
	)

	full := doctorJoinEnv(t, claudeRoot, projectDir, corpus, Window{})
	if got := metricValue(t, full, doctorMCPSessions, "corpus", ""); got != int64(2) {
		t.Errorf("unwindowed mcp_sessions = %v, want 2", got)
	}
	if got := metricValue(t, full, doctorNeverObserved, "mcp_server", "unused_server"); got != int64(2) {
		t.Errorf("unwindowed never_observed = %v, want 2", got)
	}

	w, err := NewWindow("2026-09-01", "")
	if err != nil {
		t.Fatal(err)
	}
	env := doctorJoinEnv(t, claudeRoot, projectDir, corpus, w)
	if got := metricValue(t, env, doctorMCPSessions, "corpus", ""); got != int64(1) {
		t.Errorf("windowed mcp_sessions = %v, want 1 — the August session left the denominator", got)
	}
	if got := metricValue(t, env, doctorNeverObserved, "mcp_server", "unused_server"); got != int64(1) {
		t.Errorf("windowed never_observed = %v, want 1", got)
	}
	if got := metricValue(t, env, doctorUnconfiguredObserved, "mcp_server", "ghost"); got != int64(1) {
		t.Errorf("windowed unconfigured_observed/ghost = %v, want 1", got)
	}
	if env.Corpus.Since != "2026-09-01" {
		t.Errorf("corpus since = %q, want the window — the join ran windowed", env.Corpus.Since)
	}
	if got := metricValue(t, env, "skills_checked", "corpus", ""); got != int64(1) {
		t.Errorf("the window must not gate static rows: skills_checked = %v", got)
	}
	warn := doctorWarnText(env)
	if !strings.Contains(warn, "point-in-time") {
		t.Errorf("a windowed join must warn that the static checks are not windowed:\n%s", warn)
	}
	if !strings.Contains(warn, "this run measures only events with") {
		t.Errorf("the joined envelope must carry the standard windowed C1 warning:\n%s", warn)
	}
}

// TestDoctorJoinZeroMCPCorpus keeps the DR-1 posture on an empty denominator:
// a corpus with no mcp activity earns no never_observed rows — an absence
// measured against zero sessions is the over-claim Validate cannot catch —
// and one warning says why.
func TestDoctorJoinZeroMCPCorpus(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(claudeRoot, "settings.json"),
		`{"mcpServers":{"quiet":{"command":"`+doctorBinName+`"}}}`)
	corpus := toolCorpus(t,
		useIn(tsAt(1), "s1", "u1", "Bash", cmdLS, ""), okRes(tsAt(2), "s1", "u1"),
	)
	env := doctorJoinEnv(t, claudeRoot, projectDir, corpus, Window{})

	if got := metricOrNil(env, doctorNeverObserved, "mcp_server", "quiet"); got != nil {
		t.Errorf("never_observed claimed against a zero denominator: %v", got)
	}
	if got := metricValue(t, env, doctorMCPSessions, "corpus", ""); got != int64(0) {
		t.Errorf("mcp_sessions = %v, want a measured 0 — the corpus was streamed", got)
	}
	if !strings.Contains(doctorWarnText(env), "never_observed is not claimed") {
		t.Errorf("the withheld rows need their denominator warning:\n%s", doctorWarnText(env))
	}
}

// TestDoctorJoinPartialConfigCaveat pins the floor guard: a configured set
// read off a half-broken config may misjudge an observed server as
// unconfigured, so the miss keeps its row and gains its caveat.
func TestDoctorJoinPartialConfigCaveat(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(claudeRoot, "settings.json"), `{"mcpServers": {`)
	corpus := toolCorpus(t, useIn(tsAt(1), "s1", "u1", "mcp__ghost__ping", cmdLS, ""))
	env := doctorJoinEnv(t, claudeRoot, projectDir, corpus, Window{})

	if got := metricValue(t, env, doctorUnconfiguredObserved, "mcp_server", "ghost"); got != int64(1) {
		t.Errorf("a loud join miss stays a row over a partial config: %v", got)
	}
	if !strings.Contains(doctorWarnText(env), "configured set is partial") {
		t.Errorf("the partial-config caveat is missing:\n%s", doctorWarnText(env))
	}
}

// TestDoctorJoinMissingCorpusIsFatal pins the CLI posture the task draws: a
// config root missing degrades (DR-1), a transcript corpus missing is the
// same fatal scan error every other corpus command returns.
func TestDoctorJoinMissingCorpusIsFatal(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	if _, err := DoctorJoinEnvelope(claudeRoot, projectDir, filepath.Join(projectDir, "no-corpus"),
		"test", Window{}); err == nil {
		t.Fatal("a missing corpus dir returned nil, want the scan error")
	}
}

// TestDoctorJoinDeterminism is DR-1's four-run byte-equality gate over the
// joined envelope, whose new maps (sessions, perServer) are exactly the kind
// of iteration order that broke an earlier --json.
func TestDoctorJoinDeterminism(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(claudeRoot, "settings.json"), `{"mcpServers":{
		"unused_a":{"command":"`+doctorBinName+`"},"unused_b":{"type":"http"},"ghost":{"type":"sse"}}}`)
	corpus := toolCorpus(t,
		useIn(tsAt(1), "s1", "u1", "mcp__ghost__ping", cmdLS, ""),
		useIn(tsAt(2), "s2", "u2", "mcp__ghost__other", cmdLS, ""),
		useIn(tsAt(3), "s1", "u3", "Bash", cmdLS, `"attributionMcpServer":"attrd",`),
	)
	var first []byte
	for run := range 4 {
		env := doctorJoinEnv(t, claudeRoot, projectDir, corpus, Window{})
		var buf bytes.Buffer
		if err := WriteJSON(&buf, env); err != nil {
			t.Fatalf("WriteJSON: %v", err)
		}
		if run == 0 {
			first = bytes.Clone(buf.Bytes())
			continue
		}
		if !bytes.Equal(first, buf.Bytes()) {
			t.Fatalf("run %d produced different JSON over an unchanged corpus", run)
		}
	}
	// ghost IS configured (with a static finding: sse has no url check, but
	// "unused_a"/"unused_b" must be the never_observed pair and ghost and
	// attrd the unconfigured pair never fires for ghost).
	env := doctorJoinEnv(t, claudeRoot, projectDir, corpus, Window{})
	for _, key := range []string{"unused_a", "unused_b"} {
		if got := metricOrNil(env, doctorNeverObserved, "mcp_server", key); got == nil {
			t.Errorf("%s: configured and never called, want a never_observed row", key)
		}
	}
	if got := metricOrNil(env, doctorUnconfiguredObserved, "mcp_server", "ghost"); got != nil {
		t.Errorf("a configured server reported as unconfigured: %v", got)
	}
	if got := metricValue(t, env, doctorUnconfiguredObserved, "mcp_server", "attrd"); got != int64(1) {
		t.Errorf("unconfigured_observed/attrd = %v, want 1", got)
	}
}

// TestRenderDoctorJoinTable pins the renderer to the envelope: every non-empty
// dimension gets its fixed-order table, every warning prints verbatim, and a
// flushed tabwriter table carries no tabs anywhere.
func TestRenderDoctorJoinTable(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(claudeRoot, "settings.json"),
		`{"mcpServers":{"unused_server":{"command":"`+doctorBinName+`"},"ghost":{"type":"websocket"}}}`)
	doctorWrite(t, filepath.Join(claudeRoot, "plugins", "broken", ".claude-plugin", "plugin.json"), `oops{`)
	doctorSkill(t, filepath.Join(claudeRoot, "skills"), "alpha", "# no frontmatter\n")
	corpus := toolCorpus(t, useIn(tsAt(1), "s1", "u1", "mcp__ghost__ping", cmdLS, ""))
	env := doctorJoinEnv(t, claudeRoot, projectDir, corpus, Window{})

	var buf bytes.Buffer
	if err := RenderDoctor(&buf, env); err != nil {
		t.Fatalf("RenderDoctor: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"tare doctor — " + corpus, "SKILL", "PLUGIN", "MCP_SERVER", "NEVER_OBSERVED",
		"UNCONFIGURED_OBSERVED", "unused_server", "warning: mcp_server ghost: server_type_unknown",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\t") {
		t.Errorf("a flushed tabwriter table must carry no tabs:\n%q", out)
	}
	// The ghost row carries a static finding AND an observation, never a
	// never_observed: it was seen.
	if strings.Contains(out, "no configuration issues detected") {
		t.Errorf("a tree with findings and misses declared itself clean:\n%s", out)
	}
	// The joined mcp_server dimension is one table: the static finding and
	// the join miss share the key's row, not the header.
	if strings.Count(out, "MCP_SERVER") != 1 {
		t.Errorf("MCP_SERVER table printed more than once:\n%s", out)
	}
}

// TestRenderDoctorCleanTree is the zero-findings, zero-miss line. The corpus
// here has no mcp activity at all, so the never_observed suppression warning
// must still print beside the line: the line claims no detected issue, not a
// fully measured join.
func TestRenderDoctorCleanTree(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorSkill(t, filepath.Join(claudeRoot, "skills"), "greet",
		strings.ReplaceAll(doctorGoodSkill, "%s", "greet"))
	doctorWrite(t, filepath.Join(claudeRoot, "plugins", "tare-p", ".claude-plugin", "plugin.json"),
		`{"name":"tare-p"}`)
	doctorWrite(t, filepath.Join(claudeRoot, "settings.json"),
		`{"mcpServers":{"idle":{"command":"`+doctorBinName+`"}}}`)
	env := doctorJoinEnv(t, claudeRoot, projectDir, t.TempDir(), Window{})

	var buf bytes.Buffer
	if err := RenderDoctor(&buf, env); err != nil {
		t.Fatalf("RenderDoctor: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "no configuration issues detected") {
		t.Errorf("a clean tree must print the one-line all-clear:\n%s", out)
	}
	for _, want := range []string{"SKILL", "PLUGIN", "MCP_SERVER"} {
		if strings.Contains(out, "\n"+want+"\t") {
			t.Errorf("an empty %s dimension printed a table:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "warning: mcp: no mcp tool call was seen") {
		t.Errorf("the suppressed never_observed rows need their warning under the line:\n%s", out)
	}
}

// doctorClaudeJSON is the store path doctorMCP derives from claudeRoot's
// parent — ~/.claude.json beside ~/.claude — mirrored here so the fixtures
// write exactly where the pass reads.
func doctorClaudeJSON(claudeRoot string) string {
	return filepath.Join(filepath.Dir(claudeRoot), ".claude.json")
}

// TestDoctorClaudeJSONUserScope is the flagship fix: a server configured only
// in ~/.claude.json's top-level mcpServers joins the configured set, so
// never_observed fires for it against the session denominator instead of the
// store being invisible and the server surfacing as nothing at all.
func TestDoctorClaudeJSONUserScope(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, filepath.Join(claudeRoot, "settings.json"),
		`{"mcpServers":{"used":{"command":"`+doctorBinName+`"}}}`)
	doctorWrite(t, doctorClaudeJSON(claudeRoot), `{"mcpServers":{
		"store_idle":{"command":"`+doctorBinName+`"},"used":{"command":"`+doctorBinName+`"}}}`)
	corpus := toolCorpus(t, useIn(tsAt(1), "s1", "u1", "mcp__used__ping", cmdLS, ""))
	env := doctorJoinEnv(t, claudeRoot, projectDir, corpus, Window{})

	// "used" collides user-scope across two files and counts once.
	if got := metricValue(t, env, "mcp_servers_checked", "corpus", ""); got != int64(2) {
		t.Errorf("mcp_servers_checked = %v, want 2 — the store joins the set, collisions count once", got)
	}
	if got := metricValue(t, env, doctorMCPSessions, "corpus", ""); got != int64(1) {
		t.Errorf("mcp_sessions = %v, want 1", got)
	}
	if got := metricValue(t, env, doctorNeverObserved, "mcp_server", "store_idle"); got != int64(1) {
		t.Errorf("never_observed/store_idle = %v, want 1 — the store server joins the denominator", got)
	}
	if got := metricOrNil(env, doctorNeverObserved, "mcp_server", "used"); got != nil {
		t.Errorf("the called server earned never_observed: %v", got)
	}
	if got := metricOrNil(env, doctorUnconfiguredObserved, "mcp_server", "store_idle"); got != nil {
		t.Errorf("a configured store server reported unconfigured: %v", got)
	}
	w := doctorWarnText(env)
	if !strings.Contains(w, `mcp_server "store_idle": configured but never seen in the corpus — 1 session(s)`) {
		t.Errorf("the store server's denominator warn is missing:\n%s", w)
	}
	if !strings.Contains(w, `overrides the one in `+filepath.Join(claudeRoot, "settings.json")+
		" (later source wins on name collision within the user scope)") {
		t.Errorf("the same-scope collision must be said out loud:\n%s", w)
	}
}

// TestDoctorClaudeJSONProjectOverride pins the scope precedence across the new
// source: the store's projects["<abs projectDir>"] entry is project-scoped, so
// it wins over .mcp.json's same-named entry — and the findings pass judges the
// winner only, so the loser's broken command earns no finding.
func TestDoctorClaudeJSONProjectOverride(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	// No settings.json: the store also carries a user-scope entry, shadowed
	// here by the project-scoped one under the same name.
	doctorWrite(t, doctorClaudeJSON(claudeRoot), `{"mcpServers":{
		"shared":{"command":"totally-missing-binary-xyz"}},
		"projects":{"`+projectDir+`":{"mcpServers":{
			"shared":{"command":"`+doctorBinName+`"}}}}}`)
	doctorWrite(t, filepath.Join(projectDir, ".mcp.json"),
		`{"mcpServers":{"shared":{"command":"totally-missing-binary-xyz"}}}`)

	env := doctorEnv(t, claudeRoot, projectDir)
	if got := metricOrNil(env, "findings", "mcp_server", "shared"); got != nil {
		t.Errorf("a losing entry was judged: %v", got)
	}
	if got := metricValue(t, env, "mcp_servers_checked", "corpus", ""); got != int64(1) {
		t.Errorf("mcp_servers_checked = %v, want 1 across three same-named entries", got)
	}
	w := doctorWarnText(env)
	mcp := filepath.Join(projectDir, ".mcp.json")
	store := doctorClaudeJSON(claudeRoot) + ` projects["` + projectDir + `"]`
	if !strings.Contains(w, `mcp server "shared": the entry in `+store+" overrides the one in "+mcp+
		" (later source wins on name collision within the project scope)") {
		t.Errorf("the store's project section must win over .mcp.json:\n%s", w)
	}
	if !strings.Contains(w, "overrides the one in "+doctorClaudeJSON(claudeRoot)+
		" (project config wins on name collision)") {
		t.Errorf("the store's user section must lose to .mcp.json under the DR-1 rule:\n%s", w)
	}
}

// TestDoctorClaudeJSONBroken keeps the absent-is-not-zero contract on the new
// source: an unparseable store withholds the denominator with a floor-naming
// warn and, per DR-1's standing posture, rides the withheld count with the
// findings unjudged — while the other blocks stand untouched.
func TestDoctorClaudeJSONBroken(t *testing.T) {
	claudeRoot, projectDir, _ := doctorPaths(t)
	doctorWrite(t, doctorClaudeJSON(claudeRoot), `{"mcpServers": {`)
	doctorWrite(t, filepath.Join(claudeRoot, "settings.json"),
		`{"mcpServers":{"weird":{"type":"websocket"}}}`)
	doctorSkill(t, filepath.Join(claudeRoot, "skills"), "ok", strings.ReplaceAll(doctorGoodSkill, "%s", "ok"))

	env := doctorEnv(t, claudeRoot, projectDir)
	if got := metricOrNil(env, "mcp_servers_checked", "corpus", ""); got != nil {
		t.Errorf("mcp_servers_checked = %v, want omitted over a floor", got)
	}
	w := doctorWarnText(env)
	if !strings.Contains(w, doctorClaudeJSON(claudeRoot)) || !strings.Contains(w, "not valid JSON") {
		t.Errorf("the broken store must be named: %s", w)
	}
	if !strings.Contains(w, "never_observed is then a floor, not a verdict") {
		t.Errorf("the store warn must name its effect on the join:\n%s", w)
	}
	// DR-1's standing posture on a broken source: no entries are judged, so
	// the keyed findings ride the withheld count instead of half-reporting.
	if got := metricOrNil(env, "findings", "mcp_server", "weird"); got != nil {
		t.Errorf("findings were judged over a broken source: %v", got)
	}
	if got := metricValue(t, env, "skills_checked", "corpus", ""); got != int64(1) {
		t.Errorf("the store must not take the skills block down: %v", got)
	}
}

// TestDoctorClaudeJSONEmpty is the measured-zero twin: a store that parses and
// names no servers is configuration that was read and says none — it alone
// turns the count from an absence into a real zero, with no phantom warn.
func TestDoctorClaudeJSONEmpty(t *testing.T) {
	t.Run("store alone is a measured zero", func(t *testing.T) {
		claudeRoot, projectDir, _ := doctorPaths(t)
		doctorWrite(t, doctorClaudeJSON(claudeRoot), `{"projects":{"/some/other":{"mcpServers":{"far":{"type":"http"}}}}}`)
		env := doctorEnv(t, claudeRoot, projectDir)
		if got := metricValue(t, env, "mcp_servers_checked", "corpus", ""); got != int64(0) {
			t.Errorf("mcp_servers_checked = %v, want a measured 0", got)
		}
		if got := metricOrNil(env, "findings", "mcp_server", "far"); got != nil {
			t.Errorf("another project's server leaked into this run's set: %v", got)
		}
	})
	t.Run("empty store adds nothing beside settings.json", func(t *testing.T) {
		claudeRoot, projectDir, _ := doctorPaths(t)
		doctorWrite(t, doctorClaudeJSON(claudeRoot), `{}`)
		doctorWrite(t, filepath.Join(claudeRoot, "settings.json"),
			`{"mcpServers":{"solo":{"command":"`+doctorBinName+`"}}}`)
		env := doctorEnv(t, claudeRoot, projectDir)
		if got := metricValue(t, env, "mcp_servers_checked", "corpus", ""); got != int64(1) {
			t.Errorf("mcp_servers_checked = %v, want 1", got)
		}
		if n := strings.Count(doctorWarnText(env), "mcp:"); n != 0 {
			t.Errorf("a valid empty store must earn no mcp warn:\n%s", doctorWarnText(env))
		}
	})
}
