package report

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/AngelVRodC/tare/internal/transcript"
)

// corruptStat is one tool's evidence that its output was altered before the
// agent reasoned over it. Every field is a straight count.
type corruptStat struct {
	Calls     int64
	Errors    int64
	Empty     int64
	Truncated int64
}

// CorruptionEnvelope reports the evidence that tooling changed an answer on
// the way to the model: per-tool error rate, results that arrived empty
// without being externalised, and truncation markers a tool wrote itself.
//
// One streaming pass, same as every other command. Only the tool_use id → name
// map is retained, because a tool_result names an id and never the tool that
// produced it, so every rate above is a join away from being unattributable.
func CorruptionEnvelope(dir, version string) (Envelope, error) {
	names := map[string]string{} // tool_use_id → tool name
	tools := map[string]*corruptStat{}
	markers := map[string]int64{}

	var results, unmatched int64
	b := newBuilder(dir, version, "corruption")

	scanStats, err := transcript.Scan(dir, func(ev *transcript.Event) {
		b.seeTime(ev.Timestamp)
		uses, res := ev.Blocks()
		for _, u := range uses {
			names[u.ID] = u.Name
		}
		if len(res) == 0 {
			return
		}
		// An externalised result is empty in context on purpose — the bytes
		// went to a side file losslessly. It is the opposite of corruption, so
		// it must never be counted as an empty answer.
		externalised := ev.Persisted() != nil

		for _, r := range res {
			results++
			name, ok := names[r.ToolUseID]
			if !ok {
				unmatched++
				continue
			}
			s := bucket(tools, name)
			s.Calls++
			if r.IsError {
				s.Errors++
			}
			if r.ContextBytes == 0 && r.ImageBytes == 0 && !externalised {
				s.Empty++
			}
			if r.TruncationMarker != "" {
				s.Truncated++
				markers[r.TruncationMarker]++
			}
		}
	})
	if err != nil {
		return Envelope{}, err
	}

	var totals corruptStat
	for _, s := range tools {
		totals.Calls += s.Calls
		totals.Errors += s.Errors
		totals.Empty += s.Empty
		totals.Truncated += s.Truncated
	}

	add := b.add
	add("calls", totals.Calls, "calls")
	add("distinct_tools", int64(len(tools)), "tools")
	add("errors", totals.Errors, "calls")
	add("error_rate_percent", percent(totals.Errors, totals.Calls), "percent")
	add("empty_results", totals.Empty, "calls")
	add("empty_rate_percent", percent(totals.Empty, totals.Calls), "percent")
	add("truncated_results", totals.Truncated, "calls")
	add("truncation_rate_percent", percent(totals.Truncated, totals.Calls), "percent")
	add("truncation_marker_kinds", int64(len(markers)), "markers")
	add("unmatched_results", unmatched, "blocks")

	b.rows(corruptMetrics(tools)...)
	for _, m := range slices.SortedFunc(maps.Keys(markers), func(a, c string) int {
		return cmp.Or(cmp.Compare(markers[c], markers[a]), strings.Compare(a, c))
	}) {
		b.rows(MeasuredMetric("truncated_results", "truncation_marker", m, markers[m], "calls"))
	}

	if totals.Truncated > 0 {
		b.warn("%d results carry a truncation marker; Claude Code externalises rather than truncates, "+
			"so the cut came from the tool. Markers are literal substring matches, "+
			"so a result quoting one is a false positive — check the named tools",
			totals.Truncated)
	}
	return b.done(scanStats), nil
}

// corruptMetrics emits one row group per tool, noisiest first: errors, then
// truncation, then name. Every tool is carried, including the clean ones — the
// denominator is the finding as much as the numerator.
func corruptMetrics(tools map[string]*corruptStat) []Metric {
	keys := slices.SortedFunc(maps.Keys(tools), func(a, b string) int {
		x, y := tools[a], tools[b]
		return cmp.Or(
			cmp.Compare(y.Errors, x.Errors),
			cmp.Compare(y.Truncated, x.Truncated),
			cmp.Compare(y.Empty, x.Empty),
			strings.Compare(a, b))
	})
	out := make([]Metric, 0, len(keys)*5)
	for _, k := range keys {
		s := tools[k]
		out = append(out,
			MeasuredMetric("calls", "tool", k, s.Calls, "calls"),
			MeasuredMetric("errors", "tool", k, s.Errors, "calls"),
			MeasuredMetric("error_rate_percent", "tool", k, percent(s.Errors, s.Calls), "percent"),
			MeasuredMetric("empty_results", "tool", k, s.Empty, "calls"),
			MeasuredMetric("truncated_results", "tool", k, s.Truncated, "calls"))
	}
	return out
}

// RenderCorruption prints the envelope as a table. Like every other renderer
// it reads only the envelope, so the table and `--json` cannot disagree.
//
// top caps the two ranked tables the way it does in RenderAttribute; zero or
// less prints every row.
func RenderCorruption(w io.Writer, env Envelope, top int) error {
	renderHeader(w, env)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// Three padding cells: the tool table below is six columns wide.
	writeCorpus(tw, env, "\t\t\t")

	if rows := groupRows(env.Metrics, "tool"); len(rows) > 0 {
		fmt.Fprint(tw, "\nTOOL\tCALLS\tERRORS\tERROR %\tEMPTY\tTRUNCATED\tSHARE\t\n")
		calls := corpusValue(env, "calls")
		shown, total := truncate(rows, top)
		for _, r := range shown {
			pct, grade := r.share("calls", calls)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.key,
				r.cell("calls"),
				r.cell("errors"),
				r.cell("error_rate_percent"),
				r.cell("empty_results"),
				r.cell("truncated_results"),
				pct, grade)
		}
		writeTruncation(tw, len(shown), total, "tool")
	}

	if rows := groupRows(env.Metrics, "truncation_marker"); len(rows) > 0 {
		fmt.Fprint(tw, "\nTRUNCATION_MARKER\tRESULTS\t\t\t\t\n")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t\t\t\t\n", r.key, r.cell("truncated_results"))
		}
	}
	return renderTail(w, tw, env)
}
