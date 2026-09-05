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

// toolStat is one tool's rollup. Every field is a straight count.
type toolStat struct {
	Calls         int64
	ContextBytes  int64
	ImageBytes    int64
	ProducedBytes int64
	Errors        int64
}

func (s *toolStat) add(ctx, image, produced int64, isError bool) {
	s.Calls++
	s.ContextBytes += ctx
	s.ImageBytes += image
	s.ProducedBytes += produced
	if isError {
		s.Errors++
	}
}

// ToolsEnvelope joins `tool_use` → `tool_result` by tool_use_id across the
// corpus and rolls the results up per tool name and per MCP server.
//
// The join is a single streaming pass: a result always follows its use in the
// same file, so only an id → name map is retained, never a line of content.
// An unmatched result is reported, never dropped — the measured baseline is
// zero, so any non-zero count is a finding.
func ToolsEnvelope(dir, version string) (Envelope, error) {
	names := map[string]string{} // tool_use_id → tool name
	tools := map[string]*toolStat{}
	servers := map[string]*toolStat{}
	answered := map[string]bool{}

	var useBlocks, resultBlocks, unmatched int64
	var extResults, extProduced, extContext int64
	var imageBlocks int64
	var warnings []string
	var from, to string

	stat := func(m map[string]*toolStat, key string) *toolStat {
		s := m[key]
		if s == nil {
			s = &toolStat{}
			m[key] = s
		}
		return s
	}

	scanStats, err := transcript.Scan(dir, func(ev *transcript.Event) {
		if ts := ev.Timestamp; ts != "" {
			if from == "" || ts < from {
				from = ts
			}
			if to == "" || ts > to {
				to = ts
			}
		}

		uses, results := ev.Blocks()
		for _, u := range uses {
			useBlocks++
			names[u.ID] = u.Name
		}
		if len(results) == 0 {
			return
		}

		// The externalisation record sits on the event, not the block. Every
		// one of the 39 measured records is on an event with exactly one
		// result, so a second result would make the attribution ambiguous —
		// say so rather than double-count it.
		persisted := ev.Persisted()
		if persisted != nil && len(results) != 1 {
			warnings = append(warnings, fmt.Sprintf(
				"%s: persisted output %s on an event with %d tool_result blocks — produced bytes not attributed",
				ev.File, persisted.Path, len(results)))
			persisted = nil
		}

		for _, r := range results {
			resultBlocks++
			name, ok := names[r.ToolUseID]
			if !ok {
				unmatched++
				warnings = append(warnings, fmt.Sprintf(
					"%s: tool_result %s has no matching tool_use", ev.File, r.ToolUseID))
				continue
			}
			answered[r.ToolUseID] = true

			// Inline: what was produced is what arrived — text and image
			// alike. Externalised results overwrite this below.
			produced := r.ContextBytes + r.ImageBytes
			if r.ImageBytes > 0 {
				imageBlocks++
			}
			if persisted != nil {
				produced = persisted.Size
				extResults++
				extProduced += persisted.Size
				extContext += r.ContextBytes
				if onDisk, err := persisted.OnDisk(); err != nil {
					// A transcript outlives its side file: warn, do not fail.
					warnings = append(warnings, fmt.Sprintf(
						"persisted output missing: %s (%v)", persisted.Path, err))
				} else if onDisk != persisted.Size {
					// Baseline is 39 of 39 exact — disagreement is a defect.
					warnings = append(warnings, fmt.Sprintf(
						"persisted output size mismatch: %s reports %d bytes, on disk %d",
						persisted.Path, persisted.Size, onDisk))
				}
			}

			stat(tools, name).add(r.ContextBytes, r.ImageBytes, produced, r.IsError)
			if server, _, isMCP := splitMCP(name); isMCP {
				stat(servers, server).add(r.ContextBytes, r.ImageBytes, produced, r.IsError)
			}
		}
	})
	if err != nil {
		return Envelope{}, err
	}

	env := Envelope{
		Tool:    "tare",
		Version: version,
		Command: "tools",
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

	var totals toolStat
	for _, s := range tools {
		totals.Calls += s.Calls
		totals.ContextBytes += s.ContextBytes
		totals.ImageBytes += s.ImageBytes
		totals.ProducedBytes += s.ProducedBytes
		totals.Errors += s.Errors
	}
	// A use with no result is the live tail of a running session, not a defect
	// in the same sense as an unmatched result — count it apart.
	unanswered := int64(len(names) - len(answered))

	add := func(name string, value any, unit string) {
		env.Metrics = append(env.Metrics, MeasuredMetric(name, "corpus", "", value, unit))
	}
	add("tool_use_blocks", useBlocks, "blocks")
	add("tool_result_blocks", resultBlocks, "blocks")
	add("unmatched_results", unmatched, "blocks")
	add("unanswered_uses", unanswered, "blocks")
	add("distinct_tools", len(tools), "tools")
	add("calls", totals.Calls, "calls")
	add("context_bytes", totals.ContextBytes, "bytes")
	add("image_bytes", totals.ImageBytes, "bytes")
	add("image_results", imageBlocks, "results")
	add("produced_bytes", totals.ProducedBytes, "bytes")
	add("errors", totals.Errors, "calls")
	add("externalised_results", extResults, "results")
	add("externalised_produced_bytes", extProduced, "bytes")
	add("externalised_context_bytes", extContext, "bytes")

	env.Metrics = append(env.Metrics, statMetrics("tool", tools)...)
	env.Metrics = append(env.Metrics, statMetrics("mcp_server", servers)...)

	env.Warnings = append(env.Warnings, warnings...)
	if scanStats.ParseErrors > 0 {
		env.Warnings = append(env.Warnings,
			fmt.Sprintf("%d lines failed to decode", scanStats.ParseErrors))
	}
	return env, nil
}

// splitMCP splits an `mcp__<server>__<tool>` name into its server and tool.
// A tool name containing further `__` stays intact — only the first two
// separators are structural.
func splitMCP(name string) (server, tool string, ok bool) {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return "", "", false
	}
	server, tool, found := strings.Cut(name[len(prefix):], "__")
	if !found || server == "" {
		return "", "", false
	}
	return server, tool, true
}

// statMetrics emits five rows per key, heaviest context first, so the table
// can group consecutive rows without re-sorting what the envelope already
// ordered.
func statMetrics(dimension string, stats map[string]*toolStat) []Metric {
	keys := slices.SortedFunc(maps.Keys(stats), func(a, b string) int {
		return cmp.Or(
			cmp.Compare(stats[b].ContextBytes, stats[a].ContextBytes),
			strings.Compare(a, b))
	})
	out := make([]Metric, 0, len(keys)*5)
	for _, k := range keys {
		s := stats[k]
		out = append(out,
			MeasuredMetric("calls", dimension, k, s.Calls, "calls"),
			MeasuredMetric("context_bytes", dimension, k, s.ContextBytes, "bytes"),
			MeasuredMetric("image_bytes", dimension, k, s.ImageBytes, "bytes"),
			MeasuredMetric("produced_bytes", dimension, k, s.ProducedBytes, "bytes"),
			MeasuredMetric("errors", dimension, k, s.Errors, "calls"),
		)
	}
	return out
}

// RenderTools prints the envelope as a table. Like RenderScan it reads only
// the envelope, so the table and `--json` can never report different numbers.
func RenderTools(w io.Writer, env Envelope) error {
	fmt.Fprintf(w, "tare %s — %s\n", env.Command, env.Corpus.Dir)
	fmt.Fprintf(w, "%s .. %s\n", env.Corpus.From, env.Corpus.To)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "\nCORPUS\t\t\n")
	for _, m := range env.Metrics {
		if m.Dimension == "corpus" {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", m.Name, formatValue(m.Value), m.Derivation)
		}
	}
	for _, dim := range []string{"tool", "mcp_server"} {
		rows := groupRows(env.Metrics, dim)
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(tw, "\n%s\tCALLS\tCONTEXT\tIMAGES\tPRODUCED\tERRORS\n", strings.ToUpper(dim))
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.key,
				formatValue(r.values["calls"]),
				formatValue(r.values["context_bytes"]),
				formatValue(r.values["image_bytes"]),
				formatValue(r.values["produced_bytes"]),
				formatValue(r.values["errors"]))
		}
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

type row struct {
	key    string
	values map[string]any
}

// groupRows folds the flat metric rows of one dimension back into table rows,
// preserving the order the envelope emitted them in.
func groupRows(metrics []Metric, dimension string) []row {
	var rows []row
	index := map[string]int{}
	for _, m := range metrics {
		if m.Dimension != dimension {
			continue
		}
		i, ok := index[m.Key]
		if !ok {
			i = len(rows)
			index[m.Key] = i
			rows = append(rows, row{key: m.Key, values: map[string]any{}})
		}
		rows[i].values[m.Name] = m.Value
	}
	return rows
}
