package report

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/AngelVRodC/tare/internal/transcript"
)

// The two pattern thresholds. Both are judgment calls fixed as constants,
// not flags: a tuning knob nobody documented in usage() would be a lie.
const (
	// retryLoopThreshold is how many consecutive errored results on one
	// identical call make a loop. Two is one retry, which is what a transient
	// failure looks like; three means the agent kept asking the same question
	// of the same thing and kept getting the same answer.
	retryLoopThreshold = 3

	// repeatedFailureMinSessions is the cross-session bar: the same call
	// erroring in at least this many distinct sessions is a standing defect
	// of the tool, not of one conversation.
	repeatedFailureMinSessions = 2

	// hashLen is how many hex chars of the payload digest land in a key: 48
	// bits, a birthday ceiling around 16M distinct payloads per corpus — far
	// past what one machine records, and a collision could only merge two
	// rows' identities, never split one. A full 64-char digest in a key buys
	// nothing a terminal table can show.
	hashLen = 12
)

// callInfo is everything the join needs from a tool_use event. A tool_result
// names a tool_use id and nothing else — not the tool, not the payload, not
// the turn that asked — so all three are captured when the use streams past,
// the same join corruption.go makes with its names map.
type callInfo struct {
	tool string
	hash string // payloadHash of the tool_use input
	// The attribution of the assistant turn that made the call. Attribution
	// fields ride the response, not the result event, so rolling failures up
	// by them only works if they are carried across the join here.
	skill     string
	plugin    string
	mcpServer string
}

// loopState is the retry-loop detector's entire per-session memory: the last
// call seen and its current consecutive-error run. Never a call list.
type loopState struct {
	tool   string
	hash   string
	count  int64 // consecutive is_error results on this exact call
	denied int64 // the is_error share that carried a toolDenialKind
	inWin  bool  // at least one in-window errored result in this run
}

// retryOccurrence is one completed run that reached the loop threshold.
type retryOccurrence struct {
	session, tool, hash string
	streak, denied      int64
	inWin               bool
}

// loopRow is an occurrence after keying and the window filter.
type loopRow struct {
	key    string
	streak int64
	denied int64
}

// rfKey is one (tool, normalized payload) pair — the identity the
// cross-session pattern counts by.
type rfKey struct{ tool, hash string }

func (k rfKey) String() string { return k.tool + "/" + k.hash }

// rfStat is one pair's windowed counts plus the sessions that errored it.
// corruptStat is reused as-is: its Empty/Truncated half simply never moves
// here, and Calls/Errors/Denied/Failures keep the issue #4 partition
// vocabulary identical across the two commands.
type rfStat struct {
	corruptStat
	errSessions map[string]bool
}

// failuresDims are the attribution rollups of the failures command: the three
// fields that name installed tooling. attribution_agent and
// attribution_mcp_tool stay with `attribute` — a subagent or one MCP tool
// failing is not a harness-cost finding, and every extra table is a table to
// keep honest. The names are the same dimension names `attribute` emits,
// because the JSON name is the contract; TestFailuresAttributionDimsExist
// pins that they are still in attributionDims.
var failuresDims = []struct {
	name  string
	field string
	pick  func(*callInfo) string
}{
	{"attribution_skill", "attributionSkill", func(c *callInfo) string { return c.skill }},
	{"attribution_plugin", "attributionPlugin", func(c *callInfo) string { return c.plugin }},
	{"attribution_mcp_server", "attributionMcpServer", func(c *callInfo) string { return c.mcpServer }},
}

// FailuresEnvelope reports the failure *patterns* the transcript shows, not
// per-tool rates: retry loops (the same tool on the same call payload failing
// ≥3 consecutive times within one session), the same call erroring across ≥2
// distinct sessions, and errored calls rolled up by the attribution fields of
// the turns that made them.
//
// One streaming pass over the corpus, through transcript.Scan like every
// command, so the unreadable-file and parse-error warnings are inherited, not
// re-implemented. Retained state is bounded by distinct calls and sessions —
// the join map is the precedent corruption.go already sets — never by lines.
//
// Window semantics mirror corruption (time-window-6) with one addition: the
// join map *and the streak state machine* run corpus-wide, because a window
// cut must neither sever a join nor hide the out-of-window success between two
// in-window errors and invent a loop out of two separate ones. The per-key
// counts aggregate in-window results only, and a detected loop whose errored
// results all sit outside the window is omitted, never zeroed.
//
// Denials keep the corruption split (issue #4): a blocked call is evidence of
// a policy, not of a tool altering an answer, so `failures` is what remains of
// `errors` everywhere it is reported. For *pattern* detection an is_error is
// an is_error — a denial still cost the agent a turn and returned nothing,
// which is exactly what a loop looks like from the inside — so loops and
// cross-session pairs count them and carry the denied portion in rows of
// their own, named in a warning.
func FailuresEnvelope(dir, version string, w Window) (Envelope, error) {
	calls := map[string]*callInfo{} // tool_use_id → call
	loops := map[string]*loopState{}
	rf := map[rfKey]*rfStat{}
	attr := map[dimKey]*corruptStat{}
	attrClaimed := map[string]int64{}
	var occurrences []retryOccurrence
	var unmatched, ambiguousDenials, erroredInWin int64

	b := newBuilder(dir, version, "failures", w)

	scanStats, err := transcript.Scan(dir, func(ev *transcript.Event) {
		b.seeTime(ev.Timestamp, ev.Type)
		inWin := w.Includes(ev.Timestamp)
		uses, res := ev.Blocks()
		for _, u := range uses {
			calls[u.ID] = &callInfo{
				tool: u.Name, hash: payloadHash(u.Input),
				skill: ev.AttributionSkill, plugin: ev.AttributionPlugin,
				mcpServer: ev.AttributionMcpServer,
			}
		}
		if len(res) == 0 {
			return
		}
		// toolDenialKind is per event and is_error is per block; the ambiguity
		// is counted and reported, never assumed away — same audit as
		// corruption's. Defect detection stays unconditional of the window.
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
			c, ok := calls[r.ToolUseID]
			if !ok {
				unmatched++
				continue
			}

			// The state machine sees every joined result, in window or not:
			// consecutive is consecutive, and skipping out-of-window results
			// would let a window cut manufacture a streak.
			st := bucket(loops, ev.SessionID)
			if st.tool != c.tool || st.hash != c.hash {
				st.flush(ev.SessionID, &occurrences)
				st.tool, st.hash = c.tool, c.hash
			}
			if r.IsError {
				st.count++
				if ev.ToolDenialKind != "" {
					st.denied++
				}
				if inWin {
					st.inWin = true
				}
			} else {
				st.flush(ev.SessionID, &occurrences)
			}

			if !inWin {
				continue
			}

			s := bucket(rf, rfKey{tool: c.tool, hash: c.hash})
			s.Calls++
			if !r.IsError {
				continue
			}
			erroredInWin++
			s.Errors++
			denied := ev.ToolDenialKind != ""
			if denied {
				s.Denied++
			} else {
				s.Failures++
			}
			if s.errSessions == nil {
				s.errSessions = map[string]bool{}
			}
			s.errSessions[ev.SessionID] = true
			for _, d := range failuresDims {
				key := d.pick(c)
				if key == "" {
					continue // absent is not zero: no field, no row
				}
				a := bucket(attr, dimKey{d.name, key})
				a.Errors++
				if denied {
					a.Denied++
				} else {
					a.Failures++
				}
				attrClaimed[d.name]++
			}
		}
	})
	if err != nil {
		return Envelope{}, err
	}

	// Sessions whose last run never broke ends the scan mid-streak; flushing
	// in sorted session order keeps even this append order deterministic.
	for _, session := range slices.Sorted(maps.Keys(loops)) {
		loops[session].flush(session, &occurrences)
	}

	// Keys are assigned in scan order — occurrences were appended as they
	// completed, which file order fixes — so the #N suffix names the corpus
	// truth even when a window omits the earlier occurrences of the triple.
	seq := map[string]int{}
	rows := make([]loopRow, 0, len(occurrences))
	var loopDenied int64
	for _, o := range occurrences {
		if w.Active() && !o.inWin {
			continue
		}
		triple := o.session + "/" + o.tool + "/" + o.hash
		seq[triple]++
		key := triple
		if n := seq[triple]; n > 1 {
			key = fmt.Sprintf("%s#%d", triple, n)
		}
		rows = append(rows, loopRow{key: key, streak: o.streak, denied: o.denied})
		loopDenied += o.denied
	}
	slices.SortFunc(rows, func(a, b loopRow) int {
		return cmp.Or(cmp.Compare(b.streak, a.streak), strings.Compare(a.key, b.key))
	})

	// The eligible set is collected from the map, then sorted by a total
	// order (count desc, key asc — keys are unique), so map range order can
	// never reach the output.
	pairs := make([]rfKey, 0, len(rf))
	var pairDenied int64
	for k, s := range rf {
		if len(s.errSessions) < repeatedFailureMinSessions {
			continue
		}
		pairs = append(pairs, k)
		pairDenied += s.Denied
	}
	slices.SortFunc(pairs, func(a, b rfKey) int {
		return cmp.Or(cmp.Compare(rf[b].Errors, rf[a].Errors), strings.Compare(a.String(), b.String()))
	})

	add, addWin := b.add, b.addWin
	addWin("retry_loops", int64(len(rows)), "loops")
	addWin("repeated_failure_pairs", int64(len(pairs)), "pairs")
	// The denominator of the attribution tables: every in-window errored
	// result with a tool_use to join to. The rows reach it only where the
	// calling turn recorded a field, so errors outside the tables are the
	// unattributed residue the absent-field warning below sizes.
	addWin("errored_results", erroredInWin, "calls")
	add("unmatched_results", unmatched, "blocks")

	for _, r := range rows {
		b.rows(
			MeasuredMetric("consecutive_errors", "retry_loop", r.key, r.streak, "calls"),
			MeasuredMetric("consecutive_denials", "retry_loop", r.key, r.denied, "calls"))
	}
	for _, k := range pairs {
		s := rf[k]
		key := k.String()
		b.rows(
			MeasuredMetric("calls", "repeated_failure", key, s.Calls, "calls"),
			MeasuredMetric("errors", "repeated_failure", key, s.Errors, "calls"),
			MeasuredMetric("failures", "repeated_failure", key, s.Failures, "calls"),
			MeasuredMetric("denied", "repeated_failure", key, s.Denied, "calls"),
			MeasuredMetric("error_sessions", "repeated_failure", key, int64(len(s.errSessions)), "sessions"))
	}
	// Ranged over the declared slice, never a map: the table order is fixed
	// by declaration and the row order inside each table is a total order
	// (failures outrank errors, per corruptMetrics' reasoning — ranking on
	// errors alone promotes a denial-only key over a genuinely failing one).
	for _, d := range failuresDims {
		for _, key := range dimensionKeys(attr, d.name, func(a, c *corruptStat) int {
			return cmp.Or(cmp.Compare(c.Failures, a.Failures), cmp.Compare(c.Errors, a.Errors))
		}) {
			a := attr[dimKey{d.name, key}]
			b.rows(
				MeasuredMetric("errors", d.name, key, a.Errors, "calls"),
				MeasuredMetric("failures", d.name, key, a.Failures, "calls"),
				MeasuredMetric("denied", d.name, key, a.Denied, "calls"))
		}
	}

	if denied := loopDenied + pairDenied; denied > 0 {
		b.warn("%d of the is_error results behind the retry_loop and repeated_failure rows are policy "+
			"denials labelled toolDenialKind, not tool failures — the pattern is still real (a denial cost "+
			"a turn and returned nothing), but the failing half is the failures/consecutive_denials columns, "+
			"and the attribution rows carry the same split", denied)
	}
	if ambiguousDenials > 0 {
		b.warn("%d events carry a toolDenialKind alongside more than one errored tool_result; "+
			"the field is per event and is_error is per block, so the denial is counted against "+
			"each of them and those rows are an upper bound", ambiguousDenials)
	}
	// A dimension with no row while errors exist is categorical: either no
	// errored call rode on a turn that recorded the field, or no turn in the
	// corpus does. Say which tables are unavailable, not empty — ranged over
	// the declared slice, so the string is byte-identical run to run.
	var absent []string
	for _, d := range failuresDims {
		if attrClaimed[d.name] == 0 {
			absent = append(absent, d.field)
		}
	}
	if erroredInWin > 0 && len(absent) > 0 {
		b.warn("%d of %d failure-attribution dimensions carry no row: no errored call was made on a turn "+
			"that recorded %s — those tables are unavailable for this corpus, not empty, and further use "+
			"will not fill a field the harness never wrote",
			len(absent), len(failuresDims), strings.Join(absent, ", "))
	}
	return b.done(scanStats), nil
}

// flush records the current run if it was a loop, and always resets the run.
func (st *loopState) flush(session string, out *[]retryOccurrence) {
	if st.count >= retryLoopThreshold {
		*out = append(*out, retryOccurrence{
			session: session, tool: st.tool, hash: st.hash,
			streak: st.count, denied: st.denied, inWin: st.inWin})
	}
	st.count, st.denied, st.inWin = 0, 0, false
}

// payloadHash normalizes a tool_use input and returns a short digest of it.
// "Identical call payload" has to mean semantically identical, not
// byte-identical: the model re-generates the JSON on every retry, so key
// order and whitespace can move while the call stays the same call. Decoding
// with UseNumber and re-encoding sorts object keys and preserves numeric
// literals exactly — float64 would merge two distinct 21-digit integers into
// one float and invent a loop out of two different calls. Input json refuses
// falls back to its trimmed bytes, where same-in means same-hash; a
// normalization that failed open would merge every broken payload into one
// hash. Number *spelling* ("1.0" vs "1") is deliberately left distinct: the
// conservative direction, a missed loop rather than an invented one.
func payloadHash(raw json.RawMessage) string {
	var canonical []byte
	if len(raw) > 0 {
		var v any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&v) == nil {
			canonical, _ = json.Marshal(v)
		} else {
			canonical = bytes.TrimSpace(slices.Clone(raw))
		}
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])[:hashLen]
}

// RenderFailures prints the envelope as a table. Like every other renderer it
// reads only the envelope, so the table and `--json` cannot disagree.
//
// top caps the two ranked pattern tables the way RenderCorruption caps the
// tool table; zero or less prints every row. The attribution rollups print in
// full: their rows are bounded by installed tooling, not by calls, and the
// same short list is the suspect set — cutting it hides the small-name
// failures the table exists to surface.
//
// Absence is the message: a block with no rows prints no table at all, and
// the summary line fires only when every *pattern* table is empty. An empty
// attribution table never triggers it — the envelope's own warning already
// says whether those tables are unavailable or empty, and patterns can stand
// on their own without a field to blame.
//
// Row order comes straight from the envelope: FailuresEnvelope sorted each
// dimension by a total order, so re-sorting here could only ever disagree
// with `--json`.
func RenderFailures(w io.Writer, env Envelope, top int) error {
	renderHeader(w, env)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// Three padding cells: the widest table below is six columns.
	writeCorpus(tw, env, "\t\t\t")

	loops := groupRows(env.Metrics, "retry_loop")
	if len(loops) > 0 {
		// ERRORS is the streak length and DENIED its denied share — the
		// same subset relation the corruption tool table prints. The key
		// already names session/tool/payload-hash; there is no timestamp in
		// the envelope to print, and inventing a column for one would be a
		// claim the join never made.
		fmt.Fprint(tw, "\nRETRY_LOOP\tERRORS\tDENIED\t\t\t\n")
		shown, total := truncate(loops, top)
		for _, r := range shown {
			fmt.Fprintf(tw, "%s\t%s\t%s\t\t\t\n", r.key,
				r.cell("consecutive_errors"), r.cell("consecutive_denials"))
		}
		writeTruncation(tw, len(shown), total, "retry_loop")
	}

	pairs := groupRows(env.Metrics, "repeated_failure")
	if len(pairs) > 0 {
		fmt.Fprint(tw, "\nREPEATED_FAILURE\tCALLS\tERRORS\tFAILED\tDENIED\tSESSIONS\t\n")
		shown, total := truncate(pairs, top)
		for _, r := range shown {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t\n", r.key,
				r.cell("calls"), r.cell("errors"), r.cell("failures"),
				r.cell("denied"), r.cell("error_sessions"))
		}
		writeTruncation(tw, len(shown), total, "repeated_failure")
	}

	if len(loops) == 0 && len(pairs) == 0 {
		// No tabs: this line sits in the column block and a tab here would
		// stretch a column to its width — the trailing-annotation trap.
		fmt.Fprint(tw, "\nno failure patterns detected\n")
	}

	for _, d := range failuresDims {
		rows := groupRows(env.Metrics, d.name)
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(tw, "\n%s\tERRORS\tFAILED\tDENIED\t\t\n", strings.ToUpper(d.name))
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t\t\n", r.key,
				r.cell("errors"), r.cell("failures"), r.cell("denied"))
		}
	}
	return renderTail(w, tw, env)
}
