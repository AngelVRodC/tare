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
//
// Under a window the join map and the answered/unmatched bookkeeping stay
// corpus-wide (time-window-6): no window cut may sever a join or inflate
// unmatched_results. What the window gates is the aggregation — the per-tool
// buckets and the block counters count in-window events only.
func ToolsEnvelope(dir, version string, w Window) (Envelope, error) {
	names := map[string]string{} // tool_use_id → tool name
	tools := map[string]*toolStat{}
	servers := map[string]*toolStat{}
	pluginSegments := map[string]*toolStat{}
	answered := map[string]bool{}
	usesInWindow := map[string]bool{}

	var useBlocks, resultBlocks, unmatched int64
	var extResults, extProduced, extContext int64
	var imageBlocks int64
	b := newBuilder(dir, version, "tools", w)

	scanStats, err := transcript.Scan(dir, func(ev *transcript.Event) {
		b.seeTime(ev.Timestamp, ev.Type)
		uses, results := ev.Blocks()
		inWin := w.Includes(ev.Timestamp)
		for _, u := range uses {
			// The join map is corpus-wide: a use inside the window whose
			// result lies outside must still resolve.
			names[u.ID] = u.Name
			if inWin {
				usesInWindow[u.ID] = true
				useBlocks++
			}
		}
		if len(results) == 0 {
			return
		}

		// The externalisation record sits on the event, not the block. Every
		// one of the 39 measured records is on an event with exactly one
		// result, so a second result would make the attribution ambiguous —
		// say so rather than double-count it. Defect detection stays
		// unconditional: a window restricts what is measured, not what is
		// reported broken.
		persisted := ev.Persisted()
		if persisted != nil && len(results) != 1 {
			b.warn("%s: persisted output %s on an event with %d tool_result blocks — produced bytes not attributed",
				ev.File, persisted.Path, len(results))
			persisted = nil
		}

		for _, r := range results {
			name, ok := names[r.ToolUseID]
			if !ok {
				unmatched++
				b.warn("%s: tool_result %s has no matching tool_use", ev.File, r.ToolUseID)
				continue
			}
			answered[r.ToolUseID] = true

			// The measured baseline is 39 of 39 byte-exact, so a size that
			// disagrees is a real defect. A side file that is simply gone
			// is only a warning: a transcript outlives its side file. The
			// checks run before the window gate — a defect is a finding
			// about the corpus, not about the query.
			if persisted != nil {
				if fi, err := os.Stat(persisted.Path); err != nil {
					b.warn("persisted output missing: %s (%v)", persisted.Path, err)
				} else if fi.Size() != persisted.Size {
					b.warn("persisted output size mismatch: %s reports %d bytes, on disk %d",
						persisted.Path, persisted.Size, fi.Size())
				}
			}
			if !inWin {
				continue
			}
			resultBlocks++

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
			}

			bucket(tools, name).add(r.ContextBytes, r.ImageBytes, produced, r.IsError)
			if server := mcpServer(name); server != "" {
				bucket(servers, server).add(r.ContextBytes, r.ImageBytes, produced, r.IsError)
				// Plugin-provided servers arrive as segment
				// plugin_<plugin>_<server>. The segments are parked here
				// unresolved and named after the scan — same measured bytes,
				// zero new parsing.
				if strings.HasPrefix(server, "plugin_") {
					bucket(pluginSegments, server).add(r.ContextBytes, r.ImageBytes, produced, r.IsError)
				}
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
	// in the same sense as an unmatched result — count it apart. Under a
	// window, only in-window uses are counted, and an answer counts wherever
	// in the corpus it sits.
	var unanswered int64
	for id := range usesInWindow {
		if !answered[id] {
			unanswered++
		}
	}

	add, addWin := b.add, b.addWin
	addWin("tool_use_blocks", useBlocks, "blocks")
	addWin("tool_result_blocks", resultBlocks, "blocks")
	add("unmatched_results", unmatched, "blocks")
	addWin("unanswered_uses", unanswered, "blocks")
	addWin("distinct_tools", len(tools), "tools")
	addWin("calls", totals.Calls, "calls")
	addWin("context_bytes", totals.ContextBytes, "bytes")
	addWin("image_bytes", totals.ImageBytes, "bytes")
	addWin("image_results", imageBlocks, "results")
	addWin("produced_bytes", totals.ProducedBytes, "bytes")
	addWin("errors", totals.Errors, "calls")
	addWin("externalised_results", extResults, "results")
	addWin("externalised_produced_bytes", extProduced, "bytes")
	addWin("externalised_context_bytes", extContext, "bytes")

	// Resolution happens after the scan: the authority (user-scope config,
	// independent of --dir) provides key mapping only, never a value. An
	// unreadable authority omits the whole dimension with one warning —
	// absent is not zero.
	pluginResolved := map[string]*toolStat{}
	if path, pathErr := pluginSettingsPath(); pathErr != nil {
		b.warn("plugin rollup unavailable: %v — plugin rows omitted, not zeroed", pathErr)
	} else if names, err := pluginNames(path); err != nil {
		b.warn("plugin rollup unavailable: %v — plugin rows omitted, not zeroed", err)
	} else {
		resolved, unresolvedKeys := pluginRollup(pluginSegments, names)
		pluginResolved = resolved
		if len(unresolvedKeys) > 0 {
			b.warn("plugin: %d unresolved segments: %s", len(unresolvedKeys), strings.Join(unresolvedKeys, ", "))
		}
	}

	b.rows(statMetrics("tool", tools)...)
	b.rows(statMetrics("mcp_server", servers)...)
	b.rows(statMetrics("plugin", pluginResolved)...)
	return b.done(scanStats), nil
}

// mcpServer names the server in an `mcp__<server>__<tool>` tool name, or ""
// when the name is not one. Only the first two separators are structural, so a
// tool name carrying further `__` stays whole — and the tool half is not
// returned at all, because the only rollup keyed off this is per server.
func mcpServer(name string) string {
	rest, isMCP := strings.CutPrefix(name, "mcp__")
	if !isMCP {
		return ""
	}
	server, _, found := strings.Cut(rest, "__")
	if !found {
		return ""
	}
	return server
}

// statMetrics emits up to five rows per key, heaviest context first, so the
// table can group consecutive rows without re-sorting what the envelope
// already ordered.
//
// "Up to", because a name in omit is left out of the envelope entirely rather
// than emitted as a zero. A harness that does not record image bytes has not
// measured zero of them, and `measured` is the one derivation Validate accepts
// unconditionally — so a zero row here would ship an over-claim that nothing
// downstream can catch. RenderTools renders the absent metric as a blank cell.
// omit is variadic so the two Claude Code call sites, which omit nothing, read
// exactly as they did before.
func statMetrics(dimension string, stats map[string]*toolStat, omit ...string) []Metric {
	keys := slices.SortedFunc(maps.Keys(stats), func(a, b string) int {
		return cmp.Or(
			cmp.Compare(stats[b].ContextBytes, stats[a].ContextBytes),
			strings.Compare(a, b))
	})
	out := make([]Metric, 0, len(keys)*5)
	for _, k := range keys {
		s := stats[k]
		for _, m := range [...]Metric{
			MeasuredMetric("calls", dimension, k, s.Calls, "calls"),
			MeasuredMetric("context_bytes", dimension, k, s.ContextBytes, "bytes"),
			MeasuredMetric("image_bytes", dimension, k, s.ImageBytes, "bytes"),
			MeasuredMetric("produced_bytes", dimension, k, s.ProducedBytes, "bytes"),
			MeasuredMetric("errors", dimension, k, s.Errors, "calls"),
		} {
			if !slices.Contains(omit, m.Name) {
				out = append(out, m)
			}
		}
	}
	return out
}

// RenderTools prints the envelope as a table. Like RenderScan it reads only
// the envelope, so the table and `--json` can never report different numbers.
func RenderTools(w io.Writer, env Envelope) error {
	renderHeader(w, env)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	writeCorpus(tw, env, "")
	// mcp_server rows are a subset of tool rows, so their shares are of all
	// context rather than of each other, and do not sum to 100. The plugin
	// rows are a subset too — they re-key mcp_server segments, never add
	// bytes — so the same caveat covers them.
	context := corpusValue(env, "context_bytes")
	for _, dim := range []string{"tool", "mcp_server", "plugin"} {
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
