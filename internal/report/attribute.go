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

// unattributedKey labels responses no attribution field claimed. It is a row
// of its own and is never redistributed silently across the named ones.
const unattributedKey = "(unattributed)"

// sessionDim rolls the same re-billing and allocation up per session. It is a
// dimension like the five attribution ones so that a session's dollars come
// out of the one allocation method too, never a second path.
const sessionDim = "session"

// attributionDims are the five un-redacted fields the product exists to read.
// Anthropic's own OTel export reduces the plugin and skill names to
// "third-party" and the MCP servers to "custom"; these carry the real names.
var attributionDims = []struct {
	name string
	pick func(*transcript.Event) string
}{
	{"attribution_skill", func(ev *transcript.Event) string { return ev.AttributionSkill }},
	{"attribution_plugin", func(ev *transcript.Event) string { return ev.AttributionPlugin }},
	{"attribution_agent", func(ev *transcript.Event) string { return ev.AttributionAgent }},
	{"attribution_mcp_server", func(ev *transcript.Event) string { return ev.AttributionMcpServer }},
	{"attribution_mcp_tool", func(ev *transcript.Event) string { return ev.AttributionMcpTool }},
}

// dimKey is one row's identity: which table it belongs to and which row it is.
type dimKey struct {
	dimension string
	key       string
}

// weightKey is a dimension row's weight inside one session-model, which is the
// only grain a bill exists at and therefore the only grain to allocate from.
type weightKey struct {
	sessionModel
	dimension string
	key       string
}

// attachStat is an attachment rollup: events and the bytes they occupy.
type attachStat struct {
	Events int64
	Bytes  int64
}

func (a *attachStat) add(bytes int64) {
	a.Events++
	a.Bytes += bytes
}

// bucket returns m[k], creating it on first use.
func bucket[K comparable, V any](m map[K]*V, k K) *V {
	v := m[k]
	if v == nil {
		v = new(V)
		m[k] = v
	}
	return v
}

// AttributeEnvelope rolls deduplicated per-response tokens up by each of the
// five attribution dimensions, reports context re-billing, measures attachment
// volume two levels deep, and allocates the measured session bills across it
// all as an explicit estimate.
//
// One streaming pass. Only counters are retained — never a line of content.
func AttributeEnvelope(dir, version string) (Envelope, error) {
	usage := transcript.NewUsageSet()

	var total rebill
	byModel := map[sessionModel]*rebill{}
	byDim := map[dimKey]*rebill{}
	weights := map[weightKey]float64{}

	costStates := map[string]*transcript.CostState{}
	sessions := map[string]bool{}

	var attachTotal attachStat
	attachByType := map[string]*attachStat{}
	attachByKey := map[dimKey]*attachStat{}

	var naiveResponses int64
	bld := newBuilder(dir, version, "attribute")

	scanStats, err := transcript.Scan(dir, func(ev *transcript.Event) {
		bld.see(ev)
		if ev.SessionID != "" {
			sessions[ev.SessionID] = true
		}

		if cs := ev.CostState(); cs != nil {
			costStates[cs.SessionID] = cs
			return
		}
		if at := ev.Attachment(); at != nil {
			attachTotal.add(ev.LineBytes)
			bucket(attachByType, at.Type).add(ev.LineBytes)
			for k, b := range at.KeyBytes {
				bucket(attachByKey, dimKey{at.KeyDimension, k}).add(b)
			}
			return
		}

		u := ev.Usage()
		if u == nil {
			return
		}
		naiveResponses++
		// A repeated message.id is the same billed call written to the file
		// again. Counting it here is exactly the 2.5x inflation the naive sum
		// suffers, so the dedup gate comes before every aggregation below.
		if !usage.Add(u) {
			return
		}

		total.add(u)
		sm := sessionModel{session: ev.SessionID, model: u.Model}
		bucket(byModel, sm).add(u)

		w := responseWeight(u)
		roll := func(dimension, key string) {
			bucket(byDim, dimKey{dimension, key}).add(u)
			weights[weightKey{sessionModel: sm, dimension: dimension, key: key}] += w
		}
		roll(sessionDim, ev.SessionID)
		for _, d := range attributionDims {
			key := d.pick(ev)
			if key == "" {
				key = unattributedKey
			}
			roll(d.name, key)
		}
	})
	if err != nil {
		return Envelope{}, err
	}

	// Coverage first: a dollar figure allocated over a session whose transcript
	// holds a fraction of what was billed is a fraction of the truth, and the
	// reader has to be told so before being shown the money.
	coverageRows, billed, absent := coverageMetrics(costStates, byModel)
	costByDim := allocateDims(weights, byModel, costStates)

	// Every dimension allocates the same bills, so any one of them totals the
	// allocation. What it does not reach is the gap coverage just measured:
	// models billed with no transcript events to allocate across.
	var allocated float64
	for _, dk := range slices.SortedFunc(maps.Keys(costByDim), compareDimKey) {
		if dk.dimension == sessionDim {
			allocated += costByDim[dk]
		}
	}

	add := bld.add
	add("responses_naive", naiveResponses, "responses")
	add("responses_distinct", int64(usage.Distinct), "responses")
	add("responses_duplicate", int64(usage.Duplicates), "responses")
	add("duplicate_rate_percent", percent(int64(usage.Duplicates), naiveResponses), "percent")
	add("fresh_tokens", total.Fresh, "tokens")
	add("rebilled_tokens", total.Rebilled, "tokens")
	add("output_tokens", total.Output, "tokens")
	add("thinking_tokens", total.Thinking, "tokens")
	add("rebill_multiplier", total.Multiplier(), "ratio")
	add("sessions", int64(len(sessions)), "sessions")
	add("sessions_with_cost_state", int64(len(costStates)), "sessions")
	add("sessions_without_cost_state", int64(len(sessions)-len(costStates)), "sessions")
	add("cost_state_coverage_percent", percent(int64(len(costStates)), int64(len(sessions))), "percent")
	add("attachment_events", attachTotal.Events, "events")
	add("attachment_bytes", attachTotal.Bytes, "bytes")
	add("attachment_share_percent", percent(attachTotal.Bytes, scanStats.Bytes), "percent")
	add("attachment_types", int64(len(attachByType)), "types")
	bld.rows(
		EstimatedMetric("allocated_cost_usd", "corpus", "", allocated, "usd", AllocationMethod),
		EstimatedMetric("unallocated_cost_usd", "corpus", "", billed-allocated, "usd", AllocationMethod))

	for _, d := range attributionDims {
		bld.rows(rebillMetrics(d.name, byDim, costByDim)...)
	}
	bld.rows(rebillMetrics(sessionDim, byDim, costByDim)...)
	bld.rows(availabilityMetrics(byDim, costStates)...)
	bld.rows(coverageRows...)
	bld.rows(attachMetrics("attachment_type", attachByType)...)
	for _, dim := range []string{
		transcript.DimSkill, transcript.DimMcpServer, transcript.DimMcpTool,
		transcript.DimAgent, transcript.DimHookName,
	} {
		bld.rows(attachKeyMetrics(dim, attachByKey)...)
	}

	if usage.Duplicates > 0 {
		bld.warn("%d of %d assistant responses repeat a message.id already seen — deduplicated, not summed",
			usage.Duplicates, naiveResponses)
	}
	if missing := len(sessions) - len(costStates); missing > 0 {
		bld.warn("%d of %d sessions carry no cost-state event: dollars are unavailable for them, not zero",
			missing, len(sessions))
	}
	if absent != "" {
		bld.warn("%s", absent)
	}
	return bld.done(scanStats), nil
}

// allocateDims spreads each measured session-model bill across the dimension
// rows that earned it, in proportion to weighted tokens. A session-model with
// no bill contributes nothing — never a zero.
func allocateDims(weights map[weightKey]float64, byModel map[sessionModel]*rebill,
	costStates map[string]*transcript.CostState) map[dimKey]float64 {

	out := map[dimKey]float64{}
	// Sorted, not ranged: Go randomises map iteration and float addition is
	// not associative, so an unordered sum lands on a different last bit each
	// run. That is enough to make two `tare report --json` runs over an
	// unchanged corpus differ — the one thing the artifact promises they will
	// not do.
	for _, wk := range slices.SortedFunc(maps.Keys(weights), compareWeightKey) {
		w := weights[wk]
		cs := costStates[wk.session]
		if cs == nil {
			continue
		}
		mu, ok := cs.ModelUsage[wk.model]
		if !ok {
			continue
		}
		out[dimKey{wk.dimension, wk.key}] += allocate(mu.CostUSD, w, byModel[wk.sessionModel].Weight)
	}
	return out
}

// compareWeightKey is a total order over weightKey, so allocation sums in the
// same order on every run.
func compareWeightKey(a, b weightKey) int {
	return cmp.Or(
		strings.Compare(a.session, b.session),
		strings.Compare(a.model, b.model),
		strings.Compare(a.dimension, b.dimension),
		strings.Compare(a.key, b.key),
	)
}

// compareDimKey is the same total order over dimKey.
func compareDimKey(a, b dimKey) int {
	return cmp.Or(
		strings.Compare(a.dimension, b.dimension),
		strings.Compare(a.key, b.key),
	)
}

// coverageMetrics compares each billed session-model against what the
// transcript actually recorded for it. Session-models billed with no
// transcript events collapse into a single warning naming the distinct
// models: one line per session ran to ~50 rows on the real corpus and buried
// every other finding, and the per-session detail is already carried by the
// coverage_ratio rows.
func coverageMetrics(costStates map[string]*transcript.CostState,
	byModel map[sessionModel]*rebill) (rows []Metric, billed float64, absent string) {

	absentPairs := 0
	absentModels := map[string]bool{}
	for _, session := range slices.Sorted(maps.Keys(costStates)) {
		cs := costStates[session]
		billed += cs.TotalCostUSD
		for _, model := range slices.Sorted(maps.Keys(cs.ModelUsage)) {
			mu := cs.ModelUsage[model]
			key := session + "/" + model
			tr := byModel[sessionModel{session: session, model: model}]
			if tr == nil {
				rows = append(rows,
					MeasuredMetric("coverage_ratio", "cost_coverage", key, 0.0, "ratio"))
				absentPairs++
				absentModels[model] = true
				continue
			}
			rows = append(rows,
				MeasuredMetric("coverage_ratio", "cost_coverage", key,
					coverage(tr.Tokens(), mu.Tokens()), "ratio"),
				MeasuredMetric("transcript_tokens", "cost_coverage", key, tr.Tokens(), "tokens"),
				MeasuredMetric("billed_tokens", "cost_coverage", key, mu.Tokens(), "tokens"))
		}
	}
	if absentPairs > 0 {
		absent = fmt.Sprintf(
			"%d billed session-model pairs have no transcript events — billed work the transcript never recorded; models: %s",
			absentPairs, strings.Join(slices.Sorted(maps.Keys(absentModels)), ", "))
	}
	return rows, billed, absent
}

// rebillMetrics emits one dimension's rows, heaviest re-billing first.
func rebillMetrics(dimension string, byDim map[dimKey]*rebill, cost map[dimKey]float64) []Metric {
	keys := dimensionKeys(byDim, dimension, func(a, b *rebill) int {
		return cmp.Compare(b.Rebilled, a.Rebilled)
	})
	out := make([]Metric, 0, len(keys)*5)
	for _, k := range keys {
		dk := dimKey{dimension, k}
		r := byDim[dk]
		out = append(out,
			MeasuredMetric("responses", dimension, k, r.Responses, "responses"),
			MeasuredMetric("fresh_tokens", dimension, k, r.Fresh, "tokens"),
			MeasuredMetric("rebilled_tokens", dimension, k, r.Rebilled, "tokens"),
			MeasuredMetric("rebill_multiplier", dimension, k, r.Multiplier(), "ratio"))
		// No bill reached this row, so it has no cost_usd row at all. The
		// absence is the report: unavailable is not zero.
		if usd, billed := cost[dk]; billed {
			out = append(out, EstimatedMetric("cost_usd", dimension, k, usd, "usd", AllocationMethod))
		}
	}
	return out
}

// availabilityMetrics states, per session, whether dollars exist for it at
// all. Measured 2026-09-05, 77 of 134 sessions have none — this is how that
// absence is said out loud instead of left to be inferred from a missing row.
func availabilityMetrics(byDim map[dimKey]*rebill, costStates map[string]*transcript.CostState) []Metric {
	keys := dimensionKeys(byDim, sessionDim, func(a, b *rebill) int {
		return cmp.Compare(b.Rebilled, a.Rebilled)
	})
	out := make([]Metric, 0, len(keys))
	for _, k := range keys {
		available := int64(0)
		if costStates[k] != nil {
			available = 1
		}
		out = append(out, MeasuredMetric("dollars_available", sessionDim, k, available, "sessions"))
	}
	return out
}

// attachMetrics emits the top-level attachment rollup, by `attachment.type`.
func attachMetrics(dimension string, stats map[string]*attachStat) []Metric {
	keys := slices.SortedFunc(maps.Keys(stats), func(a, b string) int {
		return cmp.Or(cmp.Compare(stats[b].Bytes, stats[a].Bytes), strings.Compare(a, b))
	})
	out := make([]Metric, 0, len(keys)*2)
	for _, k := range keys {
		out = append(out,
			MeasuredMetric("attachment_events", dimension, k, stats[k].Events, "events"),
			MeasuredMetric("attachment_bytes", dimension, k, stats[k].Bytes, "bytes"))
	}
	return out
}

// attachKeyMetrics emits the second level: the key each attachment type names
// inside itself, measured off the rendered text that key contributed.
func attachKeyMetrics(dimension string, stats map[dimKey]*attachStat) []Metric {
	keys := dimensionKeys(stats, dimension, func(a, b *attachStat) int {
		return cmp.Compare(b.Bytes, a.Bytes)
	})
	out := make([]Metric, 0, len(keys)*2)
	for _, k := range keys {
		s := stats[dimKey{dimension, k}]
		out = append(out,
			MeasuredMetric("attachment_events", dimension, k, s.Events, "events"),
			MeasuredMetric("attachment_bytes", dimension, k, s.Bytes, "bytes"))
	}
	return out
}

// dimensionKeys pulls one dimension's keys out of a dimKey-indexed map, sorted
// by the caller's comparison and then by name so the order is deterministic.
func dimensionKeys[V any](m map[dimKey]*V, dimension string, by func(a, b *V) int) []string {
	var keys []string
	for dk := range m {
		if dk.dimension == dimension {
			keys = append(keys, dk.key)
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		return cmp.Or(by(m[dimKey{dimension, a}], m[dimKey{dimension, b}]), strings.Compare(a, b))
	})
	return keys
}

// percent is a share of two measured counts. Both sides are exact, so the
// ratio is measured too — nothing about it is derived.
func percent(part, whole int64) float64 {
	if whole == 0 {
		return 0
	}
	return 100 * float64(part) / float64(whole)
}

// tableRows caps how many rows of a dimension the human table prints. The
// envelope carries every row; the terminal does not need 134 sessions.
const tableRows = 15

// RenderAttribute prints the envelope as a table. Like the other renderers it
// reads only the envelope, so the table and `--json` cannot disagree.
func RenderAttribute(w io.Writer, env Envelope) error {
	renderHeader(w, env)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	writeCorpus(tw, env)

	for _, d := range attributionDims {
		writeRebillTable(tw, env, d.name)
	}
	writeRebillTable(tw, env, sessionDim)

	for _, dim := range []string{
		"attachment_type", transcript.DimSkill, transcript.DimMcpServer,
		transcript.DimMcpTool, transcript.DimAgent, transcript.DimHookName,
	} {
		rows := groupRows(env.Metrics, dim)
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(tw, "\n%s\tEVENTS\tBYTES\t\t\t\n", strings.ToUpper(dim))
		shown, withheld := truncate(rows)
		for _, r := range shown {
			fmt.Fprintf(tw, "%s\t%s\t%s\t\t\t\n", r.key,
				r.cell("attachment_events"),
				r.cell("attachment_bytes"))
		}
		writeWithheld(tw, withheld, dim)
	}
	return renderTail(w, tw, env)
}

func writeRebillTable(tw io.Writer, env Envelope, dimension string) {
	rows := groupRows(env.Metrics, dimension)
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(tw, "\n%s\tRESPONSES\tFRESH\tREBILLED\tMULTIPLIER\tUSD (est)\n", strings.ToUpper(dimension))
	shown, withheld := truncate(rows)
	for _, r := range shown {
		// A session with no cost-state has no cost_usd row at all. It says
		// "unavailable" and never "0.00" — the difference is the finding.
		usd := "unavailable"
		if _, ok := r.values["cost_usd"]; ok {
			usd = r.cell("cost_usd")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.key,
			r.cell("responses"),
			r.cell("fresh_tokens"),
			r.cell("rebilled_tokens"),
			r.cell("rebill_multiplier"),
			usd)
	}
	writeWithheld(tw, withheld, dimension)
}

// truncate caps a table at tableRows and reports how many it withheld.
func truncate(rows []row) (shown []row, withheld int) {
	if len(rows) <= tableRows {
		return rows, 0
	}
	return rows[:tableRows], len(rows) - tableRows
}

func writeWithheld(tw io.Writer, withheld int, dimension string) {
	if withheld > 0 {
		fmt.Fprintf(tw, "… and %d more %s rows (see --json)\t\t\t\t\t\n", withheld, dimension)
	}
}
