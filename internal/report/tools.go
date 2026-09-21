package report

import (
	"cmp"
	"encoding/json"
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
	// Rent (attachment bytes an installed thing costs just by existing) and
	// skill invocations live in their own flat maps: groupRows merges rows
	// sharing Dimension+Key at render time, so a separate metric name is the
	// whole join mechanism — toolStat is deliberately not extended.
	// mcpRentRaw holds the mcp_server rent keys in the harness's own
	// punctuation (`claude.ai Notion`); every consumer canonicalizes them with
	// mcpCanon before they meet the call-side rows.
	mcpRentRaw := map[string]int64{}
	skillRent := map[string]int64{}
	skillCalls := map[string]*int64{}

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
				// Only Skill uses are decoded — one decode per Skill
				// invocation, zero cost for every other tool. Malformed or
				// empty input counts nowhere and stays quiet (unknown fails
				// quietly); a denied Skill call is still an invocation.
				if u.Name == "Skill" {
					var in struct {
						Skill string `json:"skill"`
					}
					if json.Unmarshal(u.Input, &in) == nil && in.Skill != "" {
						*bucket(skillCalls, in.Skill)++
					}
				}
			}
		}
		// Rent accumulates BEFORE the results early-return: attachment
		// events carry no tool_result blocks, so placed after that guard it
		// would silently never accumulate. Window-gated like bucket() so
		// the rent/calls join stays coherent under --since/--until.
		if at := ev.Attachment(); at != nil && inWin {
			switch at.KeyDimension {
			case transcript.DimMcpServer:
				for k, n := range at.KeyBytes {
					mcpRentRaw[k] += n
				}
			case transcript.DimSkill:
				for k, n := range at.KeyBytes {
					skillRent[k] += n
				}
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
	// The authority-unavailable default: the plugin dimension is withheld, but
	// the KEY SPELLING still canonicalizes. mcpCanon is orthogonal to plugin
	// attribution — it only rewrites `:`, `.` and space to the `_` the
	// `mcp__<server>__<tool>` tool name already encodes — so without this copy
	// the one-server-two-rows split survives on any machine whose
	// ~/.claude/settings.json is missing. The authority branch below rebuilds
	// this map with the same canonicalization plus its segment mapping.
	// Integer sums either way: merging keys neither loses nor doubles a byte,
	// and Go's randomised map order cannot move one.
	mcpRent := make(map[string]int64, len(mcpRentRaw))
	for raw, n := range mcpRentRaw {
		mcpRent[mcpCanon(raw)] += n
	}
	pluginRent := map[string]int64{}
	if path, pathErr := pluginSettingsPath(); pathErr != nil {
		b.warn("plugin rollup unavailable: %v — plugin rows omitted, not zeroed", pathErr)
	} else if names, err := pluginNames(path); err != nil {
		b.warn("plugin rollup unavailable: %v — plugin rows omitted, not zeroed", err)
	} else {
		// Rent keys normalize here, where the names authority is already in
		// scope: colon form joins the call-side segment and its bytes also
		// land on the plugin dim under the plugin name (server rent is not
		// re-counted there). rentSegment's authority decision and its `plugin`
		// return are untouched — canonicalization changes the KEY SPELLING only,
		// which is what makes a free-form key (`claude.ai Notion`) meet its
		// call row, and an unmatched colon key (`plugin:github:github`, its
		// plugin installed but not enabled) meet the tool name the harness
		// wrote for it. Never dropped, never zeroed. Skill rent needs no
		// normalization, and pluginRent's keys are plugin NAMES — canonicalizing
		// them would merge distinct plugins.
		mcpRent = make(map[string]int64, len(mcpRentRaw))
		for raw, n := range mcpRentRaw {
			seg, plugin, ok := rentSegment(raw, names)
			mcpRent[mcpCanon(seg)] += n
			if ok {
				pluginRent[plugin] += n
			}
		}
		resolved, unresolvedKeys := pluginRollup(pluginSegments, names)
		pluginResolved = resolved
		if len(unresolvedKeys) > 0 {
			b.warn("plugin: %d unresolved segments: %s", len(unresolvedKeys), strings.Join(unresolvedKeys, ", "))
		}
	}

	b.rows(statMetrics("tool", tools)...)
	b.rows(statMetrics("mcp_server", servers)...)
	b.rows(statMetrics("plugin", pluginResolved)...)
	// Honest-zero stat rows come after the called rows, rent-desc among
	// themselves: called keys never move, and a rent-only entity's zero is
	// a counted fact (the counter streamed the whole corpus).
	b.rows(zeroStatMetrics("mcp_server", rentOnlyKeys(mcpRent, servers))...)
	b.rows(zeroStatMetrics("plugin", rentOnlyKeys(pluginRent, pluginResolved))...)
	b.rows(rentMetrics("mcp_server", mcpRent)...)
	b.rows(rentMetrics("skill", skillRent)...)
	b.rows(rentMetrics("plugin", pluginRent)...)
	// skill_calls covers the union of rent keys and called skill keys, in
	// one sorted order; groupRows preserves first-seen order, so the
	// rendered skill table is rent-desc with no second sort.
	both := map[string]bool{}
	for k := range skillRent {
		both[k] = true
	}
	for k := range skillCalls {
		both[k] = true
	}
	for _, k := range slices.SortedFunc(maps.Keys(both), func(a, b string) int {
		return cmp.Or(cmp.Compare(skillRent[b], skillRent[a]), strings.Compare(a, b))
	}) {
		calls := int64(0)
		if p := skillCalls[k]; p != nil {
			calls = *p
		}
		b.rows(MeasuredMetric("skill_calls", "skill", k, calls, "calls"))
	}
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

// mcpCanon canonicalizes an MCP server name to the spelling the
// `mcp__<server>__<tool>` tool name uses: `:`, `.` and space become `_`. The
// same server reaches tare under two spellings — mcpServer parses the
// underscore spelling out of the tool name, while attributionMcpServer and the
// attachment rent keys keep the harness's own punctuation (`plugin:sre:k8s-qa`,
// `claude.ai Notion`) — so any key joining both channels must pass through
// here first or one server splits its measurement across two rows and neither
// row is the truth. Two joins need it: doctorJoin's observation-vs-config
// cross-join, and ToolsEnvelope's mcp_server rent-vs-calls join (both the
// authority branch and its authority-unavailable fallback).
//
// A canon key with no calls stays a rent-only row with an honest `calls 0` —
// the sanctioned rent-vs-use exception, not a defect to merge away.
//
// mcp_server keys ONLY. Skill and plugin rent/call keys legitimately contain
// `:` (`desplega:feedback`, `ponytail:ponytail-review`) and arrive verbatim
// from the Skill tool input, so canonicalizing them would merge distinct real
// entities and break the rent↔calls join. Never move this inside mcpServer
// either: pluginServer and rentSegment match `plugin_<name>_` prefixes against
// RAW plugin names from the authority, and a plugin name containing `.` or `:`
// would stop matching.
func mcpCanon(name string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ':', '.', ' ':
			return '_'
		}
		return r
	}, name)
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

// rentMetrics emits one rent_bytes row per key, heaviest rent first, so the
// table can group consecutive rows without re-sorting what the envelope
// already ordered. Emitted from a declared sorted slice, never a map range —
// the reproducibility gate (TestReportReproducible) depends on it.
func rentMetrics(dimension string, rent map[string]int64) []Metric {
	keys := slices.SortedFunc(maps.Keys(rent), func(a, b string) int {
		return cmp.Or(
			cmp.Compare(rent[b], rent[a]),
			strings.Compare(a, b))
	})
	out := make([]Metric, 0, len(keys))
	for _, k := range keys {
		out = append(out, MeasuredMetric("rent_bytes", dimension, k, rent[k], "bytes"))
	}
	return out
}

// rentOnlyKeys lists a rent map's keys that carry no call-side stat row, in
// the same rent-desc order the honest-zero rows are appended in.
func rentOnlyKeys(rent map[string]int64, called map[string]*toolStat) []string {
	var out []string
	for k := range rent {
		if _, ok := called[k]; !ok {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		return cmp.Or(
			cmp.Compare(rent[b], rent[a]),
			strings.Compare(a, b))
	})
	return out
}

// zeroStatMetrics emits the five measured-zero rows of a rent-only entity.
// A full stat row, not a lone calls=0, keeps the SHARE column coherent —
// 0.0% against a real zero context instead of a blank cell against a share.
func zeroStatMetrics(dimension string, keys []string) []Metric {
	out := make([]Metric, 0, len(keys)*5)
	for _, k := range keys {
		out = append(out,
			MeasuredMetric("calls", dimension, k, int64(0), "calls"),
			MeasuredMetric("context_bytes", dimension, k, int64(0), "bytes"),
			MeasuredMetric("image_bytes", dimension, k, int64(0), "bytes"),
			MeasuredMetric("produced_bytes", dimension, k, int64(0), "bytes"),
			MeasuredMetric("errors", dimension, k, int64(0), "calls"))
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
		// RENT renders on all three tables uniformly, blank where the
		// envelope carries no rent row — the renderer never sniffs the
		// envelope for whether rent exists (OpenCode's blank column is the
		// same absent-is-not-zero vocabulary as every other blank cell).
		fmt.Fprintf(tw, "\n%s\tCALLS\tCONTEXT\tRENT\tIMAGES\tPRODUCED\tERRORS\tSHARE\t\n", strings.ToUpper(dim))
		for _, r := range rows {
			pct, grade := r.share("context_bytes", context)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.key,
				r.cell("calls"),
				r.cell("context_bytes"),
				r.cell("rent_bytes"),
				r.cell("image_bytes"),
				r.cell("produced_bytes"),
				r.cell("errors"),
				pct, grade)
		}
	}
	// The skill table joins rent with invocations on one row. No truncation:
	// RenderTools never truncates, and --json carries every row.
	if rows := groupRows(env.Metrics, transcript.DimSkill); len(rows) > 0 {
		fmt.Fprintf(tw, "\nSKILL\tRENT\tCALLS\t\n")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t\n", r.key, r.cell("rent_bytes"), r.cell("skill_calls"))
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
