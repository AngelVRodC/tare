package report

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// Boost source values, reported as `boost_source` so a reader always knows
// which path a number came from. "unavailable" is a finding, never a zero.
const (
	boostUnavailable = "unavailable"
	boostFromJSON    = "json"
	boostFromDeep    = "deep"
)

// boostTimeout caps every shell-out. Boost's own report takes about a second
// on a 196 MB history; a hung child must not hang tare.
const boostTimeout = 2 * time.Minute

// boostFilter is one row of `filters.builtin[]` / `filters.custom[]`. These
// aggregates are complete — unlike the event arrays below.
type boostFilter struct {
	Name          string `json:"name"`
	Source        string `json:"source"`
	Enabled       bool   `json:"enabled"`
	SavedTokens   int64  `json:"saved_tokens"`
	ContextSaved  int64  `json:"context_saved_tokens"`
	TokensBefore  int64  `json:"tokens_before"`
	TokensAfter   int64  `json:"tokens_after"`
	RetrieveCount int64  `json:"retrieve_count"`
	EventCount    int64  `json:"event_count"`
}

// boostMcpEvent is one row of `mcp_events[]`. tool_use_id is the exact join key
// into the transcript's tool_use ids — no timestamp heuristic is needed.
type boostMcpEvent struct {
	ToolUseID     string `json:"tool_use_id"`
	ToolName      string `json:"tool_name"`
	ResponseBytes int64  `json:"response_bytes"`
	FilteredBytes int64  `json:"filtered_response_bytes"`
	SavedTokens   int64  `json:"saved_tokens"`
}

// boostReport is the subset of `boost report -f json -d 0` this command reads.
//
// The two count fields matter as much as the arrays: `mcp_tool_call_count` and
// `total_commands` are the true totals, while `mcp_events` and `events` are
// truncated samples of them. Decoding both is what lets the report say so.
type boostReport struct {
	TotalCommands    int64 `json:"total_commands"`
	McpToolCallCount int64 `json:"mcp_tool_call_count"`
	McpSavedTokens   int64 `json:"mcp_saved_tokens"`
	FilesSavedTokens int64 `json:"files_optimization_saved_tokens"`
	FilesEventCount  int64 `json:"files_optimization_event_count"`

	// Events is decoded into empty structs on purpose: only its length is
	// wanted, and the real array is a thousand rows of tool output.
	Events    []struct{}      `json:"events"`
	McpEvents []boostMcpEvent `json:"mcp_events"`

	Filters struct {
		Builtin []boostFilter `json:"builtin"`
		Custom  []boostFilter `json:"custom"`
	} `json:"filters"`
}

// boostSection gathers every Boost-derived row for `tare corruption`.
//
// Nothing here is fatal. Boost missing, sqlite3 missing, the history DB moved
// — each degrades to a stated `boost_source` plus a warning, because a reader
// must be able to tell "Boost removed nothing" from "Boost was never asked".
func boostSection(useIDs map[string]string, deep bool) (metrics []Metric, warnings []string) {
	source := func(s string) Metric {
		return MeasuredMetric("boost_source", "boost", "", s, "source")
	}

	bin, err := exec.LookPath("boost")
	if err != nil {
		return []Metric{source(boostUnavailable)}, []string{
			"boost is not on PATH: the filtering counterfactual is unavailable, not zero — " +
				"per-filter and per-call figures are absent from this report"}
	}

	out, err := runCmd(bin, "report", "-f", "json", "-d", "0")
	if err != nil {
		return []Metric{source(boostUnavailable)},
			[]string{fmt.Sprintf("boost report failed: %v — counterfactual unavailable, not zero", err)}
	}
	var rep boostReport
	if err := json.Unmarshal(out, &rep); err != nil {
		return []Metric{source(boostUnavailable)},
			[]string{fmt.Sprintf("boost report is not the expected JSON shape: %v", err)}
	}

	metrics, warnings = boostReportMetrics(&rep, useIDs)

	// The retention ceiling and the deep join both come from the history DB.
	// The ceiling is a 300-row read and always worth taking; the 1,318-row
	// per-call lift is what `--boost-deep` actually buys.
	db, dbErr := boostDBPath(bin)
	if dbErr != nil {
		warnings = append(warnings, fmt.Sprintf("boost history DB not located: %v — retention ceiling unavailable", dbErr))
	} else {
		rows, warn := retentionMetrics(db)
		metrics = append(metrics, rows...)
		warnings = append(warnings, warn...)
	}

	if !deep {
		return append(metrics, source(boostFromJSON)), warnings
	}
	if dbErr != nil {
		return append(metrics, source(boostFromJSON)),
			append(warnings, "--boost-deep asked for the full per-call join but the history DB was not located: fell back to the 100-row JSON sample")
	}
	rows, warn, ok := deepJoinMetrics(db, useIDs)
	warnings = append(warnings, warn...)
	if !ok {
		return append(metrics, source(boostFromJSON)), warnings
	}
	return append(append(metrics, rows...), source(boostFromDeep)), warnings
}

// boostReportMetrics turns a decoded report into rows. Split out from the
// shell-out so the sample-versus-total contract is testable without Boost
// installed.
func boostReportMetrics(rep *boostReport, useIDs map[string]string) (metrics []Metric, warnings []string) {
	add := func(name string, value any, unit string) {
		metrics = append(metrics, MeasuredMetric(name, "boost", "", value, unit))
	}
	add("boost_cli_events_total", rep.TotalCommands, "events")
	add("boost_cli_events_sampled", int64(len(rep.Events)), "events")
	add("boost_mcp_calls_total", rep.McpToolCallCount, "calls")
	add("boost_mcp_calls_sampled", int64(len(rep.McpEvents)), "calls")
	add("boost_mcp_saved_tokens", rep.McpSavedTokens, "tokens")
	add("boost_files_saved_tokens", rep.FilesSavedTokens, "tokens")
	add("boost_files_event_count", rep.FilesEventCount, "events")
	add("boost_builtin_filters", int64(len(rep.Filters.Builtin)), "filters")
	add("boost_custom_filters", int64(len(rep.Filters.Custom)), "filters")

	var before, after, saved, fired int64
	for _, f := range rep.Filters.Builtin {
		before += f.TokensBefore
		after += f.TokensAfter
		saved += f.SavedTokens
		if f.EventCount > 0 {
			fired++
		}
	}
	add("boost_filter_tokens_before", before, "tokens")
	add("boost_filter_tokens_after", after, "tokens")
	add("boost_filter_saved_tokens", saved, "tokens")
	add("boost_filters_fired", fired, "filters")

	metrics = append(metrics, filterMetrics(rep.Filters.Builtin)...)
	metrics = append(metrics, filterMetrics(rep.Filters.Custom)...)

	// Per-call figures live on their own dimension and every name says
	// "sampled". `mcp_events` is 100 rows of 1,318 even with `-d 0`, so a
	// consumer must not be able to mistake this block for a corpus total.
	metrics = append(metrics, joinMetrics("boost_mcp_sample", "sampled_", rep.McpEvents, useIDs)...)
	var sampleSaved int64
	for _, e := range rep.McpEvents {
		sampleSaved += e.SavedTokens
	}
	metrics = append(metrics,
		MeasuredMetric("sampled_saved_tokens", "boost_mcp_sample", "", sampleSaved, "tokens"))
	if n := int64(len(rep.McpEvents)); n < rep.McpToolCallCount {
		warnings = append(warnings, fmt.Sprintf(
			"boost per-call MCP figures cover %d of %d calls (%.1f%%): the JSON interface truncates "+
				"mcp_events regardless of -d 0. Per-filter aggregates are complete; these are a sample. "+
				"Use --boost-deep for all %d",
			n, rep.McpToolCallCount, percent(n, rep.McpToolCallCount), rep.McpToolCallCount))
	}
	if n := int64(len(rep.Events)); n < rep.TotalCommands {
		warnings = append(warnings, fmt.Sprintf(
			"boost per-event CLI figures cover %d of %d commands: a sample, not a total — "+
				"tare reads only the complete aggregates from it", n, rep.TotalCommands))
	}
	return metrics, warnings
}

// filterMetrics emits six rows per filter, every filter, heaviest claimed
// volume first. These aggregates are complete, so the envelope carries all of
// them — an installed filter that saved nothing is exactly the finding this
// tool exists to surface. The human table caps itself; the JSON never does.
//
// Ordering by TokensBefore rather than SavedTokens is the whole point. A
// filter that claims a third of the corpus and returns 2% is corruption for
// no compression, but under a saved-first sort it ranks among the winners on
// the strength of the volume it claimed. Claimed volume ranks; saved_rate_percent
// convicts.
func filterMetrics(filters []boostFilter) []Metric {
	sorted := slices.SortedFunc(slices.Values(filters), func(a, b boostFilter) int {
		return cmp.Or(
			cmp.Compare(b.TokensBefore, a.TokensBefore),
			cmp.Compare(b.SavedTokens, a.SavedTokens),
			cmp.Compare(b.EventCount, a.EventCount),
			strings.Compare(a.Name, b.Name))
	})
	out := make([]Metric, 0, len(sorted)*6)
	for _, f := range sorted {
		key := f.Name
		if f.Source != "" {
			key = f.Source + "/" + f.Name
		}
		out = append(out,
			MeasuredMetric("event_count", "boost_filter", key, f.EventCount, "events"),
			MeasuredMetric("tokens_before", "boost_filter", key, f.TokensBefore, "tokens"),
			MeasuredMetric("tokens_after", "boost_filter", key, f.TokensAfter, "tokens"),
			MeasuredMetric("saved_tokens", "boost_filter", key, f.SavedTokens, "tokens"),
			MeasuredMetric("saved_rate_percent", "boost_filter", key, percent(f.SavedTokens, f.TokensBefore), "percent"),
			MeasuredMetric("retrieve_count", "boost_filter", key, f.RetrieveCount, "retrieves"))
	}
	return out
}

// joinMetrics joins Boost's per-call rows to the transcript's tool_use ids and
// totals the bytes on both sides of the filter. `joined` is the honest
// denominator: a Boost row whose transcript has since been pruned cannot be
// attributed to a tool.
//
// prefix carries the provenance into every row name, because the same shape
// is emitted twice from sources of very different standing: "sampled_" for
// the 100-row JSON array, and nothing for the complete `--boost-deep` table.
func joinMetrics(dimension, prefix string, events []boostMcpEvent, useIDs map[string]string) []Metric {
	var joined, unjoined, response, filtered int64
	for _, e := range events {
		if _, ok := useIDs[e.ToolUseID]; ok {
			joined++
		} else {
			unjoined++
		}
		response += e.ResponseBytes
		filtered += e.FilteredBytes
	}
	add := func(name string, value any, unit string) Metric {
		return MeasuredMetric(prefix+name, dimension, "", value, unit)
	}
	return []Metric{
		add("calls", int64(len(events)), "calls"),
		add("calls_joined", joined, "calls"),
		add("calls_unjoined", unjoined, "calls"),
		add("join_rate_percent", percent(joined, int64(len(events))), "percent"),
		add("response_bytes", response, "bytes"),
		add("filtered_response_bytes", filtered, "bytes"),
		add("removed_bytes", response-filtered, "bytes"),
	}
}

// deepJoinMetrics lifts the per-call join off the 100-row JSON sample and onto
// the whole mcp_tool_calls table, by shelling out to the sqlite3 that ships
// with macOS. No Go dependency is added.
//
// It selects exactly three columns. A Boost schema change then fails loudly
// here instead of producing a quietly wrong number somewhere downstream.
func deepJoinMetrics(db string, useIDs map[string]string) (metrics []Metric, warnings []string, ok bool) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil, []string{"--boost-deep needs sqlite3 and it is not on PATH: fell back to the 100-row JSON sample"}, false
	}
	const query = `select tool_use_id, response_bytes, filtered_response_bytes from mcp_tool_calls`
	out, err := runCmd(bin, "-readonly", "-json", db, query)
	if err != nil {
		return nil, []string{fmt.Sprintf(
			"--boost-deep query failed (schema change or locked DB?): %v — fell back to the 100-row JSON sample", err)}, false
	}
	var rows []struct {
		ToolUseID     string `json:"tool_use_id"`
		ResponseBytes int64  `json:"response_bytes"`
		FilteredBytes int64  `json:"filtered_response_bytes"`
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		if err := json.Unmarshal(out, &rows); err != nil {
			return nil, []string{fmt.Sprintf("--boost-deep result is not the expected shape: %v", err)}, false
		}
	}

	events := make([]boostMcpEvent, len(rows))
	for i, r := range rows {
		events[i] = boostMcpEvent{
			ToolUseID:     r.ToolUseID,
			ResponseBytes: r.ResponseBytes,
			FilteredBytes: r.FilteredBytes,
		}
	}
	// No saved_tokens row: the query reads three columns by design, so that
	// figure was never asked for. An unasked number is not a zero.
	metrics = joinMetrics("boost_mcp_deep", "", events, useIDs)
	joined := int64(0)
	for _, e := range events {
		if _, ok := useIDs[e.ToolUseID]; ok {
			joined++
		}
	}
	if n := int64(len(events)); n > joined {
		warnings = append(warnings, fmt.Sprintf(
			"--boost-deep joined %d of %d MCP calls (%.1f%%): the rest name a tool_use_id whose "+
				"transcript has been pruned, which is the retention ceiling and not a join defect",
			joined, n, percent(joined, n)))
	}
	return metrics, warnings, true
}

// retentionMetrics reports the hard ceiling on the counterfactual: Boost
// outlives the transcripts it annotates, so a large share of its history can
// never be joined back to a `.jsonl` that still exists.
//
// Measured 2026-09-05: 124 of 285 referenced transcripts still on disk. A
// skeptic re-running this has to see why the join covers less than the whole
// history before being shown any per-call figure.
func retentionMetrics(db string) (metrics []Metric, warnings []string) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil, []string{"sqlite3 is not on PATH: the Boost retention ceiling is unavailable, not zero"}
	}
	const query = `select transcript_path from conversations where transcript_path != ''`
	out, err := runCmd(bin, "-readonly", "-json", db, query)
	if err != nil {
		return nil, []string{fmt.Sprintf("boost retention query failed: %v — ceiling unavailable, not zero", err)}
	}
	var rows []struct {
		Path string `json:"transcript_path"`
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		if err := json.Unmarshal(out, &rows); err != nil {
			return nil, []string{fmt.Sprintf("boost retention result is not the expected shape: %v", err)}
		}
	}

	var present int64
	for _, r := range rows {
		if _, err := os.Stat(r.Path); err == nil {
			present++
		}
	}
	referenced := int64(len(rows))
	add := func(name string, value any, unit string) {
		metrics = append(metrics, MeasuredMetric(name, "boost", "", value, unit))
	}
	add("boost_transcripts_referenced", referenced, "transcripts")
	add("boost_transcripts_on_disk", present, "transcripts")
	add("boost_transcripts_pruned", referenced-present, "transcripts")
	add("boost_retention_percent", percent(present, referenced), "percent")
	if pruned := referenced - present; pruned > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d of %d Boost-referenced transcripts have been pruned by Claude Code (%.1f%% joinable): "+
				"Boost outlives the transcripts it annotates, and this is the ceiling on the counterfactual",
			pruned, referenced, percent(present, referenced)))
	}
	return metrics, warnings
}

// boostDBPath asks `boost doctor` where the history DB lives, rather than
// hardcoding a path that differs per OS and per install.
func boostDBPath(bin string) (string, error) {
	out, err := runCmd(bin, "doctor")
	if err != nil {
		return "", err
	}
	return parseDoctorDB(string(out))
}

// parseDoctorDB pulls the history DB path out of `boost doctor` output.
func parseDoctorDB(out string) (string, error) {
	const label = "History DB:"
	for line := range strings.Lines(out) {
		_, rest, found := strings.Cut(line, label)
		if !found {
			continue
		}
		rest = strings.TrimSpace(rest)
		// The line reads `History DB: <path> (195.9 MB)`; the size is the last
		// parenthesised group, so trim from the last " (" rather than the first.
		if i := strings.LastIndex(rest, " ("); i > 0 {
			rest = rest[:i]
		}
		if rest != "" {
			return rest, nil
		}
	}
	return "", fmt.Errorf("no %q line in boost doctor output", label)
}

// runCmd runs a child to completion under a timeout and returns its stdout.
// Nothing here touches the network; these are local binaries reading local
// files, and stderr is folded into the error so a failure is never silent.
func runCmd(bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), boostTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", bin, err, msg)
		}
		return nil, fmt.Errorf("%s: %w", bin, err)
	}
	return out, nil
}
