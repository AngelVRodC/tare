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
// It then adds the Boost counterfactual — what a filtering layer removed —
// and states plainly which path that came from, because "Boost is not
// installed" and "Boost removed nothing" are different findings.
//
// One streaming pass, same as every other command. Only the tool_use id → name
// map is retained, because the Boost per-call join needs it.
func CorruptionEnvelope(dir, version string, deep bool) (Envelope, error) {
	names := map[string]string{} // tool_use_id → tool name
	tools := map[string]*corruptStat{}
	markers := map[string]int64{}

	var results, unmatched int64
	var from, to string

	scanStats, err := transcript.Scan(dir, func(ev *transcript.Event) {
		if ts := ev.Timestamp; ts != "" {
			if from == "" || ts < from {
				from = ts
			}
			if to == "" || ts > to {
				to = ts
			}
		}

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

	env := Envelope{
		Tool:    "tare",
		Version: version,
		Command: "corruption",
		Corpus: Corpus{
			Dir:   dir,
			Files: scanStats.Files,
			Bytes: scanStats.Bytes,
			From:  day(from),
			To:    day(to),
		},
		Metrics:  []Metric{},
		Warnings: []string{},
	}

	var totals corruptStat
	for _, s := range tools {
		totals.Calls += s.Calls
		totals.Errors += s.Errors
		totals.Empty += s.Empty
		totals.Truncated += s.Truncated
	}

	add := func(name string, value any, unit string) {
		env.Metrics = append(env.Metrics, MeasuredMetric(name, "corpus", "", value, unit))
	}
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

	env.Metrics = append(env.Metrics, corruptMetrics(tools)...)
	for _, m := range slices.SortedFunc(maps.Keys(markers), func(a, b string) int {
		return cmp.Or(cmp.Compare(markers[b], markers[a]), strings.Compare(a, b))
	}) {
		env.Metrics = append(env.Metrics,
			MeasuredMetric("truncated_results", "truncation_marker", m, markers[m], "calls"))
	}

	boostRows, boostWarnings := boostSection(names, deep)
	env.Metrics = append(env.Metrics, boostRows...)

	if totals.Truncated > 0 {
		env.Warnings = append(env.Warnings, fmt.Sprintf(
			"%d results carry a truncation marker; Claude Code externalises rather than truncates, "+
				"so the cut came from the tool. Markers are literal substring matches, "+
				"so a result quoting one is a false positive — check the named tools",
			totals.Truncated))
	}
	env.Warnings = append(env.Warnings, boostWarnings...)
	if scanStats.ParseErrors > 0 {
		env.Warnings = append(env.Warnings,
			fmt.Sprintf("%d lines failed to decode", scanStats.ParseErrors))
	}
	return env, nil
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

// writeScalars prints one keyless dimension as name/value/derivation rows.
// A dimension with no rows prints no header: an absent section is how "this
// source was unavailable" reads, and it is never a table of zeros.
func writeScalars(tw io.Writer, env Envelope, dimension string) {
	var wrote bool
	for _, m := range env.Metrics {
		if m.Dimension != dimension || m.Key != "" {
			continue
		}
		if !wrote {
			fmt.Fprintf(tw, "\n%s\t\t\t\t\t\n", strings.ToUpper(dimension))
			wrote = true
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t\t\t\n", m.Name, formatValue(m.Value), m.Derivation)
	}
}

// RenderCorruption prints the envelope as a table. Like every other renderer
// it reads only the envelope, so the table and `--json` cannot disagree.
func RenderCorruption(w io.Writer, env Envelope) error {
	fmt.Fprintf(w, "tare %s — %s\n", env.Command, env.Corpus.Dir)
	fmt.Fprintf(w, "%s .. %s\n", env.Corpus.From, env.Corpus.To)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	writeScalars(tw, env, "corpus")
	writeScalars(tw, env, "boost")
	writeScalars(tw, env, "boost_mcp_sample")
	writeScalars(tw, env, "boost_mcp_deep")

	if rows := groupRows(env.Metrics, "tool"); len(rows) > 0 {
		fmt.Fprint(tw, "\nTOOL\tCALLS\tERRORS\tERROR %\tEMPTY\tTRUNCATED\n")
		shown, withheld := truncate(rows)
		for _, r := range shown {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.key,
				formatValue(r.values["calls"]),
				formatValue(r.values["errors"]),
				formatValue(r.values["error_rate_percent"]),
				formatValue(r.values["empty_results"]),
				formatValue(r.values["truncated_results"]))
		}
		writeWithheld(tw, withheld, "tool")
	}

	if rows := groupRows(env.Metrics, "truncation_marker"); len(rows) > 0 {
		fmt.Fprint(tw, "\nTRUNCATION_MARKER\tRESULTS\t\t\t\t\n")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t\t\t\t\n", r.key, formatValue(r.values["truncated_results"]))
		}
	}

	// Only the filters that actually fired are worth a line; the envelope
	// carries all of them, and the withheld count says how many are silent.
	if rows := groupRows(env.Metrics, "boost_filter"); len(rows) > 0 {
		fmt.Fprint(tw, "\nBOOST_FILTER\tEVENTS\tTOKENS BEFORE\tTOKENS AFTER\tSAVED\tSAVED %\tRETRIEVES\n")
		shown, withheld := truncate(rows)
		for _, r := range shown {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.key,
				formatValue(r.values["event_count"]),
				formatValue(r.values["tokens_before"]),
				formatValue(r.values["tokens_after"]),
				formatValue(r.values["saved_tokens"]),
				formatValue(r.values["saved_rate_percent"]),
				formatValue(r.values["retrieve_count"]))
		}
		writeWithheld(tw, withheld, "boost_filter")
	}
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
