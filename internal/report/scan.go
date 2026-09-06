package report

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/AngelVRodC/tare/internal/transcript"
)

// ScanEnvelope walks dir and returns the corpus inventory: files, bytes, date
// range, per-type and per-version event counts, the top-level vs `subagents/`
// split, unknown types and the retention gap. Every row is measured.
func ScanEnvelope(dir, version string) (Envelope, error) {
	types := map[string]int{}
	versions := map[string]int{}
	b := newBuilder(dir, version, "scan")

	stats, err := transcript.Scan(dir, func(ev *transcript.Event) {
		b.see(ev)
		types[ev.Type]++
		if ev.Version != "" {
			versions[ev.Version]++
		}
	})
	if err != nil {
		return Envelope{}, err
	}

	topLevel := stats.Files - stats.SubagentFiles
	add := b.add
	add("files", stats.Files, "files")
	add("bytes", stats.Bytes, "bytes")
	add("events", stats.Lines, "events")
	add("files_top_level", topLevel, "files")
	add("files_subagent", stats.SubagentFiles, "files")
	add("distinct_event_types", len(types), "types")
	add("known_event_types", transcript.KnownTypeCount(), "types")
	add("distinct_cli_versions", len(versions), "versions")
	add("parse_errors", stats.ParseErrors, "errors")

	// Retention gap: Claude Code's own session count against the transcripts
	// still on disk. stats-cache.json is a sibling of the projects directory.
	cachePath := filepath.Join(filepath.Dir(filepath.Clean(dir)), "stats-cache.json")
	if sessions, err := cachedSessions(cachePath); err != nil {
		b.warn("retention gap unavailable: %v", err)
	} else {
		add("stats_cache_sessions", sessions, "sessions")
		add("retention_gap", sessions-topLevel, "sessions")
	}

	for _, t := range slices.Sorted(maps.Keys(types)) {
		b.rows(MeasuredMetric("events", "type", t, types[t], "events"))
	}
	for _, v := range slices.Sorted(maps.Keys(versions)) {
		b.rows(MeasuredMetric("events", "cli_version", v, versions[v], "events"))
	}
	for _, t := range slices.Sorted(maps.Keys(stats.UnknownTypes)) {
		b.rows(MeasuredMetric("unknown_type_events", "unknown_type", t, stats.UnknownTypes[t], "events"))
		b.warn("unknown event type %q seen %d times — counted, not parsed", t, stats.UnknownTypes[t])
	}
	return b.done(stats), nil
}

// cachedSessions reads totalSessions out of Claude Code's stats-cache.json.
func cachedSessions(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var cache struct {
		TotalSessions int `json:"totalSessions"`
	}
	if err := json.Unmarshal(b, &cache); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return cache.TotalSessions, nil
}

// RenderScan prints the envelope as a table. It reads only the envelope, so
// the table and `--json` can never disagree.
func RenderScan(w io.Writer, env Envelope) error {
	renderHeader(w, env)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	section, uniform := "", ""
	for _, m := range env.Metrics {
		if m.Dimension != section {
			writeDerivationFooter(tw, uniform, "\t\t")
			section = m.Dimension
			uniform = uniformDerivation(env.Metrics, section)
			fmt.Fprintf(tw, "\n%s\t\t\n", strings.ToUpper(section))
		}
		// A keyed row prints its key verbatim: keys are measured corpus data,
		// so only the keyless name has a human half to look up.
		row := label(m.Name)
		if m.Key != "" {
			row = m.Key
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", row, formatValue(m.Value, m.Unit), derivationCell(m, uniform))
	}
	writeDerivationFooter(tw, uniform, "\t\t")
	return renderTail(w, tw, env)
}

// uniformDerivation returns the one derivation every row of a dimension
// carries, or "" when the dimension mixes them.
//
// A column printing the same word on all 60 rows is a column that says
// nothing — it is the defect this reports. Where the field genuinely varies it
// is the whole point of the metric, and stays a column.
func uniformDerivation(metrics []Metric, dimension string) string {
	seen := ""
	for _, m := range metrics {
		if m.Dimension != dimension {
			continue
		}
		if seen == "" {
			seen = m.Derivation
		} else if seen != m.Derivation {
			return ""
		}
	}
	return seen
}

// derivationCell prints a row's derivation only where its block mixes them.
// The design constraint that every metric carries a rendered Derivation is
// still met: a uniform block renders it once as a footer instead of 60 times
// as a column, and --json keeps the per-row tag either way.
func derivationCell(m Metric, uniform string) string {
	if uniform != "" {
		return ""
	}
	return m.Derivation
}

// writeDerivationFooter closes a uniform block with the one line the column
// collapsed into. pad carries the trailing empty cells the table needs.
func writeDerivationFooter(tw io.Writer, uniform, pad string) {
	if uniform != "" {
		fmt.Fprintf(tw, "all rows %s%s\n", uniform, pad)
	}
}

// renderHeader is the two identifying lines every table starts with.
func renderHeader(w io.Writer, env Envelope) {
	fmt.Fprintf(w, "tare %s — %s\n", env.Command, env.Corpus.Dir)
	fmt.Fprintf(w, "%s .. %s\n", env.Corpus.From, env.Corpus.To)
}

// renderTail flushes the table and prints the warnings under it. They stay out
// of the table on purpose: a warning is what the command could not measure.
func renderTail(w io.Writer, tw *tabwriter.Writer, env Envelope) error {
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, warn := range env.Warnings {
		fmt.Fprintf(w, "\nwarning: %s", warn)
	}
	if len(env.Warnings) > 0 {
		fmt.Fprintln(w)
	}
	return nil
}

// labels translates the envelope's metric names into what they mean. The JSON
// name is the contract and does not move; this is the human half of it. A name
// with no entry prints as it is, so this map is never required to be complete.
//
// One map serves every command, so a name emitted by two commands needs a
// label that is true in both: `calls`, `errors`, `distinct_tools` and
// `unmatched_results` are shared by `tools` and `corruption`, and `calls` again
// by the Boost per-call join. A per-command map would let the wording drift
// apart for no reader benefit.
//
// It covers only keyless rows. A keyed row prints its key — a tool name, a CLI
// version, an event type, a session id — which is measured corpus data, and
// rewriting that would falsify the measurement.
var labels = map[string]string{
	// scan
	"files":                 "Transcript files",
	"bytes":                 "Bytes on disk",
	"events":                "Transcript events",
	"files_top_level":       "Top-level sessions",
	"files_subagent":        "Subagent transcripts",
	"distinct_event_types":  "Distinct event types",
	"known_event_types":     "Event types tare parses",
	"distinct_cli_versions": "Claude Code versions seen",
	"parse_errors":          "Lines that failed to parse",
	"stats_cache_sessions":  "Sessions in Claude Code's own cache",
	// The gap is cache minus disk, so it names transcripts Claude Code has
	// forgotten. "Retention gap" is the arithmetic; this is the finding.
	"retention_gap": "Sessions no longer on disk",

	// tools
	"tool_use_blocks":             "Tool calls requested",
	"tool_result_blocks":          "Tool results returned",
	"unmatched_results":           "Results with no matching call",
	"unanswered_uses":             "Calls still awaiting a result",
	"distinct_tools":              "Distinct tools",
	"calls":                       "Tool calls",
	"context_bytes":               "Bytes returned into context",
	"image_bytes":                 "Image bytes returned",
	"image_results":               "Results carrying an image",
	"produced_bytes":              "Bytes tools produced",
	"errors":                      "Calls that returned an error",
	"externalised_results":        "Results written to a side file",
	"externalised_produced_bytes": "Bytes produced into side files",
	"externalised_context_bytes":  "Context bytes those results still cost",

	// attribute
	"responses_naive":             "Responses counted before dedup",
	"responses_distinct":          "Distinct billed responses",
	"responses_duplicate":         "Responses written to file twice",
	"duplicate_rate_percent":      "Duplicate share of responses",
	"fresh_tokens":                "Fresh context tokens",
	"rebilled_tokens":             "Prior context re-billed",
	"output_tokens":               "Output tokens",
	"thinking_tokens":             "Thinking tokens",
	"rebill_multiplier":           "Re-billed per fresh token",
	"sessions":                    "Sessions",
	"sessions_with_cost_state":    "Sessions with a cost record",
	"sessions_without_cost_state": "Sessions with no cost record",
	"cost_state_coverage_percent": "Share of sessions carrying a cost record",
	"attachment_events":           "Attachment events",
	"attachment_bytes":            "Attachment bytes",
	"attachment_share_percent":    "Attachment share of the corpus",
	"attachment_types":            "Distinct attachment types",
	"allocated_cost_usd":          "Cost allocated to a dimension",
	"unallocated_cost_usd":        "Cost with no events to allocate across",

	// corruption
	"error_rate_percent":      "Share of calls that errored",
	"empty_results":           "Results that came back empty",
	"empty_rate_percent":      "Share of results that were empty",
	"truncated_results":       "Results carrying a truncation marker",
	"truncation_rate_percent": "Share of results carrying a marker",
	"truncation_marker_kinds": "Distinct truncation markers",

	// boost
	"boost_source":               "Boost figures read from",
	"boost_cli_events_total":     "Boost CLI events, all",
	"boost_cli_events_sampled":   "Boost CLI events, sampled",
	"boost_mcp_calls_total":      "MCP calls Boost filtered, all",
	"boost_mcp_calls_sampled":    "MCP calls Boost filtered, sampled",
	"boost_mcp_saved_tokens":     "Tokens Boost claims saved on MCP",
	"boost_files_saved_tokens":   "Tokens Boost claims saved on files",
	"boost_files_event_count":    "File events Boost filtered",
	"boost_builtin_filters":      "Built-in filters installed",
	"boost_custom_filters":       "Custom filters installed",
	"boost_filter_tokens_before": "Tokens into the built-in filters",
	"boost_filter_tokens_after":  "Tokens out of the built-in filters",
	"boost_filter_saved_tokens":  "Tokens the built-in filters removed",
	"boost_filters_fired":        "Filters that fired at least once",

	// The retention ceiling: Boost's history outlives the transcripts it
	// annotates, so these bound how much of it can be joined at all.
	"boost_transcripts_referenced": "Transcripts Boost has rows for",
	"boost_transcripts_on_disk":    "Boost transcripts still on disk",
	"boost_transcripts_pruned":     "Boost transcripts Claude Code pruned",
	"boost_retention_percent":      "Share of Boost rows still joinable",

	// The Boost per-call join emits one shape from two sources of very
	// different standing, and the name is what carries the provenance: bare
	// for the complete --boost-deep table, `sampled_` for the 100-row JSON
	// array. The labels keep that word, or the sample reads as a total.
	"calls_joined":                    "Calls joined to a transcript",
	"calls_unjoined":                  "Calls whose transcript is gone",
	"join_rate_percent":               "Share of calls joined",
	"response_bytes":                  "Bytes before filtering",
	"filtered_response_bytes":         "Bytes after filtering",
	"removed_bytes":                   "Bytes the filter removed",
	"sampled_calls":                   "Tool calls, sampled",
	"sampled_calls_joined":            "Calls joined to a transcript, sampled",
	"sampled_calls_unjoined":          "Calls whose transcript is gone, sampled",
	"sampled_join_rate_percent":       "Share of calls joined, sampled",
	"sampled_response_bytes":          "Bytes before filtering, sampled",
	"sampled_filtered_response_bytes": "Bytes after filtering, sampled",
	"sampled_removed_bytes":           "Bytes the filter removed, sampled",
	"sampled_saved_tokens":            "Tokens Boost saved, sampled",
}

// label is the human name for a metric, or the metric name itself.
func label(name string) string {
	if l, ok := labels[name]; ok {
		return l
	}
	return name
}

// writeCorpus prints the keyless corpus-wide rows as a three-column block.
func writeCorpus(tw io.Writer, env Envelope) {
	fmt.Fprint(tw, "\nCORPUS\t\t\n")
	uniform := uniformDerivation(env.Metrics, "corpus")
	for _, m := range env.Metrics {
		if m.Dimension == "corpus" {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", label(m.Name), formatValue(m.Value, m.Unit), derivationCell(m, uniform))
		}
	}
	writeDerivationFooter(tw, uniform, "\t\t")
}

// day trims an RFC3339 timestamp to its date. RFC3339 sorts lexically, so no
// time parsing is needed anywhere in this file.
func day(ts string) string {
	if len(ts) < 10 {
		return ts
	}
	return ts[:10]
}

// formatValue renders one metric value by what it measures. The unit decides
// the shape before the type does: a byte count, a percentage, a dollar amount
// and a bare count are four different things, and printing all four through
// %g is what made these tables read as JSON with tab stops.
func formatValue(v any, unit string) string {
	switch unit {
	case "bytes":
		switch n := v.(type) {
		case int:
			return humanizeBytes(int64(n))
		case int64:
			return humanizeBytes(n)
		}
	case "percent":
		if n, ok := v.(float64); ok {
			return strconv.FormatFloat(n, 'f', 1, 64) + "%"
		}
	case "usd":
		if n, ok := v.(float64); ok {
			return formatUSD(n)
		}
	case "ratio":
		if n, ok := v.(float64); ok {
			return formatRatio(n)
		}
	}
	switch n := v.(type) {
	case int:
		return comma(int64(n))
	case int64:
		return comma(n)
	case float64:
		// The tension this comment used to name — keeping a multiplier
		// readable without rounding a sub-cent allocation away to zero — is
		// resolved per unit above. 'g' remains the honest fallback for a float
		// whose unit says nothing about how to shape it.
		return strconv.FormatFloat(n, 'g', 6, 64)
	}
	return fmt.Sprint(v)
}

// humanizeBytes renders a byte count in SI units — divided by 1000, labelled
// MB. tare measures bytes on disk, which is what a disk-size claim reports.
// Dividing by 1024 and calling the result MB is the most common humanization
// bug there is; this function must never mix the two.
func humanizeBytes(n int64) string {
	if n < 0 {
		return "-" + humanizeBytes(-n)
	}
	if n < 1000 {
		return comma(n) + " B"
	}
	// 999.95 rather than 1000: the value is printed rounded to one decimal, so
	// testing the raw number here renders 999,999 B as "1000.0 kB" instead of
	// promoting it to "1.0 MB".
	f, unit := float64(n)/1000, "kB"
	for _, next := range []string{"MB", "GB", "TB"} {
		if f < 999.95 {
			break
		}
		f, unit = f/1000, next
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + " " + unit
}

// formatUSD renders money as money. Below a cent the two-decimal form would
// round a real allocation to $0.00, so those print their exact value instead:
// the sub-cent shares are the point of the per-skill cost table, and this
// codebase treats an absent number as a warning rather than a zero.
func formatUSD(n float64) string {
	if n != 0 && math.Abs(n) < 0.01 {
		return "$" + strconv.FormatFloat(n, 'f', -1, 64)
	}
	return "$" + strconv.FormatFloat(n, 'f', 2, 64)
}

// formatRatio renders a ratio as the multiple it is, always carrying the ×.
// Precision scales instead of the unit: rebill_multiplier spans 0 to 17,070 in
// one column, and a rebill_multiplier below 1 is real — a skill whose responses
// re-read less context than they send. Rounding those to an integer prints 0×
// and deletes them; dropping the × on them instead makes the column change unit
// system between two adjacent rows, which is worse.
func formatRatio(n float64) string {
	switch {
	case n < 1:
		return strconv.FormatFloat(n, 'f', 2, 64) + "×"
	case n < 10:
		return strconv.FormatFloat(n, 'f', 1, 64) + "×"
	default:
		return comma(int64(n+0.5)) + "×"
	}
}

func comma(n int64) string {
	if n < 0 {
		return "-" + comma(-n)
	}
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
