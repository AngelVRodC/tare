package report

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/AngelVRodC/tare/internal/transcript"
)

// openCodeDBName is the file `--dir` is expected to contain.
const openCodeDBName = "opencode.db"

// openCodeQuery rolls every tool call up per tool name inside sqlite3, so no
// message content ever enters this process — that is what keeps "stream, never
// load" true for a 4 MB database and for a 400 MB one.
//
// CAST(... AS BLOB) is load-bearing. SQLite's length() counts *characters* for
// a text value; tare pins ContextBytes as the decoded UTF-8 byte length, so the
// bare form is a mislabelled figure. Measured on the live corpus: 189,612 bytes
// against 189,129 characters, a 483-byte 0.26% under-count — exactly the class
// of over-claim Envelope.Validate waves through, because it accepts `measured`
// unconditionally. octet_length() is also correct but arrived in SQLite 3.43.0;
// CAST AS BLOB is right on every version a user might have.
//
// min_ms/max_ms come back per group; openCodeRows folds them into one span.
const openCodeQuery = `SELECT json_extract(data,'$.tool')                                          AS tool,
       count(*)                                                             AS calls,
       sum(length(CAST(coalesce(json_extract(data,'$.state.output'),'') AS BLOB))) AS context_bytes,
       sum(json_extract(data,'$.state.status')='error')                     AS errors,
       min(time_created) AS min_ms, max(time_created) AS max_ms
FROM part WHERE json_extract(data,'$.type')='tool'
GROUP BY tool`

// openCodeToolRow is one row of openCodeQuery. sqlite3 -json names each column
// by its SELECT alias, so the JSON tags are the aliases verbatim.
type openCodeToolRow struct {
	Tool         string `json:"tool"`
	Calls        int64  `json:"calls"`
	ContextBytes int64  `json:"context_bytes"`
	// Errors is a pointer because sum() over a group whose rows all lack
	// $.state.status returns SQL NULL, and encoding/json decodes null into an
	// int64 as a no-op — leaving 0. A 0 here would be a `measured` claim that
	// OpenCode recorded no failures, which Envelope.Validate waves through.
	// nil means "not recorded" and the caller must warn, never zero-fill.
	Errors *int64 `json:"errors"`
	MinMs  int64  `json:"min_ms"`
	MaxMs  int64  `json:"max_ms"`
}

// openCodeSpan is the corpus date range, in Unix milliseconds — OpenCode stores
// 13-digit millisecond timestamps, not the seconds a Unix epoch usually means.
type openCodeSpan struct {
	MinMs, MaxMs int64
}

// openCodeRows decodes what sqlite3 -json printed and folds the per-group
// min/max into one span. Split out from the shell-out so the decode contract
// is testable with no sqlite3 on PATH.
//
// Empty output is an empty database, not a failure: sqlite3 prints nothing at
// all rather than `[]` when a query matches no rows.
func openCodeRows(out []byte) ([]openCodeToolRow, openCodeSpan, error) {
	var span openCodeSpan
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, span, nil
	}
	var rows []openCodeToolRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, span, fmt.Errorf("opencode query result is not the expected shape: %w", err)
	}
	for _, r := range rows {
		if span.MinMs == 0 || (r.MinMs != 0 && r.MinMs < span.MinMs) {
			span.MinMs = r.MinMs
		}
		if r.MaxMs > span.MaxMs {
			span.MaxMs = r.MaxMs
		}
	}
	return rows, span, nil
}

// openCodeRead runs the rollup against the OpenCode database.
//
// The database is the command's only data source, so a missing sqlite3, a
// missing database or an unreadable reply is an error rather than a degraded
// envelope: there is nothing left to report.
// The size comes back from the same stat that proves the file is readable. A
// second stat in the caller could fail on its own and leave a 0 in
// Corpus.Bytes — an absent number rendered as a measured one, which is the
// defect the rest of this file exists to prevent.
func openCodeRead(dir string) (rows []openCodeToolRow, span openCodeSpan, dbSize int64, err error) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil, openCodeSpan{}, 0, fmt.Errorf("sqlite3 is not on PATH and the opencode harness needs it to read %s: %w",
			openCodeDBName, err)
	}
	db := filepath.Join(dir, openCodeDBName)
	fi, err := os.Stat(db)
	if err != nil {
		return nil, openCodeSpan{}, 0, fmt.Errorf("opencode database not readable: %w", err)
	}
	out, err := runCmd(bin, "-readonly", "-json", db, openCodeQuery)
	if err != nil {
		return nil, openCodeSpan{}, 0, fmt.Errorf("opencode query failed: %s: %w", openCodeFailureCause(db), err)
	}
	rows, span, err = openCodeRows(out)
	return rows, span, fi.Size(), err
}

// openCodeFailureCause names why a -readonly open of db failed, from what is on
// disk beside it rather than from guessing: telling a user to recreate a file
// already sitting next to their database is the same over-claim the rest of
// this file exists to prevent. Split out of openCodeRead so it is testable with
// no sqlite3 on PATH, exactly as openCodeRows is.
//
// Measured 2026-09-06, every failing case returning error 14:
//
//	.db alone                     rc=14
//	.db + -wal                    rc=0, and sqlite3 CREATES -shm in that directory
//	.db + -shm                    rc=14
//	all three                     rc=0
//	.db + a touched 0-byte -wal   rc=0 — an empty -wal is enough
//	chmod 555 dir, .db + -wal     rc=14 — cannot create -shm
//	chmod 555 dir, all three      rc=0
//
// So **-wal is the discriminator, not -shm**: -shm is neither necessary nor
// sufficient. It is a file sqlite3 builds for itself when it is missing, which
// is why reading needs a *writable directory* whenever it is absent — a
// read-only open still writes, just never to .db or -wal. An earlier version of
// this branch stat'd -shm, so it fired only when -wal happened to be missing
// too, blamed the wrong file, and sent a user who had copied .db + -shm hunting
// for a lock that was not there.
//
// `file:...?immutable=1` opens in every one of these states and is deliberately
// not used: it ignores the WAL, trading a loud failure for a stale number.
func openCodeFailureCause(db string) string {
	if _, err := os.Stat(db + "-wal"); os.IsNotExist(err) {
		return fmt.Sprintf("the %[1]s-wal sidecar is missing, and opencode writes %[1]s in WAL mode, "+
			"where a read-only open needs it — copy %[1]s together with its -wal and -shm files and "+
			"point --dir at the copy", openCodeDBName)
	}
	if _, err := os.Stat(db + "-shm"); os.IsNotExist(err) {
		return fmt.Sprintf("%[1]s-shm is absent, so sqlite3 has to create it before it can read %[1]s, "+
			"and the directory holding them has to be writable for that — copy all three of %[1]s, "+
			"-wal and -shm somewhere writable and point --dir there", openCodeDBName)
	}
	return "a schema change, or the database is locked by another process"
}

// openCodeServers reads the configured MCP server names out of opencode.json.
//
// The order is a total one — longest name first, then lexically — for two
// reasons: splitOpenCodeMCP needs longest-first so a server whose name prefixes
// another cannot claim its tools, and a tiebreak keeps the result reproducible
// when Go randomises the map range that produced it.
func openCodeServers(configPath string) ([]string, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s is not the expected JSON shape: %w", configPath, err)
	}
	return slices.SortedFunc(maps.Keys(cfg.MCP), func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	}), nil
}

// splitOpenCodeMCP splits an OpenCode MCP tool name into its server and tool.
//
// OpenCode joins the two with a *single* underscore and tool names contain
// underscores of their own, so `engram_mem_search` splits as plausibly into
// `engram_mem`/`search`. There is no way to read the boundary off the name —
// splitMCP's `mcp__<server>__<tool>` trick does not port — so the server list
// is the only authority, and it must arrive longest-first.
func splitOpenCodeMCP(name string, servers []string) (server, tool string, ok bool) {
	for _, s := range servers {
		if rest, found := strings.CutPrefix(name, s+"_"); found && rest != "" {
			return s, rest, true
		}
	}
	return "", "", false
}

// openCodeConfigPath is where OpenCode keeps its MCP server list. It is not
// under `--dir`: OpenCode splits data (`~/.local/share/opencode`) from config
// (`~/.config/opencode`), so the database and the server names never share a
// root. The fallback mirrors main.go's defaultDir — a relative path rather
// than an error, because a missing home directory should cost the mcp_server
// block and a warning, not the whole command.
func openCodeConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "opencode", "opencode.json")
	}
	return filepath.Join(home, ".config", "opencode", "opencode.json")
}

// openCodeTime renders one of OpenCode's 13-digit millisecond timestamps as
// the RFC3339 string the builder's date range compares lexically. This is
// formatting, not parsing, so the rule that this program parses no times
// anywhere still holds.
func openCodeTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// OpenCodeToolsEnvelope reads the OpenCode database and emits the same `tools`
// envelope the Claude Code path emits — same command name, same metric names —
// so RenderTools, WriteJSON and Validate work unchanged and the JSON contract
// does not fork per harness.
func OpenCodeToolsEnvelope(dir, version string) (Envelope, error) {
	rows, span, dbSize, err := openCodeRead(dir)
	if err != nil {
		return Envelope{}, err
	}
	// A missing or unreadable config is not fatal here, unlike a missing
	// database: it costs the mcp_server block and a warning, nothing else.
	servers, serverErr := openCodeServers(openCodeConfigPath())
	return openCodeEnvelope(dir, version, rows, span, servers, serverErr, dbSize), nil
}

// openCodeEnvelope is the arithmetic half, split from the shell-out so the
// rollup→envelope contract is testable with no sqlite3 on PATH.
func openCodeEnvelope(dir, version string, rows []openCodeToolRow, span openCodeSpan,
	servers []string, serverErr error, dbSize int64) Envelope {
	b := newBuilder(dir, version, "tools")
	// A zero span is an empty database, not midnight in 1970 — widening the
	// range to the epoch would print a corpus that starts 56 years before the
	// harness existed.
	if span.MinMs != 0 {
		b.seeTime(openCodeTime(span.MinMs))
	}
	if span.MaxMs != 0 {
		b.seeTime(openCodeTime(span.MaxMs))
	}

	tools := map[string]*toolStat{}
	byServer := map[string]*toolStat{}
	// The keys whose error count sqlite3 returned as SQL NULL. sum() over a
	// group where no row carries $.state.status has no non-NULL input, so the
	// count is unrecorded — and a 0 there would be a `measured` claim that the
	// tool never failed, which Envelope.Validate accepts unconditionally.
	unknownTool := map[string]bool{}
	unknownServer := map[string]bool{}
	var totals toolStat
	errorsKnown := true

	// Not toolStat.add: the rollup already counted the calls inside sqlite3, so
	// each row is a group total rather than one call.
	accumulate := func(into *toolStat, r openCodeToolRow) {
		into.Calls += r.Calls
		into.ContextBytes += r.ContextBytes
		if r.Errors != nil {
			into.Errors += *r.Errors
		}
	}
	for _, r := range rows {
		accumulate(bucket(tools, r.Tool), r)
		accumulate(&totals, r)
		if r.Errors == nil {
			unknownTool[r.Tool] = true
			errorsKnown = false
		}
		// splitOpenCodeMCP matches nothing when the server list is empty, so a
		// missing config yields no mcp_server rows by construction.
		if server, _, ok := splitOpenCodeMCP(r.Tool, servers); ok {
			accumulate(bucket(byServer, server), r)
			if r.Errors == nil {
				unknownServer[server] = true
			}
		}
	}

	b.add("distinct_tools", len(tools), "tools")
	b.add("calls", totals.Calls, "calls")
	b.add("context_bytes", totals.ContextBytes, "bytes")
	// One unrecorded group makes the corpus total a floor, not a measurement.
	if errorsKnown {
		b.add("errors", totals.Errors, "calls")
	}

	const omitImages, omitProduced = "image_bytes", "produced_bytes"
	b.rows(dropUnknownErrors(statMetrics("tool", tools, omitImages, omitProduced), unknownTool)...)
	b.rows(dropUnknownErrors(statMetrics("mcp_server", byServer, omitImages, omitProduced), unknownServer)...)

	b.warn("image_bytes and image_results are not reported for opencode: it stores a tool result as one output string with no image payload broken out, so the figure is unmeasured — it is not a measurement of zero")
	b.warn("produced_bytes, externalised_results, externalised_produced_bytes and externalised_context_bytes are not reported for opencode: it records no pre-truncation output size and writes no side files, so what a tool produced before it reached the context is unmeasured — it is not a measurement of zero")
	b.warn("tool_use_blocks, tool_result_blocks, unmatched_results and unanswered_uses are not reported for opencode: the call and its result share one row, so the join those counters audit does not exist here — they have no meaning rather than a value of zero")
	if !errorsKnown {
		// Three rows are withheld, so all three are named: warn's contract is
		// that nothing is ever omitted silently, and naming one of three is
		// the same silence with extra steps.
		b.warn("errors is withheld for %s and from the corpus total: opencode recorded no call status on those rows, which is not a claim that the calls succeeded",
			strings.Join(slices.Sorted(maps.Keys(unknownTool)), ", "))
		if len(unknownServer) > 0 {
			b.warn("errors is withheld from the mcp_server rows for %s for the same reason: a server inherits the unrecorded status of the tools it owns",
				strings.Join(slices.Sorted(maps.Keys(unknownServer)), ", "))
		}
	}
	// An opencode.json that parses but carries no `mcp` key produces the same
	// missing block as no file at all, so it earns the same explanation.
	if len(servers) == 0 {
		reason := "it names no mcp servers"
		if serverErr != nil {
			reason = serverErr.Error()
		}
		b.warn("no mcp server list (%s): mcp_server rows are omitted, which is not a claim that no MCP server was used", reason)
	}
	b.warn("corpus bytes is the size of %s on disk, which includes indices and tables this command does not read — it is not comparable to a Claude Code corpus byte count", openCodeDBName)
	// Files: 1 because the corpus is one database file. done needs no seam:
	// it reads three fields, and a struct literal supplies all three.
	return b.done(transcript.ScanStats{Files: 1, Bytes: dbSize})
}

// dropUnknownErrors removes the errors row for any key whose count came back as
// SQL NULL. Leaving it in prints a 0 that Validate waves through as measured;
// dropping it prints a blank cell, which is what "not recorded" looks like
// everywhere else in this program.
func dropUnknownErrors(rows []Metric, unknown map[string]bool) []Metric {
	return slices.DeleteFunc(rows, func(m Metric) bool {
		return m.Name == "errors" && unknown[m.Key]
	})
}
