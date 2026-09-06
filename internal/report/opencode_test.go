package report

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	_, _, err := openCodeRead(t.TempDir())
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
