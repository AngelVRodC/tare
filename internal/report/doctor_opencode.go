package report

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AngelVRodC/tare/internal/transcript"
)

// doctorURLMissing is the OpenCode twin of doctorCommandMissing, named for
// what the entry actually lacks: an OpenCode mcp server loads either a local
// command or a remote url, so an entry with neither is missing *a url*, not a
// command. The row name stays `findings` — only the kind printed in the warn
// line differs per harness, which is the JSON contract holding while the voice
// stays honest.
const doctorURLMissing = "url_missing"

// doctorToolSessions is the OpenCode join's denominator row. Claude's
// doctorMCPSessions counts sessions carrying any *mcp* activity, which
// OpenCode cannot observe: it records no attribution fields, and a tool name
// says nothing about whether its owner is an MCP server or a built-in. The
// honest denominator there is sessions carrying any tool call at all — a wider
// floor, named for what it counts instead of wearing claude's row name and
// meaning something else under it.
const doctorToolSessions = "tool_sessions"

// openCodeObservation is one (tool, session) pair of the OpenCode database:
// a tool call seen by a session, rolled up inside sqlite3 so message content
// never enters this process. MinMs/MaxMs are non-pointer because a GROUP BY
// row always carries at least one contributing row — min/max over a group
// cannot be SQL NULL the way sum() over all-NULL inputs can (the second
// SQLite trap; there is no group here whose rows could all lack the column,
// time_created is NOT NULL in the schema).
type openCodeObservation struct {
	Tool    string `json:"tool"`
	Session string `json:"session"`
	MinMs   int64  `json:"min_ms"`
	MaxMs   int64  `json:"max_ms"`
}

// openCodeObserveQuery builds the (tool, session) rollup, windowed in SQL:
// the doctor's whole point is holding the configured set against what the
// window *observed*, so the window has to reach the database, not just the
// envelope header. Bounds arrive from a validated Window (checkBound allows
// digits and fixed punctuation only — no quote or operator can compose into
// this string), and they compose per the window.go contract: strftime
// truncates the fraction, so msPart adds it back. A bare --until means the
// whole named day, so it windows as strictly before the next midnight.
func openCodeObserveQuery(w Window) string {
	conds := []string{"json_extract(data,'$.type')='tool'"}
	if s := w.Since(); s != "" {
		conds = append(conds, fmt.Sprintf("time_created >= strftime('%%s','%s')*1000 + %d", s, msPart(s)))
	}
	if u := w.Until(); u != "" {
		if w.UntilBare() {
			conds = append(conds, fmt.Sprintf("time_created < strftime('%%s','%s','+1 day')*1000", u))
		} else {
			conds = append(conds, fmt.Sprintf("time_created <= strftime('%%s','%s')*1000 + %d", u, msPart(u)))
		}
	}
	return "SELECT json_extract(data,'$.tool') AS tool, session_id AS session,\n" +
		"       min(time_created) AS min_ms, max(time_created) AS max_ms\n" +
		"FROM part WHERE " + strings.Join(conds, " AND ") + "\n" +
		"GROUP BY tool, session"
}

// openCodeObservations decodes the rollup, split from the shell-out exactly
// as openCodeRows is, so the decode contract is testable with no sqlite3 on
// PATH. Empty output is an empty (or fully windowed-out) database, not a
// failure: sqlite3 prints nothing rather than [] for a zero-row result.
func openCodeObservations(out []byte) ([]openCodeObservation, openCodeSpan, error) {
	var span openCodeSpan
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, span, nil
	}
	var obs []openCodeObservation
	if err := json.Unmarshal(out, &obs); err != nil {
		return nil, span, fmt.Errorf("opencode query result is not the expected shape: %w", err)
	}
	for _, o := range obs {
		if span.MinMs == 0 || (o.MinMs != 0 && o.MinMs < span.MinMs) {
			span.MinMs = o.MinMs
		}
		if o.MaxMs > span.MaxMs {
			span.MaxMs = o.MaxMs
		}
	}
	return obs, span, nil
}

// openCodeObserve reads the (tool, session) rollup from the database in dir,
// under exactly openCodeRead's discipline — that is the "snapshot discipline"
// this adapter's precedent carries: a -readonly sqlite3 open of the live
// database, never a mutable one, and openCodeFailureCause naming why a
// failure happened from what sits on disk beside it (-wal gates the open,
// absent -shm means the read may create one, .db and -wal bytes are never
// touched). Unlike the tools reader, a failure here returns an error the
// caller degrades — the doctor's blocks stand alone — instead of aborting the
// command.
func openCodeObserve(dir string, w Window) (obs []openCodeObservation, span openCodeSpan, dbSize int64, err error) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil, openCodeSpan{}, 0, fmt.Errorf("sqlite3 is not on PATH (%v), so the opencode %s cannot be read", err, openCodeDBName)
	}
	db := filepath.Join(dir, openCodeDBName)
	fi, err := os.Stat(db)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, openCodeSpan{}, 0, fmt.Errorf("no %s in %s (%v) — if no opencode session has ever run, this is expected", openCodeDBName, dir, err)
		}
		return nil, openCodeSpan{}, 0, fmt.Errorf("opencode database not readable: %w", err)
	}
	out, err := runCmd(bin, "-readonly", "-json", db, openCodeObserveQuery(w))
	if err != nil {
		return nil, openCodeSpan{}, 0, fmt.Errorf("opencode query failed: %s: %w", openCodeFailureCause(db), err)
	}
	obs, span, err = openCodeObservations(out)
	return obs, span, fi.Size(), err
}

// OpenCodeDoctorEnvelope is the second doctor adapter: same command name,
// same envelope, same renderer as DoctorJoinEnvelope, chosen by main.go's
// case — the tools precedent, with no interface and no registry. configRoot
// is ~/.config/opencode (its opencode.json holds the mcp block and its
// skills/ dir is a skill root), projectDir the repo being inspected, dbDir
// the directory holding opencode.db.
//
// The three passes are Claude's doctor minus what OpenCode cannot support,
// each standing alone:
//
//  1. MCP config: the mcp blocks of <configRoot>/opencode.json and
//     <projectDir>/opencode.json, merged project-wins, local servers' first
//     command token checked on PATH, remote servers counted with no liveness
//     claim, enabled:false read as a state, not a defect.
//  2. Skills: the same doctorCheckSkill rules over the roots OpenCode reads —
//     the parser is shared verbatim, only the root list differs.
//  3. Plugins: OpenCode has no manifest tare can validate, so the block is
//     omitted with one warning. plugins_checked is absent, never 0 — the
//     absent≠zero rule earning its keep on the second harness.
//
// The join then holds the configured server names against the (tool, session)
// pairs openCodeObserve read: never_observed (with the disabled servers
// excluded — inert configuration observed nothing by contract) and
// unconfigured_observed, whose keys are tool names, because OpenCode joins
// server and tool with a single underscore and records no attribution
// fields. A failed or absent database costs the join and warns; the config
// passes still run. Every metric is read straight off files or SQL, so every
// row is `measured`.
func OpenCodeDoctorEnvelope(configRoot, projectDir, dbDir, version string, w Window) (Envelope, error) {
	obs, span, dbSize, obsErr := openCodeObserve(dbDir, w)
	return openCodeDoctorEnvelope(configRoot, projectDir, dbDir, version, obs, span, obsErr, dbSize, w)
}

// openCodeDoctorEnvelope is the arithmetic half, split from the database
// read so the rollup→envelope contract is testable with no sqlite3 on PATH —
// the openCodeEnvelope precedent. obs/obsSpan/obsErr/dbSize are whatever the
// read produced; a set obsErr degrades the join block and nothing else.
func openCodeDoctorEnvelope(configRoot, projectDir, dbDir, version string,
	obs []openCodeObservation, obsSpan openCodeSpan, obsErr error, dbSize int64, w Window) (Envelope, error) {
	configErr := statRoot(configRoot)
	projectErr := statRoot(projectDir)
	if configErr != nil && projectErr != nil {
		return Envelope{}, fmt.Errorf(
			"doctor can read neither the opencode config root %s (%w) nor the project root %s (%w)",
			configRoot, configErr, projectDir, projectErr)
	}

	b := newBuilder(dbDir, version, "doctor", w)
	if w.Active() {
		// Said once up front rather than inside the join: it is true whether
		// or not the database opened — the config rows are point-in-time
		// either way. done() adds the standard C1 on top of this.
		b.warn("--since/--until windows the opencode database join only: the static config checks are "+
			"point-in-time — configuration carries no event timestamps (window given: %s..%s)",
			w.Since(), w.Until())
	}

	rd := &doctorReader{}
	skills, skillsCountable, skillBlock := rd.doctorSkillsAt([]string{
		filepath.Join(configRoot, "skills"),
		filepath.Join(projectDir, ".opencode", "skills"),
		filepath.Join(projectDir, ".claude", "skills"),
		filepath.Join(projectDir, ".agents", "skills"),
	})
	servers, serversCountable, configured, disabled, mcpBlock := rd.openCodeDoctorMCP(configRoot, projectDir)

	if skillsCountable {
		b.add("skills_checked", int64(skills), "skills")
	}
	if serversCountable {
		b.add("mcp_servers_checked", int64(servers), "servers")
	}
	emitDoctorBlock(b, skillBlock)
	b.warn("plugins: plugin checks are claude-only — opencode has no equivalent manifest tare can " +
		"validate; plugins_checked is omitted, and absence here is not a claim the harness is healthy")
	emitDoctorBlock(b, mcpBlock)

	if obsErr == nil {
		// The span drives the header's date range and the window's matched
		// flag, exactly as the tools adapter folds it; a zero span is an
		// empty (or windowed-out) database, not midnight in 1970. The type
		// is empty because OpenCode has no Event — millisecond integers
		// formatted, not lines parsed.
		if obsSpan.MinMs != 0 {
			b.seeTime(openCodeTime(obsSpan.MinMs), "")
		}
		if obsSpan.MaxMs != 0 {
			b.seeTime(openCodeTime(obsSpan.MaxMs), "")
		}
		openCodeDoctorJoin(b, w, configured, disabled, obs, mcpBlock.partial)
	} else {
		b.warn("opencode: %v — the cross-join is omitted: never_observed and unconfigured_observed are "+
			"not claimed, which is an absence measured against nothing, not a claim the configuration is "+
			"healthy; the config passes above stand alone", obsErr)
	}

	stats := transcript.ScanStats{Files: rd.files, Bytes: rd.bytes}
	if obsErr == nil {
		// Files: 1 for the database, like the tools adapter; its bytes join
		// the config bytes read above, with the same caveat that DB bytes
		// include indexes and tables this command does not read.
		stats.Files++
		stats.Bytes += dbSize
		b.warn("corpus bytes is the size of %s on disk plus the config files read, which includes "+
			"indices and tables this command does not read — it is not comparable to a Claude Code "+
			"corpus byte count", openCodeDBName)
	}
	return b.done(stats), nil
}

// openCodeDoctorMCP merges the mcp blocks of the two opencode.json files —
// global user scope, project wins — and judges each winning entry. Absent
// files add nothing silently; a file that exists but cannot be read or parsed
// sets the same broken floor as Claude's pass: the count is withheld, the
// surviving names join anyway, and the block is marked partial. A file that
// parses with no mcp key is a read config that says none — a measured zero.
func (r *doctorReader) openCodeDoctorMCP(configRoot, projectDir string) (
	count int, countable bool, names []string, disabled map[string]bool, block doctorBlock) {
	block.dimension = "mcp_server"
	block.findings = map[string][]doctorFinding{}
	disabled = map[string]bool{}
	global := filepath.Join(configRoot, "opencode.json")
	project := filepath.Join(projectDir, "opencode.json")

	var srcs []mcpSrc
	broken, sawSource := false, false
	read := func(path, scope string) {
		raw, err := r.read(path)
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		if err != nil {
			block.warns = append(block.warns, fmt.Sprintf(
				"mcp: %s is not readable (%v) — mcp_servers_checked is omitted, not zero; the configured "+
					"set may be incomplete, so never_observed is then a floor, not a verdict", path, err))
			broken = true
			return
		}
		var doc struct {
			MCP map[string]json.RawMessage `json:"mcp"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			block.warns = append(block.warns, fmt.Sprintf(
				"mcp: %s exists but is not valid JSON (%v) — mcp_servers_checked is omitted, not zero; the "+
					"configured set may be incomplete, so never_observed is then a floor, not a verdict", path, err))
			broken = true
			return
		}
		sawSource = true
		srcs = append(srcs, mcpSrc{path, scope, doc.MCP})
	}
	read(global, "user")
	read(project, "project")

	merged, collisions := doctorMergeMCPEntries(srcs)
	block.warns = append(block.warns, collisions...)
	names = slices.Sorted(maps.Keys(merged))
	if broken {
		block.partial = true
		return len(merged), false, names, disabled, block
	}
	if !sawSource {
		block.warns = append(block.warns, fmt.Sprintf(
			"mcp: none of %s exists — mcp_servers_checked is omitted, not zero; opencode.jsonc beside "+
				"them is configuration tare does not read", strings.Join([]string{global, project}, ", ")))
		return 0, false, names, disabled, block
	}
	for _, name := range names {
		finds, isDisabled := openCodeCheckServer(merged[name].raw)
		if isDisabled {
			disabled[name] = true
		}
		if len(finds) > 0 {
			block.findings[name] = finds
		}
	}
	return len(merged), true, names, disabled, block
}

// openCodeCheckServer judges one opencode mcp entry. OpenCode spells its two
// server kinds local (a command array) and remote (a url), and the entry
// shape sniffed here is tolerant: the command wins when present (first token
// on PATH — the same measurement and the same caveat as Claude's stdio
// branch), a url alone loads remotely and earns no liveness claim, and an
// entry with neither names nothing loadable at all. enabled:false is a state
// the caller needs for the join, not a finding: a server the harness is told
// not to load is configured-but-inert, and its silence is contract, not
// defect.
func openCodeCheckServer(raw json.RawMessage) (findings []doctorFinding, isDisabled bool) {
	var spec struct {
		Type    string          `json:"type"`
		Command json.RawMessage `json:"command"`
		URL     string          `json:"url"`
		Enabled *bool           `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return []doctorFinding{{doctorServerTypeUnknown,
			fmt.Sprintf("entry is not a JSON object (%v), so no server type can be read", err)}}, false
	}
	isDisabled = spec.Enabled != nil && !*spec.Enabled
	switch spec.Type {
	case "", "local", "remote":
	default:
		return []doctorFinding{{doctorServerTypeUnknown,
			fmt.Sprintf("type %q is not one of local, remote (absent type is read off command or url)", spec.Type)}}, isDisabled
	}
	if command := openCodeFirstToken(spec.Command); command != "" {
		if _, err := exec.LookPath(command); err != nil {
			return []doctorFinding{doctorLookPathFinding(command, err)}, isDisabled
		}
		return nil, isDisabled
	}
	if strings.TrimSpace(spec.URL) != "" {
		return nil, isDisabled
	}
	return []doctorFinding{{doctorURLMissing,
		"entry names neither a command to launch nor a url to connect to"}}, isDisabled
}

// openCodeFirstToken reads the launch token of an opencode local server's
// command: the documented shape is an array whose first element is the
// binary; a bare string is tolerated the same way. Anything else (a number,
// an object, an empty array) reads as no command at all, and the caller's
// url branch or url_missing judges it.
func openCodeFirstToken(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err == nil {
		if len(argv) > 0 {
			return strings.TrimSpace(argv[0])
		}
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

// openCodeDoctorJoin emits the two join misses over the observed rollup. The
// matching is openCodeServer's longest-name-first prefix against the
// configured list — mcpServer's mcp__ split does not travel, because OpenCode
// joins server and tool with a single underscore. The window already gated
// the observation inside the SQL, and both row kinds stay `measured`.
//
// never_observed's denominator is tool_sessions — every session carrying any
// tool call — because OpenCode cannot tell an MCP call from a built-in one by
// anything but a configured name, and the warn says so: the wider floor is
// named in the sentence. unconfigured_observed keys are observed *tool*
// names: a miss here means "no configured server prefixes this name", and
// the warn names the benign arrivals (built-in tools, plugin-provided
// servers, config scopes like opencode.jsonc that tare does not read) rather
// than pretending the row indicts a server.
func openCodeDoctorJoin(b *builder, w Window, configured []string, disabled map[string]bool,
	obs []openCodeObservation, configPartial bool) {
	// The configured list arrives key-ascending; openCodeServer needs it
	// longest-name-first or a short server claims a longer server's tools
	// ("server_a" would steal "server_a_b_x" from "server_a_b"). Same order
	// openCodeServers pins for the tools adapter — total, so the join stays
	// reproducible.
	longest := slices.Clone(configured)
	slices.SortFunc(longest, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	sessions := map[string]bool{}
	perServer := map[string]map[string]bool{}
	perTool := map[string]map[string]bool{}
	for _, o := range obs {
		sessions[o.Session] = true
		if o.Tool == "" {
			continue // an unnamed part cannot name a server either way — silence, not a row
		}
		into := perServer
		server := openCodeServer(o.Tool, longest)
		if server == "" {
			into, server = perTool, o.Tool
		}
		if into[server] == nil {
			into[server] = map[string]bool{}
		}
		into[server][o.Session] = true
	}
	toolSessions := int64(len(sessions))
	b.addWin(doctorToolSessions, toolSessions, "sessions")

	isConfigured := make(map[string]bool, len(configured))
	for _, name := range configured {
		isConfigured[name] = true
	}
	if configPartial {
		b.warn("mcp: a config source was unreadable, so the configured set is partial — " +
			"an unconfigured_observed server may live in the source doctor could not read")
	}

	var never, unconfigured []string
	var disabledNames []string
	for _, name := range configured { // already key-ascending from the pass
		switch {
		case disabled[name]:
			disabledNames = append(disabledNames, name)
		case perServer[name] == nil:
			never = append(never, name)
		}
	}
	for _, tool := range slices.Sorted(maps.Keys(perTool)) {
		unconfigured = append(unconfigured, tool)
	}
	if len(disabledNames) > 0 {
		b.warn("mcp: %s are configured with enabled:false — never_observed is not claimed against a "+
			"server the harness is told not to load; their silence is the configuration working, and the "+
			"names stay in mcp_servers_checked", strings.Join(disabledNames, ", "))
	}
	if toolSessions == 0 && len(never) > 0 {
		scope := "in the database"
		if w.Active() {
			scope = "in the database within this window"
		}
		b.warn("opencode: no tool call was seen %s, so never_observed is not claimed — %d configured "+
			"servers would be an absence measured against zero tool-using sessions, which says nothing",
			scope, len(never))
		never = nil
	}

	// Two sorted passes rather than Claude's single merged one: the two key
	// sets are server names and tool names, and one name can be both sides
	// at once (a tool literally named like a configured server); two row
	// names over two keys cannot collide when each kind keeps its own list.
	for _, key := range never {
		b.rows(MeasuredMetric(doctorNeverObserved, "mcp_server", key, toolSessions, "sessions"))
		b.warn("mcp_server %q: configured but never seen in the opencode database — %d session(s) carry "+
			"any tool call and none names this server; either it never connected, the harness wrote no "+
			"call for it, or the window predates it; absence here is not a claim the server is broken",
			key, toolSessions)
	}
	for _, key := range unconfigured {
		n := int64(len(perTool[key]))
		b.rows(MeasuredMetric(doctorUnconfiguredObserved, "mcp_server", key, n, "sessions"))
		b.warn("mcp_server %q: observed in the opencode database (%d tool-using session(s)) but matched "+
			"by no configured server name — opencode records no attribution fields, so this join is "+
			"name-based over tool names: built-in tools, plugin-provided servers and config scopes tare "+
			"does not read (opencode.jsonc) arrive here too; a join miss, not a claim the server is "+
			"misconfigured", key, n)
	}
}
