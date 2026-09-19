package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/AngelVRodC/tare/internal/transcript"
)

// Skill-finding kinds. The names are DR-2 renderer contract: the warn line
// prints the kind verbatim and the keyed `findings` row counts it, so the set
// is fixed here and consumed by name there.
const (
	doctorFrontmatterMissing      = "frontmatter_missing"
	doctorFrontmatterUnterminated = "frontmatter_unterminated"
	doctorNameMismatch            = "name_mismatch"
	doctorDescriptionMissing      = "description_missing"
	doctorAllowedToolsMalformed   = "allowed_tools_malformed"
)

// Plugin-finding kinds.
const (
	doctorPluginJSONInvalid  = "plugin_json_invalid"
	doctorPluginNameMismatch = "plugin_name_mismatch"
)

// MCP-server finding kinds. `command_not_found` measures the PATH of the
// running tare process, not the environment the harness launches servers
// with — the warn text says so, because that scope is a caveat on the
// reading, not a number to emit.
const (
	doctorServerTypeUnknown = "server_type_unknown"
	doctorCommandMissing    = "command_missing"
	doctorCommandNotFound   = "command_not_found"
)

// doctorFinding is one static-config defect: its kind and a human-readable
// detail. The kind is the machine half (it lands in the warn line); the
// detail explains which value said it.
type doctorFinding struct {
	kind, detail string
}

// doctorToolToken is one token of a plain allowed-tools list: a tool name
// (word chars, dots, slashes, dashes — `Read`, `WebFetch`, `mcp/github`)
// optionally followed by a parenthesised specifier (`Bash(go:*)`). Anything
// else — `&&`, `;`, an unbalanced paren — is allowed_tools_malformed.
var doctorToolToken = regexp.MustCompile(`^[A-Za-z0-9_./-]+(\([^()]*\))?$`)

// DoctorEnvelope reports the *static* health of the installed harness
// configuration: skill frontmatter, plugin manifests, and MCP server entries
// — read from disk, never from the transcript corpus. It answers "is the
// configuration the harness will load actually loadable", which no
// transcript-reading command can see.
//
// claudeRoot is the user's ~/.claude equivalent and projectDir the repo being
// inspected; both are parameters so tests build fixture trees under
// t.TempDir() and the command never needs the real HOME. The envelope's
// corpus dir names claudeRoot — it is the harness root the checks center on.
//
// Every pass is tolerant of absence: a missing directory or file costs that
// block (its `_checked` row omitted, not zeroed) and one warning, never an
// error. Only both roots being unreadable at once is an error — then there is
// nothing left to report, the same line the OpenCode reader draws for its
// database. A config file or directory that exists but cannot be read degrades
// its block to omitted too: a count missing the unreadable source's entries
// would be a floor passed off as a total, and `measured` is exactly the tag
// Envelope.Validate accepts unconditionally.
//
// The window cannot apply: configuration carries no event timestamps, so an
// active --since/--until is named in a warning and every row stays
// full-configuration. That is also why newBuilder is handed a zero Window —
// done()'s C1 warning speaks about events, and doctor reads none; the
// envelope's since/until header fields stay empty because this run genuinely
// measured against no window.
//
// Every number here is read straight off the files, so every row is
// `measured` and nothing needs a Method. A findings rollup would be a second
// row saying what the keyed rows already say, so the human-readable finding
// list lives in warnings and each key carries only its own count.
func DoctorEnvelope(claudeRoot, projectDir, version string, w Window) (Envelope, error) {
	claudeErr := statRoot(claudeRoot)
	projectErr := statRoot(projectDir)
	if claudeErr != nil && projectErr != nil {
		return Envelope{}, fmt.Errorf(
			"doctor can read neither the harness root %s (%w) nor the project root %s (%w)",
			claudeRoot, claudeErr, projectDir, projectErr)
	}

	b := newBuilder(claudeRoot, version, "doctor", Window{})
	if w.Active() {
		b.warn("--since/--until cannot window a static config scan: configuration carries no event "+
			"timestamps, so every doctor row is full-configuration (window given: %s..%s)",
			w.Since(), w.Until())
	}

	// One reader counts every config file actually read, so the corpus header
	// says what this run consumed instead of claiming an empty transcript.
	rd := &doctorReader{}

	skills, skillsCountable, skillBlock := rd.doctorSkills(claudeRoot, projectDir)
	plugins, pluginsCountable, pluginBlock := rd.doctorPlugins(claudeRoot)
	servers, serversCountable, mcpBlock := rd.doctorMCP(claudeRoot, projectDir)

	emit := func(block doctorBlock) {
		for _, warn := range block.warns {
			b.warn("%s", warn)
		}
		// Keys ascending — a total order, so the row order and the warn order
		// are byte-identical run to run over one tree (the map the findings
		// came from is never ranged directly).
		for _, key := range slices.Sorted(maps.Keys(block.findings)) {
			b.rows(MeasuredMetric("findings", block.dimension, key,
				int64(len(block.findings[key])), "findings"))
			for _, f := range block.findings[key] {
				b.warn("%s %s: %s — %s", block.dimension, key, f.kind, f.detail)
			}
		}
	}

	// The checked rows are the blocks' denominators, emitted in pass order
	// (skill, plugin, mcp) before the keyed findings they cover. A zero is
	// honest when the block WAS readable — the directory was walked and held
	// nothing, which is a measurement, not an absence — and the countable
	// flag withholds it, with the block's own warn saying why, whenever a
	// source was missing or unreadable and no number can be claimed.
	if skillsCountable {
		b.add("skills_checked", int64(skills), "skills")
	}
	if pluginsCountable {
		b.add("plugins_checked", int64(plugins), "plugins")
	}
	if serversCountable {
		b.add("mcp_servers_checked", int64(servers), "servers")
	}
	emit(skillBlock)
	emit(pluginBlock)
	emit(mcpBlock)

	return b.done(transcript.ScanStats{Files: rd.files, Bytes: rd.bytes}), nil
}

// statRoot reports an error when path cannot be inspected at all. One root
// failing is tolerated (a machine with no ~/.claude still has a project to
// check); both failing is the doctor's only hard error.
func statRoot(path string) error {
	_, err := os.Stat(path)
	return err
}

// doctorBlock is one pass's findings, keyed the way the pass keys them, plus
// the block-level warnings (an omission, a collision) that precede them.
type doctorBlock struct {
	dimension string
	findings  map[string][]doctorFinding
	warns     []string
}

// doctorReader counts the files it reads, which becomes the envelope's corpus
// size: doctor's corpus is exactly these config files.
type doctorReader struct {
	files int
	bytes int64
}

func (r *doctorReader) read(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		r.files++
		r.bytes += int64(len(raw))
	}
	return raw, err
}

// doctorSkills walks every skills root one level deep — <root>/<name>/SKILL.md
// — across the user root and the two project roots. A directory without a
// SKILL.md is not a skill (it may be a wrapper holding nested ones) and is
// skipped without a finding: tare's posture is silence on the unknown, not a
// complaint about a layout it does not recognise.
//
// ponytail: one level only, and symlinked dirs are not followed (a
// ReadDir entry for a symlink reports ModeSymlink, not IsDir). Upgrade path:
// filepath.WalkDir with a depth cap and os.Stat on symlinks, if real
// install layouts start nesting deeper than <name>/SKILL.md.
func (r *doctorReader) doctorSkills(claudeRoot, projectDir string) (count int, countable bool, block doctorBlock) {
	block.dimension = "skill"
	block.findings = map[string][]doctorFinding{}
	roots := []string{
		filepath.Join(claudeRoot, "skills"),
		filepath.Join(projectDir, ".claude", "skills"),
		filepath.Join(projectDir, ".agents", "skills"),
	}
	present := 0
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue // a project without .agents/skills is normal, not a defect
		case err != nil:
			// Exists but unlistable: the count would be a floor, so the whole
			// block's count is withheld and said out loud.
			block.warns = append(block.warns, fmt.Sprintf(
				"skills: %s is not readable (%v) — skills_checked is omitted, not zero", root, err))
			return count, false, block
		}
		present++
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			key := filepath.Join(root, entry.Name())
			raw, err := r.read(filepath.Join(key, "SKILL.md"))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				block.warns = append(block.warns, fmt.Sprintf(
					"skills: %s/SKILL.md is not readable (%v) — skills_checked is omitted, not zero", key, err))
				return count, false, block
			}
			count++
			if finds := doctorCheckSkill(entry.Name(), raw); len(finds) > 0 {
				block.findings[key] = finds
			}
		}
	}
	if present == 0 {
		block.warns = append(block.warns, fmt.Sprintf(
			"skills: none of %s exists — skills_checked is omitted, not zero", strings.Join(roots, ", ")))
		return count, false, block
	}
	return count, true, block
}

// doctorCheckSkill runs the four frontmatter rules over one SKILL.md. The
// findings arrive in check order, which is fixed, so one file produces a
// byte-identical warn run to run.
func doctorCheckSkill(dirName string, raw []byte) []doctorFinding {
	fields, missing, unterminated := doctorParseFrontmatter(raw)
	switch {
	case missing:
		return []doctorFinding{{doctorFrontmatterMissing,
			"SKILL.md does not open with a `---` frontmatter block"}}
	case unterminated:
		return []doctorFinding{{doctorFrontmatterUnterminated,
			"the `---` frontmatter block is never closed; its fields cannot be trusted"}}
	}
	var findings []doctorFinding
	name, named := fields["name"]
	switch {
	case !named:
		findings = append(findings, doctorFinding{doctorNameMismatch,
			fmt.Sprintf("frontmatter carries no name, so it cannot match the directory name %q "+
				"(a name mismatch is the documented silent skill-failure cause)", dirName)})
	case name != dirName:
		findings = append(findings, doctorFinding{doctorNameMismatch,
			fmt.Sprintf("frontmatter name %q does not match the directory name %q (the documented silent skill-failure cause)",
				name, dirName)})
	}
	// A description key present but empty is missing: an empty description is
	// exactly the trigger-shape that never fires, so absence-of-content and
	// absence-of-key must not be different findings.
	if strings.TrimSpace(fields["description"]) == "" {
		findings = append(findings, doctorFinding{doctorDescriptionMissing,
			"frontmatter carries no description"})
	}
	if v, ok := fields["allowed-tools"]; ok && doctorAllowedToolsBad(v) {
		findings = append(findings, doctorFinding{doctorAllowedToolsMalformed,
			fmt.Sprintf("allowed-tools %q is neither a bracketed list nor a plain list of Tool or Tool(spec) tokens", v)})
	}
	return findings
}

// doctorParseFrontmatter extracts the leading `---` … `---` block of a
// SKILL.md as flat top-level `key: value` pairs, values unquoted one layer.
// A missing opening fence, an unclosed block, and the parsed fields are the
// three answers the caller needs; a nested or blank key is skipped, not an
// error — the unknown is tolerated, only the join is loud.
//
// ponytail: line-based `key: value` only, first occurrence wins, CRLF
// tolerated. Ceiling: block scalars (`description: >` + indented lines),
// multiline quoted values and nested maps read as absent or empty. Upgrade
// path is a real YAML parser, which the stdlib has no equivalent of — the
// key: subset is the sanctioned line (see the task), because a dependency
// would make tare argue against its own zero-dependency claim.
func doctorParseFrontmatter(raw []byte) (fields map[string]string, missing, unterminated bool) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	fields = map[string]string{}
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return fields, true, false
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return fields, false, true
	}
	for _, line := range lines[1:end] {
		if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") ||
			strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if _, seen := fields[key]; !seen {
			fields[key] = doctorUnquote(strings.TrimSpace(value))
		}
	}
	return fields, false, false
}

// doctorUnquote strips one layer of matching surrounding quotes, which is how
// YAML spellings like description: "…" arrive at a line-based parser.
func doctorUnquote(v string) string {
	for _, q := range []string{`"`, "'"} {
		if len(v) >= 2 && strings.HasPrefix(v, q) && strings.HasSuffix(v, q) {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// doctorAllowedToolsBad accepts the two shapes harnesses document — a
// bracketed flow list `[...]` (contents unchecked, the ceiling the parser
// already declared) and a plain comma/space-separated list of doctorToolToken
// words. An empty value, `[]` and a commas-only value are absent, not
// malformed: absence earns no finding at all.
func doctorAllowedToolsBad(v string) bool {
	v = doctorUnquote(strings.TrimSpace(v))
	if v == "" {
		return false
	}
	if strings.HasPrefix(v, "[") {
		return !strings.HasSuffix(v, "]")
	}
	tokens := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	return slices.ContainsFunc(tokens, func(t string) bool { return !doctorToolToken.MatchString(t) })
}

// doctorPlugins walks <claudeRoot>/plugins one level deep. A plugin directory
// without .claude-plugin/plugin.json earns no finding — the manifest being
// absent is a layout tare does not judge; it being unparseable is a defect.
func (r *doctorReader) doctorPlugins(claudeRoot string) (count int, countable bool, block doctorBlock) {
	block.dimension = "plugin"
	block.findings = map[string][]doctorFinding{}
	root := filepath.Join(claudeRoot, "plugins")
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		block.warns = append(block.warns, fmt.Sprintf(
			"plugins: %s does not exist — plugins_checked is omitted, not zero", root))
		return count, false, block
	}
	if err != nil {
		block.warns = append(block.warns, fmt.Sprintf(
			"plugins: %s is not readable (%v) — plugins_checked is omitted, not zero", root, err))
		return count, false, block
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		count++
		key := entry.Name()
		manifest := filepath.Join(root, key, ".claude-plugin", "plugin.json")
		raw, err := r.read(manifest)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			// Same rule as the skills pass: a manifest that exists but cannot
			// be read makes any count of this block a floor.
			block.warns = append(block.warns, fmt.Sprintf(
				"plugins: %s is not readable (%v) — plugins_checked is omitted, not zero", manifest, err))
			return count, false, block
		}
		var meta struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			block.findings[key] = append(block.findings[key], doctorFinding{doctorPluginJSONInvalid,
				fmt.Sprintf("%s does not parse: %v", manifest, err)})
			continue
		}
		if meta.Name != key {
			detail := fmt.Sprintf("manifest name %q does not match the directory name %q", meta.Name, key)
			if meta.Name == "" {
				detail = fmt.Sprintf("manifest carries no name, so it cannot match the directory name %q", key)
			}
			block.findings[key] = append(block.findings[key], doctorFinding{doctorPluginNameMismatch, detail})
		}
	}
	return count, true, block
}

// doctorMCP merges the mcpServers maps of <claudeRoot>/settings.json and
// <projectDir>/.mcp.json — project entries win a name collision, and the
// collision is warned because the shadowed entry is configuration that will
// never load.
func (r *doctorReader) doctorMCP(claudeRoot, projectDir string) (count int, countable bool, block doctorBlock) {
	block.dimension = "mcp_server"
	block.findings = map[string][]doctorFinding{}
	type entry struct {
		raw  json.RawMessage
		from string
	}
	sources := []string{
		filepath.Join(claudeRoot, "settings.json"),
		filepath.Join(projectDir, ".mcp.json"),
	}
	merged := map[string]entry{}
	var collisions []string
	broken, sawSource := false, false
	for _, source := range sources {
		raw, err := r.read(source)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			block.warns = append(block.warns, fmt.Sprintf(
				"mcp: %s is not readable (%v) — mcp_servers_checked is omitted, not zero", source, err))
			broken = true
			continue
		}
		var doc struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			block.warns = append(block.warns, fmt.Sprintf(
				"mcp: %s exists but is not valid JSON (%v) — mcp_servers_checked is omitted, not zero", source, err))
			broken = true
			continue
		}
		sawSource = true
		for _, name := range slices.Sorted(maps.Keys(doc.MCPServers)) {
			if prev, dup := merged[name]; dup {
				collisions = append(collisions, fmt.Sprintf(
					"mcp server %q: the entry in %s overrides the one in %s (project config wins on name collision)",
					name, source, prev.from))
			}
			merged[name] = entry{raw: doc.MCPServers[name], from: source}
		}
	}
	// Collisions first, sorted with the pass: sources were walked user-then-
	// project and names ascending, so this order is fixed by construction.
	block.warns = append(block.warns, collisions...)
	if broken {
		// One unreadable source makes the total unknowable even though the
		// other half parsed; its keyed rows still stand, only the denominator
		// is withheld.
		return len(merged), false, block
	}
	if !sawSource {
		// A source that exists and parses but names no servers is a measured
		// zero — the config was read and it says none. Only when there is no
		// config to read at all is the count an absence.
		block.warns = append(block.warns, fmt.Sprintf(
			"mcp: neither %s exists — mcp_servers_checked is omitted, not zero",
			strings.Join(sources, " nor ")))
		return 0, false, block
	}
	for _, name := range slices.Sorted(maps.Keys(merged)) {
		if finds := doctorCheckServer(merged[name].raw); len(finds) > 0 {
			block.findings[name] = finds
		}
	}
	return len(merged), true, block
}

// doctorCheckServer judges one mcpServers entry. The harness sniffs its own
// tolerance: absent type means stdio, and http/sse load no local command, so
// the PATH checks belong to stdio servers only. A broken entry shape is
// judged as an unknown type — whatever it is, it is not one of the three the
// harness can load, and the detail says what was actually seen.
func doctorCheckServer(raw json.RawMessage) []doctorFinding {
	var spec struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return []doctorFinding{{doctorServerTypeUnknown,
			fmt.Sprintf("entry is not a JSON object (%v), so no server type can be read", err)}}
	}
	switch spec.Type {
	case "", "stdio":
		command := strings.TrimSpace(spec.Command)
		if command == "" {
			return []doctorFinding{{doctorCommandMissing,
				"a stdio server has no command to launch"}}
		}
		if _, err := exec.LookPath(command); err != nil {
			return []doctorFinding{{doctorCommandNotFound, fmt.Sprintf(
				"command %q does not resolve on the PATH of the running tare process (%v); "+
					"this check measures this process's environment, not the one the harness "+
					"launches servers with", command, err)}}
		}
	case "http", "sse":
	default:
		return []doctorFinding{{doctorServerTypeUnknown,
			fmt.Sprintf("type %q is not one of stdio, http, sse (absent type means stdio)", spec.Type)}}
	}
	return nil
}
