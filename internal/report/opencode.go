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
// min/max into one span. Split out from the shell-out, exactly as
// boostReportMetrics is, so the decode contract is testable with no sqlite3 on
// PATH.
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
// Unlike Boost, this is the command's only data source, so a missing sqlite3, a
// missing database or an unreadable reply is an error rather than a degraded
// envelope: there is nothing left to report.
func openCodeRead(dir string) ([]openCodeToolRow, openCodeSpan, error) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil, openCodeSpan{}, fmt.Errorf("sqlite3 is not on PATH and the opencode harness needs it to read %s: %w",
			openCodeDBName, err)
	}
	db := filepath.Join(dir, openCodeDBName)
	if _, err := os.Stat(db); err != nil {
		return nil, openCodeSpan{}, fmt.Errorf("opencode database not readable: %w", err)
	}
	out, err := runCmd(bin, "-readonly", "-json", db, openCodeQuery)
	if err != nil {
		return nil, openCodeSpan{}, fmt.Errorf("opencode query failed (schema change or locked DB?): %w", err)
	}
	return openCodeRows(out)
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
