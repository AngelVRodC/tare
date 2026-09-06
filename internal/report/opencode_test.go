package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// openCodeFixture is the literal `sqlite3 -readonly -json` reply, measured
// 2026-09-06 against the live database. Six of the eighteen tools, kept
// verbatim so the test exercises the real wire shape rather than a hand-typed
// idea of it. The byte figures are the CAST(... AS BLOB) ones.
const openCodeFixture = `[{"tool":"bash","calls":21,"context_bytes":20582,"errors":0,"min_ms":1786915693156,"max_ms":1786919026356},
{"tool":"context7_query-docs","calls":2,"context_bytes":9756,"errors":0,"min_ms":1786917107053,"max_ms":1786917107905},
{"tool":"engram_mem_search","calls":6,"context_bytes":3161,"errors":0,"min_ms":1786915743815,"max_ms":1786919026688},
{"tool":"read","calls":24,"context_bytes":103472,"errors":0,"min_ms":1786915824416,"max_ms":1786918934111},
{"tool":"task","calls":5,"context_bytes":6940,"errors":1,"min_ms":1786915187624,"max_ms":1786918905731},
{"tool":"webfetch","calls":2,"context_bytes":37206,"errors":0,"min_ms":1786917098091,"max_ms":1786917410008}]`

func TestOpenCodeRowsDecode(t *testing.T) {
	rows, span, err := openCodeRows([]byte(openCodeFixture))
	if err != nil {
		t.Fatalf("openCodeRows: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("rows = %d, want 6", len(rows))
	}

	// read is the heaviest tool and task is the only one carrying an error, so
	// between them they pin every column the query selects.
	if got := rows[3]; got.Tool != "read" || got.Calls != 24 || got.ContextBytes != 103472 || got.Errors == nil || *got.Errors != 0 {
		t.Errorf("read row = %+v, want {read 24 103472 0}", got)
	}
	if got := rows[4]; got.Tool != "task" || got.Calls != 5 || got.ContextBytes != 6940 || got.Errors == nil || *got.Errors != 1 {
		t.Errorf("task row = %+v, want {task 5 6940 1}", got)
	}

	var calls, bytes, errs int64
	for _, r := range rows {
		calls += r.Calls
		bytes += r.ContextBytes
		if r.Errors != nil {
			errs += *r.Errors
		}
	}
	if calls != 60 || bytes != 181117 || errs != 1 {
		t.Errorf("totals = %d calls, %d bytes, %d errors; want 60, 181117, 1", calls, bytes, errs)
	}

	// The span is the fold across every group, not any one row's pair.
	if span.MinMs != 1786915187624 || span.MaxMs != 1786919026688 {
		t.Errorf("span = %+v, want {1786915187624 1786919026688}", span)
	}
}

// sqlite3 prints nothing at all — not `[]` — for a query matching no rows. An
// empty database is not a failure.
func TestOpenCodeRowsEmptyIsNotAnError(t *testing.T) {
	rows, span, err := openCodeRows([]byte("  \n"))
	if err != nil || len(rows) != 0 || span != (openCodeSpan{}) {
		t.Errorf("openCodeRows(empty) = %v, %+v, %v; want no rows, zero span, no error", rows, span, err)
	}
}

func TestOpenCodeRowsRejectsNonJSON(t *testing.T) {
	if _, _, err := openCodeRows([]byte("Error: no such table: part")); err == nil {
		t.Error("openCodeRows accepted a non-JSON reply; a garbled result must be an error")
	}
}

func TestSplitOpenCodeMCP(t *testing.T) {
	// As openCodeServers returns them: longest first, then lexically.
	servers := []string{"context7", "engram"}
	nested := []string{"engram_mem", "engram"}

	cases := []struct {
		name         string
		servers      []string
		server, tool string
		ok           bool
	}{
		{"engram_mem_search", servers, "engram", "mem_search", true},
		{"context7_query-docs", servers, "context7", "query-docs", true},
		// Longest-first is what stops `engram` claiming a tool that belongs to
		// `engram_mem`; lexical order alone would get this backwards.
		{"engram_mem_search", nested, "engram_mem", "search", true},
		{"read", servers, "", "", false},
		// A bare server name is not a call to one of its tools.
		{"engram", servers, "", "", false},
		{"engram_", servers, "", "", false},
	}
	for _, c := range cases {
		server, tool, ok := splitOpenCodeMCP(c.name, c.servers)
		if server != c.server || tool != c.tool || ok != c.ok {
			t.Errorf("splitOpenCodeMCP(%q, %v) = %q, %q, %v; want %q, %q, %v",
				c.name, c.servers, server, tool, ok, c.server, c.tool, c.ok)
		}
	}
}

func TestOpenCodeServersOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"a":{},"abc":{},"zz":{},"ab":{},"ay":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// ab/ay/zz are equal-length on purpose: with three distinct lengths the
	// length comparator orders the whole fixture by itself and the lexical
	// tiebreak is never reached, so deleting it would not fail the test.
	want := []string{"abc", "ab", "ay", "zz", "a"}
	// Twice: the names come off a Go map, whose range order is randomised, so
	// one agreeing run proves nothing.
	for i := range 2 {
		got, err := openCodeServers(path)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("run %d: openCodeServers = %v, want %v", i, got, want)
		}
	}
}

func TestOpenCodeServersMissingFile(t *testing.T) {
	got, err := openCodeServers(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Error("openCodeServers accepted a missing config; the caller needs the reason to warn with")
	}
	if len(got) != 0 {
		t.Errorf("openCodeServers = %v, want no servers", got)
	}
}

func TestOpenCodeReadSkipsWithoutSqlite3(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, _, _, err := openCodeRead(t.TempDir())
	if err == nil {
		t.Fatal("openCodeRead succeeded with no sqlite3 on PATH")
	}
	// Unlike Boost, this is the only data source, so the error has to name what
	// is missing rather than degrade to an empty report.
	if !strings.Contains(err.Error(), "sqlite3") {
		t.Errorf("error %q does not name sqlite3", err)
	}
}

// A tool group whose rows all lack $.state.status makes sum() return SQL NULL.
// Decoding that into an int64 field would silently claim zero errors.
func TestOpenCodeRowsNullErrorsIsNotZero(t *testing.T) {
	const nullErrors = `[{"tool":"read","calls":2,"context_bytes":4,"errors":null,"min_ms":1786915187624,"max_ms":1786915187625}]`
	rows, _, err := openCodeRows([]byte(nullErrors))
	if err != nil {
		t.Fatalf("openCodeRows: %v", err)
	}
	if rows[0].Errors != nil {
		t.Errorf("Errors = %d, want nil — a null aggregate is absent, not zero", *rows[0].Errors)
	}
}

// openCodeFixtureEnvelope builds the envelope the measured rollup produces,
// with no sqlite3, no database and no config file involved — the same split
// boostReportMetrics uses to keep its arithmetic testable.
func openCodeFixtureEnvelope(t *testing.T, servers []string, serverErr error) Envelope {
	t.Helper()
	rows, span, err := openCodeRows([]byte(openCodeFixture))
	if err != nil {
		t.Fatalf("openCodeRows: %v", err)
	}
	// 4,284,416 is the measured size of the live opencode.db.
	return openCodeEnvelope("/oc", "test", rows, span, servers, serverErr, 4284416)
}

// openCodeServerList is what openCodeServers returns for the live config:
// longest first, then lexically.
var openCodeServerList = []string{"context7", "engram"}

func TestOpenCodeEnvelopeValidates(t *testing.T) {
	env := openCodeFixtureEnvelope(t, openCodeServerList, nil)
	if err := env.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if env.Command != "tools" {
		t.Errorf("command = %q, want tools — the JSON contract must not fork per harness", env.Command)
	}
	// The date range comes from formatting OpenCode's millisecond integers,
	// never from parsing a timestamp.
	if env.Corpus.From != "2026-08-16" || env.Corpus.To != "2026-08-16" {
		t.Errorf("corpus range = %q..%q, want 2026-08-16 on both", env.Corpus.From, env.Corpus.To)
	}
	if env.Corpus.Files != 1 || env.Corpus.Bytes != 4284416 {
		t.Errorf("corpus = %d files, %d bytes; want 1, 4284416", env.Corpus.Files, env.Corpus.Bytes)
	}

	want := map[[3]string]any{
		{"distinct_tools", "corpus", ""}:            6,
		{"calls", "corpus", ""}:                     int64(60),
		{"context_bytes", "corpus", ""}:             int64(181117),
		{"errors", "corpus", ""}:                    int64(1),
		{"context_bytes", "tool", "read"}:           int64(103472),
		{"errors", "tool", "task"}:                  int64(1),
		{"context_bytes", "mcp_server", "engram"}:   int64(3161),
		{"context_bytes", "mcp_server", "context7"}: int64(9756),
	}
	for k, exp := range want {
		if got := metricValue(t, env, k[0], k[1], k[2]); got != exp {
			t.Errorf("%s/%s/%s = %v, want %v", k[0], k[1], k[2], got, exp)
		}
	}
	// Heaviest context first, so the table needs no second sort.
	var order []string
	for _, m := range env.Metrics {
		if m.Dimension == "tool" && m.Name == "calls" {
			order = append(order, m.Key)
		}
	}
	if len(order) != 6 || order[0] != "read" {
		t.Errorf("tool order = %v, want the heaviest context (read) first", order)
	}
}

// TestOpenCodeEnvelopeOmitsUnavailable is the absent-versus-zero gate. A zero
// here would be a `measured` row, and Validate accepts measured unconditionally.
func TestOpenCodeEnvelopeOmitsUnavailable(t *testing.T) {
	env := openCodeFixtureEnvelope(t, openCodeServerList, nil)
	for _, m := range env.Metrics {
		switch m.Name {
		case "image_bytes", "produced_bytes", "image_results",
			"externalised_results", "externalised_produced_bytes", "externalised_context_bytes",
			"tool_use_blocks", "tool_result_blocks", "unmatched_results", "unanswered_uses":
			t.Errorf("metric %s/%s/%s = %v was emitted; opencode records nothing to measure it from",
				m.Name, m.Dimension, m.Key, m.Value)
		}
	}
	joined := strings.Join(env.Warnings, "\n")
	for _, want := range []string{"image_bytes", "produced_bytes", "unmatched_results"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning names %s; an omitted metric must be stated, never dropped silently:\n%s", want, joined)
		}
	}
	// The corpus byte figure is a database file size, not a transcript volume,
	// and saying so is the difference between a measurement and a comparison
	// nobody can make.
	if !strings.Contains(joined, openCodeDBName) {
		t.Errorf("no warning explains that corpus bytes is the %s file size:\n%s", openCodeDBName, joined)
	}
}

// TestOpenCodeEnvelopeIsReproducible guards the defect TestReportReproducible
// already caught once: the rollup and the server split both range over Go maps,
// whose order is randomised.
func TestOpenCodeEnvelopeIsReproducible(t *testing.T) {
	first, err := json.Marshal(openCodeFixtureEnvelope(t, openCodeServerList, nil))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		again, err := json.Marshal(openCodeFixtureEnvelope(t, openCodeServerList, nil))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("run %d differs from run 0", i+1)
		}
	}
}

// TestStatMetricsOmit guards the two pre-existing Claude Code call sites, which
// pass no omit list and must keep emitting all five rows per key.
func TestStatMetricsOmit(t *testing.T) {
	stats := map[string]*toolStat{
		"Bash": {Calls: 2, ContextBytes: 90, ImageBytes: 3, ProducedBytes: 4, Errors: 1},
		"Read": {Calls: 1, ContextBytes: 10},
	}
	all := statMetrics("tool", stats)
	if len(all) != 10 {
		t.Errorf("statMetrics with no omit = %d rows, want 10 (5 per key)", len(all))
	}
	fewer := statMetrics("tool", stats, "image_bytes", "produced_bytes")
	if len(fewer) != 6 {
		t.Errorf("statMetrics omitting two = %d rows, want 6 (3 per key)", len(fewer))
	}
	for _, m := range fewer {
		if m.Name == "image_bytes" || m.Name == "produced_bytes" {
			t.Errorf("omitted name %s survived", m.Name)
		}
	}
	// Omitting is not reordering: what is left keeps the order it had.
	if fewer[0].Name != "calls" || fewer[1].Name != "context_bytes" || fewer[2].Name != "errors" {
		t.Errorf("row order = %s/%s/%s, want calls/context_bytes/errors",
			fewer[0].Name, fewer[1].Name, fewer[2].Name)
	}
}

// TestRenderToolsHandlesOmittedMetrics pins both failure modes: 0 is the
// over-claim, <nil> is what formatValue printed before the guard.
func TestRenderToolsHandlesOmittedMetrics(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderTools(&buf, openCodeFixtureEnvelope(t, openCodeServerList, nil)); err != nil {
		t.Fatalf("RenderTools: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "<nil>") {
		t.Errorf("table prints <nil> for an omitted metric:\n%s", out)
	}

	// The two columns are read positionally. tabwriter left-aligns, so a data
	// cell starts at the same offset as its header — and positional reading is
	// the only way to see a *blank* cell, since splitting on whitespace
	// collapses it into the gap beside it.
	var header, read string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "TOOL "):
			header = l
		case strings.HasPrefix(l, "read "):
			read = l
		}
	}
	if header == "" || read == "" {
		t.Fatalf("no TOOL header or read row:\n%s", out)
	}
	for _, col := range []string{"IMAGES", "PRODUCED"} {
		at := strings.Index(header, col)
		if at < 0 || at+len(col) > len(read) {
			t.Fatalf("cannot locate the %s column:\n%s", col, out)
		}
		if got := strings.TrimSpace(read[at : at+len(col)]); got != "" {
			t.Errorf("%s on the read row = %q, want blank — 0 claims a measurement nobody made", col, got)
		}
	}
	// The columns tare *did* measure still print.
	if !strings.Contains(read, "103.5 kB") {
		t.Errorf("read row lost its context bytes:\n%s", out)
	}
}

// TestOpenCodeEnvelopeOmitsNullErrors is the envelope half of the SQL NULL
// trap openCodeRows guards on the decode side: sum() over a group where no row
// carries $.state.status is unrecorded, and folding it in as 0 would ship
// "opencode recorded no errors" as a measured figure.
func TestOpenCodeEnvelopeOmitsNullErrors(t *testing.T) {
	// engram_mem_search carries the NULL so the server that owns it inherits
	// the gap — the mcp_server row is the third thing withheld, and the one
	// easiest to withhold silently.
	const nullErrors = `[{"tool":"engram_mem_search","calls":2,"context_bytes":400,"errors":null,"min_ms":1786915187624,"max_ms":1786915187625},
{"tool":"task","calls":1,"context_bytes":100,"errors":1,"min_ms":1786915187624,"max_ms":1786915187625}]`
	rows, span, err := openCodeRows([]byte(nullErrors))
	if err != nil {
		t.Fatalf("openCodeRows: %v", err)
	}
	env := openCodeEnvelope("/oc", "test", rows, span, openCodeServerList, nil, 4096)
	if err := env.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	for _, m := range env.Metrics {
		if m.Name != "errors" {
			continue
		}
		switch {
		case m.Key == "engram_mem_search":
			t.Errorf("errors/tool/engram_mem_search = %v was emitted; the count is unrecorded, not zero", m.Value)
		case m.Dimension == "mcp_server" && m.Key == "engram":
			t.Errorf("errors/mcp_server/engram = %v was emitted; the server inherits its tool's unrecorded status", m.Value)
		case m.Dimension == "corpus":
			t.Errorf("errors/corpus = %v was emitted; one unrecorded group makes the total a floor, not a measurement", m.Value)
		}
	}
	// The group that *did* record a status keeps its measured count — omitting
	// is per key, not a whole-column surrender.
	if got := metricValue(t, env, "errors", "tool", "task"); got != int64(1) {
		t.Errorf("errors/tool/task = %v, want 1", got)
	}
	// The mcp_server row itself survives; only its errors cell is withheld.
	if got := metricValue(t, env, "context_bytes", "mcp_server", "engram"); got != int64(400) {
		t.Errorf("context_bytes/mcp_server/engram = %v, want 400", got)
	}
	// warn's contract is that nothing is omitted silently, so all three
	// withheld rows must be named, not just the tool.
	joined := strings.Join(env.Warnings, "\n")
	for _, want := range []string{
		"errors is withheld for engram_mem_search and from the corpus total",
		"errors is withheld from the mcp_server rows for engram",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning says %q:\n%s", want, joined)
		}
	}
}

// TestOpenCodeEnvelopeWithoutConfigStillBuilds is the caller half of
// openCodeServers' error: the function returns one, and non-fatality lives
// here. No server list costs the mcp_server block and a warning, nothing else.
//
// The two ways to have no list are covered together on purpose. A config that
// parses but names no servers returns (empty, nil), so the error is not what
// the warning can key off — and the reader of the table sees the same missing
// block either way, so they are owed the same explanation.
func TestOpenCodeEnvelopeWithoutConfigStillBuilds(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "opencode.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := map[string]func(t *testing.T) string{
		"no file": func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.json") },
		"no mcp key": func(t *testing.T) string {
			return write(t, `{"theme":"dark"}`)
		},
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			servers, serverErr := openCodeServers(path(t))
			if len(servers) != 0 {
				t.Fatalf("openCodeServers = %v, want none", servers)
			}
			env := openCodeFixtureEnvelope(t, servers, serverErr)
			if err := env.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			for _, m := range env.Metrics {
				if m.Dimension == "mcp_server" {
					t.Errorf("mcp_server row %s/%s emitted with no server list", m.Name, m.Key)
				}
			}
			// The tool rows are untouched — only the split is lost, not the data.
			if got := metricValue(t, env, "context_bytes", "tool", "engram_mem_search"); got != int64(3161) {
				t.Errorf("engram_mem_search context_bytes = %v, want 3161", got)
			}
			if !strings.Contains(strings.Join(env.Warnings, "\n"), "no mcp server list") {
				t.Errorf("no warning explains the missing server list: %v", env.Warnings)
			}
		})
	}
}

// TestOpenCodeTimeIsUTC pins the .UTC() in openCodeTime. The fixture's own
// extremes (21:19Z and 22:23Z) still land on 2026-08-16 everywhere from UTC-12
// to UTC+2, so every envelope test and every UTC CI box stays green with the
// call removed — while a user east of UTC+2 gets a corpus range off by a day.
// Only a non-UTC local zone catches it.
func TestOpenCodeTimeIsUTC(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	defer func(orig *time.Location) { time.Local = orig }(time.Local)
	time.Local = tokyo

	// The same instant is 2026-08-17T06:19:47+09:00 in Tokyo — a different day,
	// which is the whole point.
	if got := openCodeTime(1786915187624); got != "2026-08-16T21:19:47Z" {
		t.Errorf("openCodeTime(1786915187624) = %q, want %q", got, "2026-08-16T21:19:47Z")
	}
}
