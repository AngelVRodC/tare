package report

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"os"
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
	b := newBuilder(dir, version, "tools")

	scanStats, err := transcript.Scan(dir, func(ev *transcript.Event) {
		b.see(ev)
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
			b.warn("%s: persisted output %s on an event with %d tool_result blocks — produced bytes not attributed",
				ev.File, persisted.Path, len(results))
			persisted = nil
		}

		for _, r := range results {
			resultBlocks++
			name, ok := names[r.ToolUseID]
			if !ok {
				unmatched++
				b.warn("%s: tool_result %s has no matching tool_use", ev.File, r.ToolUseID)
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
				// The measured baseline is 39 of 39 byte-exact, so a size that
				// disagrees is a real defect. A side file that is simply gone
				// is only a warning: a transcript outlives its side file.
				if fi, err := os.Stat(persisted.Path); err != nil {
					b.warn("persisted output missing: %s (%v)", persisted.Path, err)
				} else if fi.Size() != persisted.Size {
					b.warn("persisted output size mismatch: %s reports %d bytes, on disk %d",
						persisted.Path, persisted.Size, fi.Size())
				}
			}

			bucket(tools, name).add(r.ContextBytes, r.ImageBytes, produced, r.IsError)
			if server, _, isMCP := splitMCP(name); isMCP {
				bucket(servers, server).add(r.ContextBytes, r.ImageBytes, produced, r.IsError)
			}
		}
	})
	if err != nil {
		return Envelope{}, err
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

	add := b.add
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

	b.rows(statMetrics("tool", tools)...)
	b.rows(statMetrics("mcp_server", servers)...)
	return b.done(scanStats), nil
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
	renderHeader(w, env)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	writeCorpus(tw, env)
	// mcp_server rows are a subset of tool rows, so their shares are of all
	// context rather than of each other, and do not sum to 100.
	context := corpusValue(env, "context_bytes")
	for _, dim := range []string{"tool", "mcp_server"} {
		rows := groupRows(env.Metrics, dim)
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(tw, "\n%s\tCALLS\tCONTEXT\tIMAGES\tPRODUCED\tERRORS\tSHARE\t\n", strings.ToUpper(dim))
		for _, r := range rows {
			pct, grade := r.share("context_bytes", context)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.key,
				r.cell("calls"),
				r.cell("context_bytes"),
				r.cell("image_bytes"),
				r.cell("produced_bytes"),
				r.cell("errors"),
				pct, grade)
		}
	}
	return renderTail(w, tw, env)
}

type row struct {
	key    string
	values map[string]Metric
}

// cell formats one of a row's metrics by what that metric measures. A name the
// row does not carry yields the zero Metric, whose nil Value formats exactly as
// a missing map key did before.
func (r row) cell(name string) string {
	m := r.values[name]
	return formatValue(m.Value, m.Unit)
}

// num reads one of a row's metrics as an integer count.
func (r row) num(name string) int64 {
	return asInt64(r.values[name].Value)
}

// share renders one row's slice of a corpus-wide denominator, plus the concern
// grade for it. A zero denominator prints neither: an absent number is a
// warning in this codebase, never a 0.0%.
func (r row) share(name string, whole int64) (pct, grade string) {
	if whole == 0 {
		return "", ""
	}
	s := percent(r.num(name), whole)
	return formatValue(s, "percent"), concern(s)
}

// corpusValue reads one keyless corpus-wide metric off the envelope. Every
// share denominator is already measured as a single int64, so nothing here is
// re-accumulated — summing float64 over a Go map range is what made an earlier
// --json output non-reproducible, and a share is a division, not a sum.
func corpusValue(env Envelope, name string) int64 {
	for _, m := range env.Metrics {
		if m.Dimension == "corpus" && m.Name == name {
			return asInt64(m.Value)
		}
	}
	return 0
}

// asInt64 widens the two integer kinds the envelope carries. A value that is
// neither counts as nothing, which is what a missing metric already meant.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

// concern grades a share the way git-sizer grades a repository metric: the
// interpretation lives in the table, so there is no separate prose summary to
// disagree with it. It collapses at the top rather than running to 30 marks.
func concern(share float64) string {
	switch {
	case share >= 50:
		return "!!"
	case share >= 35:
		return "***"
	case share >= 20:
		return "**"
	case share >= 5:
		return "*"
	}
	return ""
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
			rows = append(rows, row{key: m.Key, values: map[string]Metric{}})
		}
		rows[i].values[m.Name] = m
	}
	return rows
}
