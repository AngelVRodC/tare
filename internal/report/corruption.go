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
	Calls  int64
	Errors int64
	// Denied is the share of Errors the harness blocked before the tool ran;
	// Failures is the rest. Errors stays the whole on purpose: `tools` and
	// `corruption` both emit it, and merge() reports a disagreement between
	// two commands as a defect in this tool — which it would be.
	Denied    int64
	Failures  int64
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
//
// Under a window the join map stays corpus-wide (time-window-6): no window cut
// may sever a join. What the window gates is the per-key aggregation — tools,
// denial kinds and truncation markers aggregate in-window results only — while
// defect detection (the ambiguity count, the unmatched audit) stays
// unconditional.
func CorruptionEnvelope(dir, version string, w Window) (Envelope, error) {
	names := map[string]string{} // tool_use_id → tool name
	tools := map[string]*corruptStat{}
	markers := map[string]int64{}
	denials := map[string]int64{} // toolDenialKind → errored results

	var results, unmatched, ambiguousDenials int64
	b := newBuilder(dir, version, "corruption", w)

	scanStats, err := transcript.Scan(dir, func(ev *transcript.Event) {
		b.seeTime(ev.Timestamp, ev.Type)
		inWin := w.Includes(ev.Timestamp)
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

		// toolDenialKind is one field on the event, while is_error is per
		// block. Measured on 1,045 calls: no event ever carried more than one
		// tool_result, so the two are one-to-one there. Where they are not,
		// the denial is counted against every errored result in the event and
		// the ambiguity is reported rather than assumed away. Defect
		// detection stays unconditional of the window.
		errored := 0
		for _, r := range res {
			if r.IsError {
				errored++
			}
		}
		if ev.ToolDenialKind != "" && errored > 1 {
			ambiguousDenials++
		}

		for _, r := range res {
			results++
			name, ok := names[r.ToolUseID]
			if !ok {
				unmatched++
				continue
			}
			if !inWin {
				continue
			}
			s := bucket(tools, name)
			s.Calls++
			if r.IsError {
				s.Errors++
				if ev.ToolDenialKind != "" {
					s.Denied++
					denials[ev.ToolDenialKind]++
				} else {
					s.Failures++
				}
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
		totals.Denied += s.Denied
		totals.Failures += s.Failures
		totals.Empty += s.Empty
		totals.Truncated += s.Truncated
	}

	add, addWin := b.add, b.addWin
	addWin("calls", totals.Calls, "calls")
	addWin("distinct_tools", int64(len(tools)), "tools")
	addWin("errors", totals.Errors, "calls")
	addWin("error_rate_percent", percent(totals.Errors, totals.Calls), "percent")
	// The split, not a replacement. errors keeps its meaning; these say which
	// half of it is evidence about a tool and which half is about a policy.
	addWin("denied", totals.Denied, "calls")
	addWin("failures", totals.Failures, "calls")
	addWin("failure_rate_percent", percent(totals.Failures, totals.Calls), "percent")
	addWin("empty_results", totals.Empty, "calls")
	addWin("empty_rate_percent", percent(totals.Empty, totals.Calls), "percent")
	addWin("truncated_results", totals.Truncated, "calls")
	addWin("truncation_rate_percent", percent(totals.Truncated, totals.Calls), "percent")
	addWin("truncation_marker_kinds", int64(len(markers)), "markers")
	add("unmatched_results", unmatched, "blocks")

	b.rows(corruptMetrics(tools)...)
	for _, m := range slices.SortedFunc(maps.Keys(markers), func(a, c string) int {
		return cmp.Or(cmp.Compare(markers[c], markers[a]), strings.Compare(a, c))
	}) {
		b.rows(MeasuredMetric("truncated_results", "truncation_marker", m, markers[m], "calls"))
	}

	for _, kind := range slices.SortedFunc(maps.Keys(denials), func(a, c string) int {
		return cmp.Or(cmp.Compare(denials[c], denials[a]), strings.Compare(a, c))
	}) {
		b.rows(MeasuredMetric("denied", "denial_kind", kind, denials[kind], "calls"))
	}

	if totals.Denied > 0 {
		b.warn("%d of %d errored results are policy denials that Claude Code labelled with "+
			"toolDenialKind, not tool failures — a denial cost a turn and returned nothing, "+
			"and is not evidence a tool altered an answer. The failure figures are the %d that remain",
			totals.Denied, totals.Errors, totals.Failures)
	}
	if ambiguousDenials > 0 {
		b.warn("%d events carry a toolDenialKind alongside more than one errored tool_result; "+
			"the field is per event and is_error is per block, so the denial is counted against "+
			"each of them and those rows are an upper bound", ambiguousDenials)
	}
	if totals.Truncated > 0 {
		b.warn("%d results carry a truncation marker; Claude Code externalises rather than truncates, "+
			"so the cut came from the tool. Markers are literal substring matches, "+
			"so a result quoting one is a false positive — check the named tools",
			totals.Truncated)
	}
	return b.done(scanStats), nil
}

// corruptMetrics emits one row group per tool, noisiest first: failures, then
// errors, then truncation, then name. Every tool is carried, including the
// clean ones — the denominator is the finding as much as the numerator.
//
// Failures outrank errors in that order on purpose. Ranking by errors puts a
// tool whose every error was a denial above one that genuinely failed, which
// is the same conflation the split exists to remove.
func corruptMetrics(tools map[string]*corruptStat) []Metric {
	keys := slices.SortedFunc(maps.Keys(tools), func(a, b string) int {
		x, y := tools[a], tools[b]
		return cmp.Or(
			cmp.Compare(y.Failures, x.Failures),
			cmp.Compare(y.Errors, x.Errors),
			cmp.Compare(y.Truncated, x.Truncated),
			cmp.Compare(y.Empty, x.Empty),
			strings.Compare(a, b))
	})
	out := make([]Metric, 0, len(keys)*8)
	for _, k := range keys {
		s := tools[k]
		out = append(out,
			MeasuredMetric("calls", "tool", k, s.Calls, "calls"),
			MeasuredMetric("errors", "tool", k, s.Errors, "calls"),
			MeasuredMetric("error_rate_percent", "tool", k, percent(s.Errors, s.Calls), "percent"),
			MeasuredMetric("denied", "tool", k, s.Denied, "calls"),
			MeasuredMetric("failures", "tool", k, s.Failures, "calls"),
			MeasuredMetric("failure_rate_percent", "tool", k, percent(s.Failures, s.Calls), "percent"),
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
	// Six padding cells: the tool table below is nine columns wide.
	writeCorpus(tw, env, "\t\t\t\t\t\t")

	if rows := groupRows(env.Metrics, "tool"); len(rows) > 0 {
		// ERROR % is kept beside FAIL % rather than replaced by it. It is the
		// figure a reader of an older run will look for, and dropping a column
		// is a change to the printed contract, not a bug fix — so DENIED and
		// FAILED are added and nothing is taken away.
		fmt.Fprint(tw, "\nTOOL\tCALLS\tERRORS\tERROR %\tDENIED\tFAILED\tFAIL %\tEMPTY\tTRUNCATED\tSHARE\t\n")
		calls := corpusValue(env, "calls")
		shown, total := truncate(rows, top)
		for _, r := range shown {
			pct, grade := r.share("calls", calls)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.key,
				r.cell("calls"),
				r.cell("errors"),
				r.cell("error_rate_percent"),
				r.cell("denied"),
				r.cell("failures"),
				r.cell("failure_rate_percent"),
				r.cell("empty_results"),
				r.cell("truncated_results"),
				pct, grade)
		}
		writeTruncation(tw, len(shown), total, "tool")
	}

	if rows := groupRows(env.Metrics, "denial_kind"); len(rows) > 0 {
		fmt.Fprint(tw, "\nDENIAL_KIND\tRESULTS\t\t\t\t\t\t\t\n")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t\t\t\t\t\t\n", r.key, r.cell("denied"))
		}
	}

	if rows := groupRows(env.Metrics, "truncation_marker"); len(rows) > 0 {
		fmt.Fprint(tw, "\nTRUNCATION_MARKER\tRESULTS\t\t\t\t\t\t\t\n")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t\t\t\t\t\t\n", r.key, r.cell("truncated_results"))
		}
	}
	return renderTail(w, tw, env)
}
