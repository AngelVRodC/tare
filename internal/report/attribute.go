package report

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"math"
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

// thinAttribution is the (unattributed) share of re-billed tokens above which
// a dimension's named rows are a minority of its own tokens, so the dollars
// beside them are a floor rather than a total.
//
// A judgment call, not a measurement — but half is the one value that makes the
// claim literally true: past it the rows no field claimed outweigh every named
// row combined. A lower gate would fire on dimensions where the named rows
// still hold the majority, and then the warning would overstate its case.
//
// On the corpus this was written against all five dimensions sit between 56%
// and 94%, so the gate does not separate them today. It exists to keep saying
// so, and to go quiet the day one drops below half — which is the only signal
// it can send.
const thinAttribution = 50.0

// attributionDims are the five un-redacted fields the product exists to read.
// Anthropic's own OTel export reduces the plugin and skill names to
// "third-party" and the MCP servers to "custom"; these carry the real names.
//
// field is the transcript field each one reads. It is carried so that a
// dimension no event ever claimed can name the field that was missing, not
// only the table that came out empty — the reader's next move depends on
// which of the two it is.
var attributionDims = []struct {
	name  string
	field string
	pick  func(*transcript.Event) string
}{
	{"attribution_skill", "attributionSkill", func(ev *transcript.Event) string { return ev.AttributionSkill }},
	{"attribution_plugin", "attributionPlugin", func(ev *transcript.Event) string { return ev.AttributionPlugin }},
	{"attribution_agent", "attributionAgent", func(ev *transcript.Event) string { return ev.AttributionAgent }},
	{"attribution_mcp_server", "attributionMcpServer", func(ev *transcript.Event) string { return ev.AttributionMcpServer }},
	{"attribution_mcp_tool", "attributionMcpTool", func(ev *transcript.Event) string { return ev.AttributionMcpTool }},
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
	// claimed counts, per dimension, the responses whose field carried a value.
	// Zero is categorically different from a low count: it means the corpus has
	// no such field at all, so no amount of further use will fill that table.
	claimed := map[string]int64{}

	costStates := map[string]*transcript.CostState{}
	sessions := map[string]bool{}

	var attachTotal attachStat
	attachByType := map[string]*attachStat{}
	attachByKey := map[dimKey]*attachStat{}

	var naiveResponses int64
	bld := newBuilder(dir, version, "attribute")

	scanStats, err := transcript.Scan(dir, func(ev *transcript.Event) {
		bld.seeTime(ev.Timestamp)
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
			} else {
				claimed[d.name]++
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
	var allocated int64
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
		EstimatedMetric("allocated_cost_usd", "corpus", "", allocated, "microdollars", AllocationMethod),
		EstimatedMetric("unallocated_cost_usd", "corpus", "", billed-allocated, "microdollars", AllocationMethod))

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
	// (unattributed) prints as an ordinary top row, but it is the confidence
	// bound on the whole table: every response no field claimed lands there, so
	// past thinAttribution the named rows own a minority of the tokens and each
	// dollar beside them is a floor. One line for all of them — five
	// near-identical lines is the noise 7ce2c75 collapsed for absent models.
	//
	// sessionDim is deliberately not checked. Its key is the raw session id and
	// never the sentinel, so that dimension has no (unattributed) row to
	// measure and a share there would be 0% by construction, not by coverage.
	//
	// Ranged over attributionDims rather than a map: the order is fixed by
	// declaration — the order the tables themselves print in — so the string
	// is byte-identical run to run, which is what TestReportReproducible pins.
	//
	// A dimension no response ever claimed is separated out first. Both come
	// out of the table as (unattributed), but they are different findings and
	// they imply opposite next moves: thin attribution can improve with more
	// use, an absent field cannot improve at all. Folding the second into
	// "majority-unclaimed" reports a floor where there is no measurement, and
	// naming a share for it would dress the absence up as coverage.
	var absentField, thin []string
	for _, d := range attributionDims {
		if claimed[d.name] == 0 {
			absentField = append(absentField, d.field)
			continue
		}
		r := byDim[dimKey{d.name, unattributedKey}]
		if r == nil {
			continue
		}
		if share := percent(r.Rebilled, total.Rebilled); share > thinAttribution {
			thin = append(thin, fmt.Sprintf("%s %.1f%%", d.name, share))
		}
	}
	if len(absentField) > 0 {
		bld.warn("%d of %d attribution dimensions are unmeasured, not unclaimed: no event in this corpus carries %s. Those tables cannot improve with further use — the Claude Code versions that wrote these transcripts never recorded the field",
			len(absentField), len(attributionDims), strings.Join(absentField, ", "))
	}
	if len(thin) > 0 {
		// The qualifier leads and the list follows. Trailing it after the names
		// read as if it applied only to the last one.
		bld.warn("%d of %d attribution dimensions are majority-unclaimed — over %.0f%% of their re-billed tokens are (unattributed): %s. Every dollar figure in those tables is a floor, not a total",
			len(thin), len(attributionDims), thinAttribution, strings.Join(thin, ", "))
	}
	return bld.done(scanStats), nil
}

// allocateDims spreads each measured session-model bill across the dimension
// rows that earned it, in proportion to weighted tokens. A session-model with
// no bill contributes nothing — never a zero.
func allocateDims(weights map[weightKey]float64, byModel map[sessionModel]*rebill,
	costStates map[string]*transcript.CostState) map[dimKey]int64 {

	out := map[dimKey]int64{}
	// Sorted, not ranged: an unordered sum once made two `tare report --json`
	// runs differ in the last bit of every dollar figure. Integer addition is
	// associative, so the order no longer moves the total — but the sorted
	// walk costs nothing and keeps the property from depending on that.
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
	byModel map[sessionModel]*rebill) (rows []Metric, billed int64, absent string) {

	absentPairs := 0
	absentModels := map[string]bool{}
	for _, session := range slices.Sorted(maps.Keys(costStates)) {
		cs := costStates[session]
		billed += int64(math.Round(cs.TotalCostUSD * 1e6))
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
func rebillMetrics(dimension string, byDim map[dimKey]*rebill, cost map[dimKey]int64) []Metric {
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
		if micro, billed := cost[dk]; billed {
			out = append(out, EstimatedMetric("cost_usd", dimension, k, micro, "microdollars", AllocationMethod))
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

// RenderAttribute prints the envelope as a table. Like the other renderers it
// reads only the envelope, so the table and `--json` cannot disagree.
//
// top caps how many rows of a dimension each table prints; zero or less prints
// every one. The envelope always carries them all, so the cap only ever
// changes what a terminal is asked to scroll.
func RenderAttribute(w io.Writer, env Envelope, top int) error {
	renderHeader(w, env)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	writeCorpus(tw, env, "")

	for _, d := range attributionDims {
		writeRebillTable(tw, env, d.name, top)
	}
	writeRebillTable(tw, env, sessionDim, top)

	attachments := corpusValue(env, "attachment_bytes")
	for _, dim := range []string{
		"attachment_type", transcript.DimSkill, transcript.DimMcpServer,
		transcript.DimMcpTool, transcript.DimAgent, transcript.DimHookName,
	} {
		rows := groupRows(env.Metrics, dim)
		if len(rows) == 0 {
			continue
		}
		// Not every attachment dimension partitions attachment_bytes: three of
		// the six are sub-rollups whose largest row is under half a percent of
		// it, and there the column would print 0.0% on every row and deliver no
		// verdict — the same defect the uniform derivation column had. Measured
		// rather than listed by name, so this follows the corpus instead of the
		// one it was written against.
		whole, header := attachments, "SHARE"
		if !gradeable(rows, "attachment_bytes", whole) {
			whole, header = 0, ""
		}
		fmt.Fprintf(tw, "\n%s\tEVENTS\tBYTES\t%s\t\t\n", strings.ToUpper(dim), header)
		shown, total := truncate(rows, top)
		for _, r := range shown {
			pct, grade := r.share("attachment_bytes", whole)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t\n", r.key,
				r.cell("attachment_events"),
				r.cell("attachment_bytes"),
				pct, grade)
		}
		writeTruncation(tw, len(shown), total, dim)
	}
	return renderTail(w, tw, env)
}

// gradeable reports whether any row of a table is a large enough slice of the
// corpus to earn a concern mark. A SHARE column whose every row grades blank
// answers nothing the ranking has not already answered.
func gradeable(rows []row, name string, whole int64) bool {
	for _, r := range rows {
		if concern(percent(r.num(name), whole)) != "" {
			return true
		}
	}
	return false
}

func writeRebillTable(tw io.Writer, env Envelope, dimension string, top int) {
	rows := groupRows(env.Metrics, dimension)
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(tw, "\n%s\tRESPONSES\tFRESH\tREBILLED\tMULTIPLIER\tUSD (est)\tSHARE\t\n", strings.ToUpper(dimension))
	rebilled := corpusValue(env, "rebilled_tokens")
	shown, total := truncate(rows, top)
	for _, r := range shown {
		// A session with no cost-state has no cost_usd row at all. It says
		// "unavailable" and never "0.00" — the difference is the finding.
		usd := "unavailable"
		if _, ok := r.values["cost_usd"]; ok {
			usd = r.cell("cost_usd")
		}
		pct, grade := r.share("rebilled_tokens", rebilled)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.key,
			r.cell("responses"),
			r.cell("fresh_tokens"),
			r.cell("rebilled_tokens"),
			r.cell("rebill_multiplier"),
			usd, pct, grade)
	}
	writeTruncation(tw, len(shown), total, dimension)
}

// truncate caps a table at top rows and reports the true total, not the
// remainder: the line under the table names both halves, and a caller that got
// only the remainder would have to add them back.
//
// A top of zero or less truncates nothing, which is what `--all` and `--top 0`
// both ask for — one rule covers them, so there is no second sentinel.
func truncate(rows []row, top int) (shown []row, total int) {
	if top <= 0 || len(rows) <= top {
		return rows, len(rows)
	}
	return rows[:top], len(rows)
}

// writeTruncation says a table was cut and names the flag that uncuts it. The
// line it replaced printed a count with no way to reach the rows it hid.
//
// No sort key is named. Three tables share this line and each ranks by a
// different column — attachment bytes, re-billed tokens and calls — so any one
// key would be a false claim on the other two.
//
// It carries no tabs on purpose: a tab-terminated first cell joins the table's
// column block above it and stretches that column to the width of this whole
// sentence.
func writeTruncation(tw io.Writer, shown, total int, dimension string) {
	if shown < total {
		fmt.Fprintf(tw, "showing top %d of %d %s rows — use --all\n", shown, total, dimension)
	}
}
