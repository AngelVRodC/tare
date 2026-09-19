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
