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
		b.seeTime(ev.Timestamp)
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
	// Emitted only when there is something to report: a row of zero on every
	// clean corpus would say nothing, and the warning is what carries the
	// finding when there is one.
	if stats.NonTranscriptRecords > 0 {
		add("files_non_transcript", stats.NonTranscriptFiles, "files")
		add("non_transcript_records", stats.NonTranscriptRecords, "records")
		b.warn("%d records under the root carry an agentId and a resume key and no sessionId — "+
			"Workflow-tool journal entries, not transcript events. They are excluded from every "+
			"count above except bytes on disk. Files holding nothing else: %d, excluded from the "+
			"file counts too",
			stats.NonTranscriptRecords, stats.NonTranscriptFiles)
	}

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
// `unmatched_results` are all shared by `tools` and `corruption`. A per-command
// map would let the wording drift apart for no reader benefit.
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
	// A journal is not a conversation, so it is named for what it is not
	// rather than counted as a transcript it never was.
	"files_non_transcript":   "Non-transcript files",
	"non_transcript_records": "Non-transcript records",
	"stats_cache_sessions":   "Sessions in Claude Code's own cache",
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
}

// label is the human name for a metric, or the metric name itself.
func label(name string) string {
	if l, ok := labels[name]; ok {
		return l
	}
	return name
}

// writeCorpus prints the keyless corpus-wide rows as a three-column block.
// pad carries the extra empty cells a wider table below it needs; a table of
// exactly three columns passes "".
func writeCorpus(tw io.Writer, env Envelope, pad string) {
	fmt.Fprintf(tw, "\nCORPUS\t\t%s\n", pad)
	uniform := uniformDerivation(env.Metrics, "corpus")
	for _, m := range env.Metrics {
		if m.Dimension == "corpus" {
			fmt.Fprintf(tw, "%s\t%s\t%s%s\n", label(m.Name), formatValue(m.Value, m.Unit), derivationCell(m, uniform), pad)
		}
	}
	writeDerivationFooter(tw, uniform, "\t\t"+pad)
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
	// An omitted metric reaches here as a nil value, because row.cell reads a
	// name the row does not carry and gets the zero Metric back. The fallback
	// below would print the literal "<nil>" for it. This codebase's rule is
	// that an absent number is a warning, never a 0 — and "<nil>" in a column
	// is worse than either, so a blank cell is what an omitted metric renders as.
	if v == nil {
		return ""
	}
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
