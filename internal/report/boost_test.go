package report

import (
	"slices"
	"strings"
	"testing"
)

// TestNoBoostPresent is the graceful-degradation gate. With nothing on PATH the
// command must still produce a report, and must say the counterfactual is
// unavailable rather than quietly reporting a smaller one.
func TestNoBoostPresent(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // an empty directory: no boost, no sqlite3
	dir := toolCorpus(t, use("t1", "Bash"), result("t1", `"hello"`, false))

	env, err := CorruptionEnvelope(dir, "test", true) // --boost-deep too
	if err != nil {
		t.Fatalf("CorruptionEnvelope must not fail when boost is absent: %v", err)
	}
	if got := metricValue(t, env, "boost_source", "boost", ""); got != boostUnavailable {
		t.Errorf("boost_source = %v, want %q", got, boostUnavailable)
	}
	// The transcript-only half still has to work.
	if got := metricValue(t, env, "calls", "tool", "Bash"); got != int64(1) {
		t.Errorf("Bash calls = %v, want 1 — transcript signals do not depend on boost", got)
	}
	joined := strings.Join(env.Warnings, "\n")
	if !strings.Contains(joined, "not on PATH") || !strings.Contains(joined, "not zero") {
		t.Errorf("warnings must state boost is absent and that absent is not zero: %v", env.Warnings)
	}
	// Absent is not a table of zeros: no Boost figure may be emitted at all.
	for _, m := range env.Metrics {
		if strings.HasPrefix(m.Dimension, "boost") && m.Name != "boost_source" {
			t.Errorf("metric %s/%s emitted with no boost installed", m.Dimension, m.Name)
		}
	}
}

// TestSampleNotTotal is the honesty gate on Boost's truncated event arrays.
// `boost report -f json -d 0` returns 100 of 1,318 mcp_events and 1,000 of
// 9,334 events; the per-filter aggregates are complete. Nothing derived from a
// sample may be presentable as a corpus total.
func TestSampleNotTotal(t *testing.T) {
	rep := &boostReport{
		TotalCommands:    9334,
		McpToolCallCount: 1318,
		Events:           make([]struct{}, 1000),
		McpEvents: []boostMcpEvent{
			{ToolUseID: "toolu_known", ResponseBytes: 1000, FilteredBytes: 400, SavedTokens: 150},
			{ToolUseID: "toolu_pruned", ResponseBytes: 500, FilteredBytes: 500},
		},
	}
	rep.Filters.Builtin = []boostFilter{
		{Name: "jq", Source: "builtin", EventCount: 12, TokensBefore: 900, TokensAfter: 300, SavedTokens: 600},
		{Name: "silent", Source: "builtin"},
	}
	useIDs := map[string]string{"toolu_known": "mcp__context7__query-docs"}

	metrics, warnings := boostReportMetrics(rep, useIDs)
	env := Envelope{Metrics: metrics}

	if got := metricValue(t, env, "boost_mcp_calls_total", "boost", ""); got != int64(1318) {
		t.Errorf("boost_mcp_calls_total = %v, want 1318", got)
	}
	if got := metricValue(t, env, "boost_mcp_calls_sampled", "boost", ""); got != int64(2) {
		t.Errorf("boost_mcp_calls_sampled = %v, want 2", got)
	}
	if got := metricValue(t, env, "boost_cli_events_total", "boost", ""); got != int64(9334) {
		t.Errorf("boost_cli_events_total = %v, want 9334", got)
	}

	// Every per-call byte figure lives on its own dimension and says "sampled".
	// A consumer reading only the `boost` dimension can never pick one up.
	for _, m := range metrics {
		if m.Dimension == "boost_mcp_sample" && !strings.HasPrefix(m.Name, "sampled_") {
			t.Errorf("per-call metric %q does not name itself a sample", m.Name)
		}
		if m.Dimension == "boost" && strings.Contains(m.Name, "response_bytes") {
			t.Errorf("per-call byte metric %q leaked onto the aggregate dimension", m.Name)
		}
	}
	if got := metricValue(t, env, "sampled_removed_bytes", "boost_mcp_sample", ""); got != int64(600) {
		t.Errorf("sampled_removed_bytes = %v, want 600", got)
	}
	if got := metricValue(t, env, "sampled_calls_joined", "boost_mcp_sample", ""); got != int64(1) {
		t.Errorf("sampled_calls_joined = %v, want 1", got)
	}
	if got := metricValue(t, env, "sampled_calls_unjoined", "boost_mcp_sample", ""); got != int64(1) {
		t.Errorf("sampled_calls_unjoined = %v, want 1", got)
	}

	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "2 of 1318") || !strings.Contains(joined, "sample") {
		t.Errorf("a warning must state the sample against its true total: %v", warnings)
	}
	if !strings.Contains(joined, "1000 of 9334") {
		t.Errorf("a warning must state the CLI event sample against its total: %v", warnings)
	}

	// Per-filter aggregates are complete, so the filter count is a total and a
	// silent filter is still counted in it.
	if got := metricValue(t, env, "boost_builtin_filters", "boost", ""); got != int64(2) {
		t.Errorf("boost_builtin_filters = %v, want 2 — every filter counts, fired or not", got)
	}
	if got := metricValue(t, env, "boost_filters_fired", "boost", ""); got != int64(1) {
		t.Errorf("boost_filters_fired = %v, want 1", got)
	}
	if got := metricValue(t, env, "saved_tokens", "boost_filter", "builtin/jq"); got != int64(600) {
		t.Errorf("jq saved_tokens = %v, want 600", got)
	}
	// Every filter gets a row, fired or silent: an installed filter that saved
	// nothing is exactly the finding, and the aggregates are complete enough
	// to say so. On the real corpus this is all 221 builtins.
	var rows int
	for _, m := range metrics {
		if m.Dimension == "boost_filter" && m.Name == "event_count" {
			rows++
		}
	}
	if rows != 2 {
		t.Errorf("boost_filter rows = %d, want 2 — the envelope must carry every filter", rows)
	}
	if got := metricValue(t, env, "event_count", "boost_filter", "builtin/silent"); got != int64(0) {
		t.Errorf("silent filter event_count = %v, want an explicit 0 row", got)
	}
}

// TestBoostDBPath covers the `boost doctor` line parse, including the size
// suffix that must not end up in the path.
func TestBoostDBPath(t *testing.T) {
	const out = "boost doctor\n\nData\n----\n" +
		"History DB: /Users/x/Library/Application Support/boost/history.db (195.9 MB)\n" +
		"  WAL: 5.5 MB\n"
	got, err := parseDoctorDB(out)
	if err != nil {
		t.Fatalf("parseDoctorDB: %v", err)
	}
	const want = "/Users/x/Library/Application Support/boost/history.db"
	if got != want {
		t.Errorf("parseDoctorDB = %q, want %q", got, want)
	}
	if _, err := parseDoctorDB("boost doctor\nno such line\n"); err == nil {
		t.Errorf("a doctor output with no History DB line must be an error, not an empty path")
	}
}

// TestFilterRankedByClaimedVolume is the gate on the ordering defect this
// table exists to avoid. The fixture is the measured shape on the real corpus:
// `psql` claims a third of everything Boost touched and returns 2%, while
// `html` claims almost nothing and returns 95%. Ranked by saved tokens, psql
// reads as a top performer. Ranked by claimed volume with a rate beside it,
// the corruption-for-no-compression case is the first line you see.
func TestFilterRankedByClaimedVolume(t *testing.T) {
	metrics := filterMetrics([]boostFilter{
		{Name: "html", Source: "builtin", EventCount: 4, TokensBefore: 16026, TokensAfter: 772, SavedTokens: 15254},
		{Name: "psql", Source: "builtin", EventCount: 143, TokensBefore: 282977, TokensAfter: 277077, SavedTokens: 5900},
		{Name: "silent", Source: "builtin"},
	})
	env := Envelope{Metrics: metrics}

	var order []string
	for _, m := range metrics {
		if m.Name == "event_count" {
			order = append(order, m.Key)
		}
	}
	want := []string{"builtin/psql", "builtin/html", "builtin/silent"}
	if !slices.Equal(order, want) {
		t.Errorf("filter order = %v, want %v — heaviest claimed volume must rank first", order, want)
	}

	// 5,900 of 282,977 is 2.08%: the number that convicts the top row.
	if got := metricValue(t, env, "saved_rate_percent", "boost_filter", "builtin/psql").(float64); got < 2.07 || got > 2.09 {
		t.Errorf("psql saved_rate_percent = %v, want ~2.08", got)
	}
	if got := metricValue(t, env, "saved_rate_percent", "boost_filter", "builtin/html").(float64); got < 95.1 || got > 95.2 {
		t.Errorf("html saved_rate_percent = %v, want ~95.18", got)
	}
	// A filter that never fired divides by zero. It reports 0, not NaN.
	if got := metricValue(t, env, "saved_rate_percent", "boost_filter", "builtin/silent").(float64); got != 0 {
		t.Errorf("silent saved_rate_percent = %v, want an explicit 0", got)
	}
}
