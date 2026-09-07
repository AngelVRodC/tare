package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFormatValueByUnit pins the shape of every unit the envelope carries. The
// unit decides the rendering, so a regression here is a regression in every
// table at once.
func TestFormatValueByUnit(t *testing.T) {
	cases := []struct {
		unit  string
		value any
		want  string
	}{
		// bytes: SI, and int and int64 must agree.
		{"bytes", int64(254427880), "254.4 MB"},
		{"bytes", int64(0), "0 B"},
		{"bytes", int64(999), "999 B"},
		{"bytes", int64(1000), "1.0 kB"},
		{"bytes", 1500, "1.5 kB"},

		// percent: one decimal, never six significant figures.
		{"percent", 24.813621, "24.8%"},
		{"percent", 100.0, "100.0%"},
		{"percent", 0.088123, "0.1%"},
		{"percent", 0.0, "0.0%"},

		// usd: money to the cent, except below a cent, where rounding to
		// $0.00 would delete a real allocation.
		{"usd", 813.0334, "$813.03"},
		{"usd", 0.07878235507735767, "$0.08"},
		{"usd", 0.0000149, "$0.0000149"},
		{"usd", 0.0, "$0.00"},

		// ratio always carries the ×; only the precision scales. A
		// rebill_multiplier below 1 is real and must survive rounding.
		{"ratio", 0.5169169885527817, "0.52×"},
		{"ratio", 0.664, "0.66×"},
		{"ratio", 1.0, "1.0×"},
		{"ratio", 0.0, "0.00×"},
		{"ratio", 2.7, "2.7×"},
		{"ratio", 28.1045, "28×"},
		{"ratio", 17070.5, "17,071×"},

		// Every other unit keeps the pre-existing rendering.
		{"tokens", int64(14356), "14,356"},
		{"events", 60, "60"},
		{"sessions", int64(-3), "-3"},
		{"source", "json", "json"},
		// An omitted metric arrives as a nil value; a blank cell says "not
		// measured" where "<nil>" says the renderer broke and 0 would be a
		// claim nobody measured.
		{"", nil, ""},
	}
	for _, c := range cases {
		if got := formatValue(c.value, c.unit); got != c.want {
			t.Errorf("formatValue(%v, %q) = %q, want %q", c.value, c.unit, got, c.want)
		}
	}
}

// TestFormatValueMicrodollars pins the money rendering: an integer count of
// microdollars renders through formatUSD as the dollar amount it is. The int
// case is accepted alongside int64, per the bytes-case precedent.
func TestFormatValueMicrodollars(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{int64(1234567), formatUSD(1.234567)},
		// Sub-cent allocations are the point of the per-skill table: they
		// render exactly, never rounded away to $0.00.
		{int64(149), formatUSD(0.000149)},
		{int64(0), "$0.00"},
		{int64(-1234567), formatUSD(-1.234567)},
		{1234567, formatUSD(1.234567)},
	}
	for _, c := range cases {
		if got := formatValue(c.value, "microdollars"); got != c.want {
			t.Errorf("formatValue(%v, %q) = %q, want %q", c.value, "microdollars", got, c.want)
		}
	}
}

// TestHumanizeBytesIsSI is the guard against the most common humanization bug
// there is: dividing by 1024 and labelling the result MB. tare measures bytes
// on disk, so it divides by 1000 and the two systems are never mixed.
func TestHumanizeBytesIsSI(t *testing.T) {
	if got := humanizeBytes(254427880); got != "254.4 MB" {
		t.Errorf("humanizeBytes(254427880) = %q, want %q (SI, not the IEC 242.6 MiB)", got, "254.4 MB")
	}
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},     // the B/kB boundary, below
		{1000, "1.0 kB"},   // and above
		{999999, "1.0 MB"}, // promotes on the rounded value, not the raw one
		{1000000, "1.0 MB"},
		{1500000000, "1.5 GB"},
		{2000000000000, "2.0 TB"},
		{-1500, "-1.5 kB"},
	}
	for _, c := range cases {
		if got := humanizeBytes(c.in); got != c.want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestHumanizeBytesPromotesOnRoundedValue guards the boundary the scale loop
// gets wrong if it tests the raw quotient: 999,999 B is 999.999 kB, which
// prints as "1000.0 kB" unless it is promoted first.
func TestHumanizeBytesPromotesOnRoundedValue(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{999949, "999.9 kB"}, // rounds to 999.9 — stays kB
		{999950, "1.0 MB"},   // rounds to 1000.0 — promotes
		{999999999, "1.0 GB"},
	} {
		if got := humanizeBytes(c.in); got != c.want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLabelFallsThrough pins the contract that keeps the labels map optional:
// a mapped name reads as English, an unmapped one prints as it is. A metric
// added without a label degrades to its JSON name, never to an empty cell.
func TestLabelFallsThrough(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		// The gap is the transcripts Claude Code has forgotten, and the label
		// has to say that — "Retention gap" only restates the subtraction.
		{"retention_gap", "Sessions no longer on disk"},
		{"files_top_level", "Top-level sessions"},
		// Unmapped: a new metric, and a key that must never be rewritten.
		{"some_future_metric", "some_future_metric"},
		{"mcp__context7__query-docs", "mcp__context7__query-docs"},
		{"", ""},
	}
	for _, c := range cases {
		if got := label(c.name); got != c.want {
			t.Errorf("label(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// scanFixture writes one transcript file holding the given lines and returns
// its directory. Every fixture stays under t.TempDir(); the live corpus is
// never a test input.
func scanFixture(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// scanEvent is a minimal decoded transcript event with a timestamp. ts may be
// empty, which is how an untimestamped event type is written into a fixture.
func scanEvent(ts, typ string) string {
	line := `{"type":"` + typ + `","sessionId":"s1"`
	if ts != "" {
		line += `,"timestamp":"` + ts + `"`
	}
	return line + `}`
}

// TestWindowedScanCountsEventsNotLines is the time-window-3 gate: `events`
// counts in-window events, never stats.Lines, which accumulates before the
// window can speak — a line the window cannot place (out of range, or refused
// by the decoder) must not inflate it. files, bytes and parse_errors stay
// corpus-wide, which is what makes the two figures differ here.
func TestWindowedScanCountsEventsNotLines(t *testing.T) {
	dir := scanFixture(t,
		scanEvent("2026-08-01T10:00:00.000Z", "user"),
		scanEvent("2026-08-15T12:00:00.000Z", "assistant"),
		scanEvent("2026-08-20T09:00:00.000Z", "user"),
		"not json at all",
	)
	w, err := NewWindow("2026-08-10", "2026-08-16")
	if err != nil {
		t.Fatal(err)
	}
	env, err := ScanEnvelope(dir, "test", w)
	if err != nil {
		t.Fatalf("ScanEnvelope: %v", err)
	}

	// One event sits inside the window; one is out of range below it, one
	// out of range above it, and one never decoded. The old stats.Lines
	// figure for this corpus was 4.
	if got := metricValue(t, env, "events", "corpus", ""); got != 1 {
		t.Errorf("events = %v, want 1 — in-window events, not lines", got)
	}
	if got := metricValue(t, env, "files", "corpus", ""); got != 1 {
		t.Errorf("files = %v, want 1 — corpus totals stay corpus-wide", got)
	}
	if got := metricValue(t, env, "parse_errors", "corpus", ""); got != 1 {
		t.Errorf("parse_errors = %v, want 1 — corpus-wide under a window", got)
	}
	// Per-type rows follow the window too.
	if got := metricValue(t, env, "events", "type", "assistant"); got != 1 {
		t.Errorf("events/type/assistant = %v, want 1", got)
	}
	for _, m := range env.Metrics {
		if m.Name == "events" && m.Dimension == "type" && m.Key == "user" {
			t.Errorf("events/type/user = %v emitted — both user events sit outside the window", m.Value)
		}
	}
}

// TestBareDateUntilIncludesWholeDay is the time-window-4 asymmetry gate: a
// bare-date --until compares day(ts) <= until, so the whole named day is in —
// its first midnight and its last millisecond alike — and the next day is out.
func TestBareDateUntilIncludesWholeDay(t *testing.T) {
	dir := scanFixture(t,
		scanEvent("2026-08-15T00:00:00.000Z", "user"),
		scanEvent("2026-08-15T23:59:59.999Z", "assistant"),
		scanEvent("2026-08-16T00:00:00.000Z", "user"),
	)
	w, err := NewWindow("", "2026-08-15")
	if err != nil {
		t.Fatal(err)
	}
	env, err := ScanEnvelope(dir, "test", w)
	if err != nil {
		t.Fatalf("ScanEnvelope: %v", err)
	}
	if got := metricValue(t, env, "events", "corpus", ""); got != 2 {
		t.Errorf("events = %v, want 2 — a bare --until includes the whole named day", got)
	}
}

// TestFullPrecisionUntilInclusive pins the other half of time-window-4: a
// full-precision --until includes the event exactly at the instant.
func TestFullPrecisionUntilInclusive(t *testing.T) {
	dir := scanFixture(t,
		scanEvent("2026-08-15T11:59:59.999Z", "assistant"),
		scanEvent("2026-08-15T12:00:00.000Z", "user"),
		scanEvent("2026-08-15T12:00:00.001Z", "assistant"),
	)
	w, err := NewWindow("2026-08-15T00:00:00.000Z", "2026-08-15T12:00:00.000Z")
	if err != nil {
		t.Fatal(err)
	}
	env, err := ScanEnvelope(dir, "test", w)
	if err != nil {
		t.Fatalf("ScanEnvelope: %v", err)
	}
	if got := metricValue(t, env, "events", "corpus", ""); got != 2 {
		t.Errorf("events = %v, want 2 — the event exactly at --until is included", got)
	}
	if got := metricValue(t, env, "events", "type", "user"); got != 1 {
		t.Errorf("events/type/user = %v, want 1 — the boundary event is the user's", got)
	}
}

// TestParseErrorsStayCorpusWide is time-window-7: a line the decoder refused
// has no timestamp to window by, so parse_errors covers the whole corpus even
// when the window matches nothing.
func TestParseErrorsStayCorpusWide(t *testing.T) {
	dir := scanFixture(t,
		scanEvent("2026-08-15T12:00:00.000Z", "user"),
		"{",
	)
	w, err := NewWindow("2027-01-01", "2027-01-31")
	if err != nil {
		t.Fatal(err)
	}
	env, err := ScanEnvelope(dir, "test", w)
	if err != nil {
		t.Fatalf("ScanEnvelope: %v", err)
	}
	if got := metricValue(t, env, "parse_errors", "corpus", ""); got != 1 {
		t.Errorf("parse_errors = %v, want 1 — corpus-wide under a window", got)
	}
	if got := metricValue(t, env, "files", "corpus", ""); got != 1 {
		t.Errorf("files = %v, want 1 — corpus totals stay corpus-wide", got)
	}
}

// TestEmptyWindowOmitsNotZeroes is time-window-8: when the window matches no
// events, the windowed KEYLESS rows are omitted — a measured zero the window
// emptied is an over-claim nothing downstream can catch — while corpus rows
// still render (they remain true) and exactly one warning states the miss.
func TestEmptyWindowOmitsNotZeroes(t *testing.T) {
	dir := scanFixture(t,
		scanEvent("2026-08-15T12:00:00.000Z", "user"),
		scanEvent("2026-08-15T13:00:00.000Z", "assistant"),
	)
	w, err := NewWindow("2027-01-01", "2027-01-31")
	if err != nil {
		t.Fatal(err)
	}
	env, err := ScanEnvelope(dir, "test", w)
	if err != nil {
		t.Fatalf("ScanEnvelope: %v", err)
	}

	for _, name := range []string{"events", "distinct_event_types", "distinct_cli_versions"} {
		if m := findMetric(env, name, "corpus", ""); m != nil {
			t.Errorf("%s = %v emitted over an empty window — omit, never zero", name, m.Value)
		}
	}
	// Corpus rows still render: they remain true.
	for _, name := range []string{"files", "bytes"} {
		if findMetric(env, name, "corpus", "") == nil {
			t.Errorf("%s was omitted — corpus totals stay corpus-wide even when the window is empty", name)
		}
	}
	// Exactly one warning states the miss.
	n := 0
	for _, warn := range env.Warnings {
		if strings.Contains(warn, "matched no events") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d no-match warnings, want exactly one: %v", n, env.Warnings)
	}
	// Corpus.Since/Until carry the bounds the run was measured with.
	if env.Corpus.Since != "2027-01-01" || env.Corpus.Until != "2027-01-31" {
		t.Errorf("corpus window = %q..%q, want 2027-01-01..2027-01-31", env.Corpus.Since, env.Corpus.Until)
	}
}

// TestWindowedWarningC1Rendered pins time-window-9 by rendering, not by
// Validate: a windowed run's output names all four C1 items — the subset
// semantics, what stayed corpus-wide, the untimestamped types excluded (named
// unavailable, never zero) and the session granularity of cost.
func TestWindowedWarningC1Rendered(t *testing.T) {
	dir := scanFixture(t,
		scanEvent("2026-08-15T12:00:00.000Z", "user"),
		// A type with no timestamp: the window cannot place it, so it is
		// excluded from the counts and named in the warning.
		scanEvent("", "mode"),
	)
	w, err := NewWindow("2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatal(err)
	}
	env, err := ScanEnvelope(dir, "test", w)
	if err != nil {
		t.Fatalf("ScanEnvelope: %v", err)
	}
	var buf strings.Builder
	if err := RenderScan(&buf, env); err != nil {
		t.Fatalf("RenderScan: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"a lexical subset of the corpus", // (a) subset semantics
		"remain whole-corpus",            // (b) corpus-wide items
		"mode",                           // (c) the untimestamped type, by name
		"reported unavailable, not zero", // (c) omission, never a zero
		"session-granular",               // (d) cost joins at session granularity
		"billed wholly to it",            // (d)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("windowed output is missing %q\n%s", want, out)
		}
	}
}
