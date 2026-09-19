package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"

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

// The corpus cross-join's metric names. DR-2's renderer contract, like the
// finding kinds above: RenderDoctor reads them off the mcp_server dimension
// by name, and `--json` is the envelope these land in.
const (
	// doctorMCPSessions is the join's denominator: in-window sessions that
	// carry any mcp activity at all.
	doctorMCPSessions = "mcp_sessions"
	// doctorNeverObserved is a configured server seen in zero of those
	// sessions; its value is the denominator, because an absence is only a
	// measurement against something.
	doctorNeverObserved = "never_observed"
	// doctorUnconfiguredObserved is a server name the transcripts carry that
	// no config tare reads declares; its value is the sessions that carried
	// it.
	doctorUnconfiguredObserved = "unconfigured_observed"
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
// DoctorJoinEnvelope keeps every check here and additionally streams a
// transcript corpus, which is why its corpus dir names that corpus instead.
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
// The window cannot apply here: configuration carries no event timestamps, so
// an active --since/--until is named in a warning and every row stays
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
	return doctorRun(claudeRoot, projectDir, "", version, w)
}

// DoctorJoinEnvelope is DoctorEnvelope plus the corpus cross-join: the
// configured MCP servers are held against the server names the Claude Code
// transcripts actually observed — `mcp__<server>__<tool>` tool names read off
// via mcpServer, plus every `attributionMcpServer` value — and both join
// misses are reported: configured-but-never-observed and observed-but-
// unconfigured. corpusDir is the transcript root, read-only through
// transcript.Scan like every corpus command; a missing one is fatal, because
// the join measured nothing then. The config roots are unchanged from
// DoctorEnvelope.
func DoctorJoinEnvelope(claudeRoot, projectDir, corpusDir, version string, w Window) (Envelope, error) {
	return doctorRun(claudeRoot, projectDir, corpusDir, version, w)
}

// doctorRun is both constructors: an empty corpusDir is the static envelope
// exactly as DR-1 shipped it (zero-window builder, claudeRoot corpus dir, one
// "cannot window" warning), a set one adds the transcript scan and its join.
func doctorRun(claudeRoot, projectDir, corpusDir, version string, w Window) (Envelope, error) {
	join := corpusDir != ""
	claudeErr := statRoot(claudeRoot)
	projectErr := statRoot(projectDir)
	if claudeErr != nil && projectErr != nil {
		return Envelope{}, fmt.Errorf(
			"doctor can read neither the harness root %s (%w) nor the project root %s (%w)",
			claudeRoot, claudeErr, projectDir, projectErr)
	}

	// Static mode names claudeRoot as the corpus dir and windows nothing;
	// joined mode's corpus header is the transcript corpus, because that is
	// the input whose files, bytes, date range and window bounds the header
	// then truthfully reports. The config roots stay in the finding keys.
	dir, win := claudeRoot, Window{}
	if join {
		dir, win = corpusDir, w
	}
	b := newBuilder(dir, version, "doctor", win)
	if w.Active() && !join {
		b.warn("--since/--until cannot window a static config scan: configuration carries no event "+
			"timestamps, so every doctor row is full-configuration (window given: %s..%s)",
			w.Since(), w.Until())
	}

	// One reader counts every config file actually read, so the static
	// envelope's corpus header says what this run consumed instead of
	// claiming an empty transcript. Under the join the header is the
	// transcript scan's, and the config read count is not claimable as one
	// number beside a dir that holds neither.
	rd := &doctorReader{}

	skills, skillsCountable, skillBlock := rd.doctorSkills(claudeRoot, projectDir)
	plugins, pluginsCountable, pluginBlock := rd.doctorPlugins(claudeRoot)
	servers, serversCountable, configuredServers, mcpBlock := rd.doctorMCP(claudeRoot, projectDir)

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
	emitDoctorBlock(b, skillBlock)
	emitDoctorBlock(b, pluginBlock)
	emitDoctorBlock(b, mcpBlock)

	if !join {
		return b.done(transcript.ScanStats{Files: rd.files, Bytes: rd.bytes}), nil
	}
	return doctorJoin(b, w, corpusDir, configuredServers, mcpBlock.partial)
}

// emitDoctorBlock closes one static pass: its block warnings, then one
// `findings` count per key plus the human sentence for each finding. Keys
// ascending — a total order, so the row order and the warn order are
// byte-identical run to run over one tree (the map the findings came from is
// never ranged directly).
func emitDoctorBlock(b *builder, block doctorBlock) {
	for _, warn := range block.warns {
		b.warn("%s", warn)
	}
	for _, key := range slices.Sorted(maps.Keys(block.findings)) {
		b.rows(MeasuredMetric("findings", block.dimension, key,
			int64(len(block.findings[key])), "findings"))
		for _, f := range block.findings[key] {
			b.warn("%s %s: %s — %s", block.dimension, key, f.kind, f.detail)
		}
	}
}

// doctorJoin streams the corpus and emits the two join misses. Retained state
// is bounded by distinct sessions and distinct server names — the failures
// errSessions set is the precedent — never by lines.
//
// The window gates the observation (an out-of-window tool_use is not seen),
// and both row kinds stay `measured`: measured observation within the window,
// not an estimate. The static half is point-in-time and says so in its own
// warning; done() adds the standard C1 on top because the builder carries the
// real window here.
//
// never_observed's value is the denominator — the in-window sessions carrying
// ANY mcp activity — because the absence only means something against a
// corpus that uses MCP at all. With zero such sessions no row is emitted and
// one warning says why: an absence measured against nothing would be exactly
// the over-claim Envelope.Validate accepts unconditionally.
// unconfigured_observed is the loud join miss (unknown fails quietly, the join
// does not), never fatal, and its detail admits the honest benign cases —
// plugin-provided servers and config scopes doctor does not read land here.
func doctorJoin(b *builder, w Window, corpusDir string, configured []string, configPartial bool) (Envelope, error) {
	if w.Active() {
		b.warn("--since/--until windows the transcript join only: the static config checks are "+
			"point-in-time — configuration carries no event timestamps (window given: %s..%s)",
			w.Since(), w.Until())
	}
	sessions := map[string]bool{}             // any mcp activity, by session
	perServer := map[string]map[string]bool{} // observed server → sessions
	see := func(session, server string) {
		sessions[session] = true
		if perServer[server] == nil {
			perServer[server] = map[string]bool{}
		}
		perServer[server][session] = true
	}

	scanStats, err := transcript.Scan(corpusDir, func(ev *transcript.Event) {
		b.seeTime(ev.Timestamp, ev.Type)
		if !w.Includes(ev.Timestamp) {
			return
		}
		// Both observation channels: the tool name carries the server for a
		// direct call, and attributionMcpServer names it on the turn even
		// where no mcp__-shaped use rides the same event.
		if s := ev.AttributionMcpServer; s != "" {
			see(ev.SessionID, s)
		}
		uses, _ := ev.Blocks()
		for _, u := range uses {
			if s := mcpServer(u.Name); s != "" {
				see(ev.SessionID, s)
			}
		}
	})
	if err != nil {
		return Envelope{}, err
	}

	mcpSessions := int64(len(sessions))
	b.addWin(doctorMCPSessions, mcpSessions, "sessions")

	isConfigured := make(map[string]bool, len(configured))
	for _, name := range configured {
		isConfigured[name] = true
	}
	var never, unconfigured []string
	for _, name := range configured { // already key-ascending from the pass
		if perServer[name] == nil {
			never = append(never, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(perServer)) {
		if !isConfigured[name] {
			unconfigured = append(unconfigured, name)
		}
	}
	if configPartial {
		// One side of the join is a floor, so a miss against it is soft:
		// rows still stand, but the reading has to be warned about.
		b.warn("mcp: a config source was unreadable, so the configured set is partial — " +
			"an unconfigured_observed server may live in the source doctor could not read")
	}
	if mcpSessions == 0 && len(never) > 0 {
		scope := "in the corpus"
		if w.Active() {
			scope = "in the corpus within this window"
		}
		b.warn("mcp: no mcp tool call was seen %s, so never_observed is not claimed — %d configured "+
			"servers would be an absence measured against zero mcp-using sessions, which says nothing",
			scope, len(never))
		never = nil
	}

	// One merged ascending pass over both miss kinds: keys are disjoint by
	// construction (a key is in exactly one list), so the order is total and
	// map iteration never reaches the output.
	misses := append(slices.Clone(never), unconfigured...)
	slices.Sort(misses)
	for _, key := range misses {
		if isConfigured[key] {
			b.rows(MeasuredMetric(doctorNeverObserved, "mcp_server", key, mcpSessions, "sessions"))
			b.warn("mcp_server %q: configured but never seen in the corpus — %d session(s) carry any "+
				"mcp call and none names this server; either it never connected, the harness wrote no "+
				"call for it, or the corpus window predates it; absence here is not a claim the server "+
				"is broken", key, mcpSessions)
			continue
		}
		n := int64(len(perServer[key]))
		b.rows(MeasuredMetric(doctorUnconfiguredObserved, "mcp_server", key, n, "sessions"))
		b.warn("mcp_server %q: observed in the corpus (%d mcp-using session(s)) but named by no config "+
			"tare reads — plugin-provided servers and config scopes doctor does not read arrive here too; "+
			"a join miss, not a claim the server is misconfigured", key, n)
	}
	return b.done(scanStats), nil
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
	// partial says at least one source existed but could not be read or
	// parsed: the configured-name set doctorMCP returns is what survived, not
	// the whole truth, and the join warns beside any miss it measures against
	// a floor.
	partial bool
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

// doctorMCP merges the mcpServers maps of the four places Claude Code keeps
// them: <claudeRoot>/settings.json and the top-level mcpServers of
// ~/.claude.json (user scope), <projectDir>/.mcp.json and
// ~/.claude.json projects["<abs projectDir>"].mcpServers (project scope).
// The store file sits one level above claudeRoot — ~/.claude.json beside
// ~/.claude — which is why no extra parameter threads through the
// constructors. A later-listed source wins a name collision: project scope
// always beats user (DR-1's rule), and within one scope the .claude.json
// store wins over the hand-written file — that intra-scope precedence is
// tare's documented choice, not an observed harness rule, and every
// collision is warned naming both files because the shadowed entry is
// configuration that will never load. The findings pass judges the winning
// definition only. The ascending name list is the join's configured side:
// whatever the sources that DID parse declare, countable or not.
func (r *doctorReader) doctorMCP(claudeRoot, projectDir string) (count int, countable bool, names []string, block doctorBlock) {
	block.dimension = "mcp_server"
	block.findings = map[string][]doctorFinding{}
	type entry struct {
		raw   json.RawMessage
		from  string
		scope string // "user" | "project"; project entries merge last and win
	}
	type src struct {
		label string // named in warns — file path, or path plus section
		scope string
		srvs  map[string]json.RawMessage
	}
	settings := filepath.Join(claudeRoot, "settings.json")
	mcpJSON := filepath.Join(projectDir, ".mcp.json")
	claudeJSON := filepath.Join(filepath.Dir(claudeRoot), ".claude.json")

	var srcs []src
	broken, sawSource := false, false
	// readPlain contributes one whole-document mcpServers map, in the
	// standing posture: absent adds nothing silently, unreadable or
	// unparseable sets the broken floor and says so.
	readPlain := func(path, scope string) {
		raw, err := r.read(path)
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		if err != nil {
			block.warns = append(block.warns, fmt.Sprintf(
				"mcp: %s is not readable (%v) — mcp_servers_checked is omitted, not zero", path, err))
			broken = true
			return
		}
		var doc struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			block.warns = append(block.warns, fmt.Sprintf(
				"mcp: %s exists but is not valid JSON (%v) — mcp_servers_checked is omitted, not zero", path, err))
			broken = true
			return
		}
		sawSource = true
		srcs = append(srcs, src{path, scope, doc.MCPServers})
	}
	readPlain(settings, "user")

	// The one whole-file JSON read in doctor: ~/.claude.json carries
	// unrelated bulk (oauth state, per-project history), but the decoder
	// touches only the two mcpServers fields and the file is one per-machine
	// config of hundreds of KB — bounded by being a config, not by the
	// daily-growing corpus that stream-never-load protects. Absent is
	// silent like every other source; unreadable names the floor it puts
	// under the join, because never_observed against a half-read user
	// scope is a floor, not a verdict.
	var local *src
	raw, err := r.read(claudeJSON)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		block.warns = append(block.warns, fmt.Sprintf(
			"mcp: %s is not readable (%v) — mcp_servers_checked is omitted, not zero; the user-scope "+
				"set may be incomplete, so never_observed is then a floor, not a verdict", claudeJSON, err))
		broken = true
	default:
		var doc struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
			Projects   map[string]struct {
				MCPServers map[string]json.RawMessage `json:"mcpServers"`
			} `json:"projects"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			block.warns = append(block.warns, fmt.Sprintf(
				"mcp: %s exists but is not valid JSON (%v) — mcp_servers_checked is omitted, not zero; "+
					"the user-scope set may be incomplete, so never_observed is then a floor, not a verdict",
				claudeJSON, err))
			broken = true
			break
		}
		sawSource = true
		srcs = append(srcs, src{claudeJSON, "user", doc.MCPServers})
		abs, absErr := filepath.Abs(projectDir)
		if absErr != nil {
			abs = projectDir // it arrived from Getwd or a flag; best-effort key
		}
		if p, ok := doc.Projects[abs]; ok {
			local = &src{claudeJSON + ` projects["` + abs + `"]`, "project", p.MCPServers}
		}
	}
	readPlain(mcpJSON, "project")
	if local != nil {
		srcs = append(srcs, *local)
	}

	merged := map[string]entry{}
	var collisions []string
	for _, s := range srcs {
		for _, name := range slices.Sorted(maps.Keys(s.srvs)) {
			if prev, dup := merged[name]; dup {
				rule := fmt.Sprintf("later source wins on name collision within the %s scope", s.scope)
				if prev.scope != s.scope {
					rule = "project config wins on name collision"
				}
				collisions = append(collisions, fmt.Sprintf(
					"mcp server %q: the entry in %s overrides the one in %s (%s)",
					name, s.label, prev.from, rule))
			}
			merged[name] = entry{raw: s.srvs[name], from: s.label, scope: s.scope}
		}
	}
	// Collisions first, sorted with the pass: sources were walked user-then-
	// project and names ascending, so this order is fixed by construction.
	block.warns = append(block.warns, collisions...)
	names = slices.Sorted(maps.Keys(merged))
	if broken {
		// One unreadable source makes the total unknowable even though the
		// others parsed; their keyed rows still stand, only the denominator
		// is withheld. The parsed names survive for the join, marked partial
		// so a miss against them carries its caveat.
		block.partial = true
		return len(merged), false, names, block
	}
	if !sawSource {
		// A source that exists and parses but names no servers is a measured
		// zero — the config was read and it says none. Only when there is no
		// config to read at all is the count an absence.
		block.warns = append(block.warns, fmt.Sprintf(
			"mcp: none of %s exists — mcp_servers_checked is omitted, not zero",
			strings.Join([]string{settings, mcpJSON, claudeJSON}, ", ")))
		return 0, false, names, block
	}
	for _, name := range names {
		if finds := doctorCheckServer(merged[name].raw); len(finds) > 0 {
			block.findings[name] = finds
		}
	}
	return len(merged), true, names, block
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

// RenderDoctor prints the envelope as a table. Like every other renderer it
// reads only the envelope, so the table and `--json` cannot disagree.
//
// No --top, and no truncation: doctor's tables are bounded by the config
// surface — installed skills, plugins, servers — not by calls, so a cap would
// hide the finding the command exists to surface. The warnings ARE the report
// body: doctor's findings already ride them as human sentences, so renderTail
// prints every one verbatim under the flushed table and the tables only carry
// the counts.
//
// Keys print verbatim, absolute paths included: the finding names a file the
// user must open, and rewriting a path falsifies the address.
// ponytail: long skill keys widen column 0; add path-shortening here when
// real install layouts make the table unreadable — a renderer concern then,
// never an envelope change.
func RenderDoctor(w io.Writer, env Envelope) error {
	renderHeader(w, env)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// Two padding cells: every table below is four columns wide.
	writeCorpus(tw, env, "\t\t")
	for _, dim := range []string{"skill", "plugin"} {
		rows := groupRows(env.Metrics, dim)
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(tw, "\n%s\tFINDINGS\t\t\t\n", strings.ToUpper(dim))
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t\t\t\n", r.key, r.cell("findings"))
		}
	}
	if rows := groupRows(env.Metrics, "mcp_server"); len(rows) > 0 {
		fmt.Fprint(tw, "\nMCP_SERVER\tFINDINGS\tNEVER_OBSERVED\tUNCONFIGURED_OBSERVED\t\n")
		for _, r := range rows {
			// A blank cell is the vocabulary for "this server has no row of
			// that kind" — never a 0, which would claim a measured absence
			// of findings on every join-miss row and vice versa.
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t\n", r.key,
				r.cell("findings"), r.cell(doctorNeverObserved), r.cell(doctorUnconfiguredObserved))
		}
	}
	if !hasDoctorIssue(env) {
		// No tabs: the trailing-annotation rule. Omitted-block warnings, if
		// any, still follow under it — an omission is not a detected issue,
		// and this line claims nothing about rows the envelope could not
		// measure.
		fmt.Fprint(tw, "\nno configuration issues detected\n")
	}
	return renderTail(w, tw, env)
}

// hasDoctorIssue reports whether the envelope carries a static finding or a
// join miss — the two kinds RenderDoctor prints tables for.
func hasDoctorIssue(env Envelope) bool {
	return slices.ContainsFunc(env.Metrics, func(m Metric) bool {
		return m.Name == "findings" || m.Name == doctorNeverObserved || m.Name == doctorUnconfiguredObserved
	})
}
