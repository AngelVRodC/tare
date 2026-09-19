package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The fixture helpers here differ from tools_test.go's in one way the patterns
// force: they take the session and the tool_use input, because retry_loop keys
// within a session and both patterns key on the normalized payload.

const (
	cmdLS     = `{"command":"ls"}`
	cmdLS2    = `{"command":"ls","timeout":30}`
	cmdLSDir  = `{"command":"ls -la"}`
	cmdGrep   = `{"pattern":"x"}`
	cmdPathZ  = `{"path":"z"}`
	cmdPathX1 = `{"path":"x"}`
)

func useIn(ts, session, id, name, input, attrs string) string {
	return `{"type":"assistant","sessionId":"` + session + `","timestamp":"` + ts + `",` +
		attrs + `"message":{"role":"assistant","content":[{"type":"tool_use","id":"` + id +
		`","name":"` + name + `","input":` + input + `}]}}`
}

// errRes is an errored tool_result in a named session. kind, when non-empty,
// is the top-level toolDenialKind Claude Code writes on a blocked call.
func errRes(ts, session, id, kind string) string {
	denial := ""
	if kind != "" {
		denial = `"toolDenialKind":"` + kind + `",`
	}
	return `{"type":"user","sessionId":"` + session + `","timestamp":"` + ts + `",` + denial +
		`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id +
		`","content":"boom","is_error":true}]}}`
}

func okRes(ts, session, id string) string {
	return `{"type":"user","sessionId":"` + session + `","timestamp":"` + ts + `",` +
		`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id +
		`","content":"fine","is_error":false}]}}`
}

// tsAt returns the n-th second of the fixtures' base minute, so a fixture can
// order its events without spelling twenty timestamps.
func tsAt(n int) string {
	return fmt.Sprintf("2026-09-01T10:00:%02d.000Z", n)
}

func loopKey(session, tool, input string) string {
	return session + "/" + tool + "/" + payloadHash(json.RawMessage(input))
}

func pairKey(tool, input string) string {
	return tool + "/" + payloadHash(json.RawMessage(input))
}

// failingLoop is n consecutive errored results for the same tool+payload in
// one session, uses and results interleaved the way the harness writes them.
// idp namespaces the tool_use ids, because fixtures compose several runs.
func failingLoop(session, idp, tool, input, kind string, n int) []string {
	out := make([]string, 0, n*2)
	for i := range n {
		u := fmt.Sprintf("%s-%d", idp, i)
		out = append(out,
			useIn(tsAt(i+1), session, u, tool, input, ""),
			errRes(tsAt(i+30), session, u, kind))
	}
	return out
}

// cleanCall is failingLoop's successful single-call twin.
func cleanCall(ts, session, id, tool, input string) []string {
	return []string{useIn(ts, session, id, tool, input, ""), okRes(ts, session, id)}
}

// dimRows returns every metric of one dimension, in emission order.
func dimRows(env Envelope, dimension string) []Metric {
	var out []Metric
	for _, m := range env.Metrics {
		if m.Dimension == dimension {
			out = append(out, m)
		}
	}
	return out
}

// TestFailuresCleanCorpus is the no-crash, no-finding baseline: a corpus
// whose calls all succeed emits no pattern rows and honest measured zeros for
// the counters.
func TestFailuresCleanCorpus(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // hermetic: this gate is transcript-only
	dir := toolCorpus(t,
		useIn(tsAt(1), "s1", "t1", "Bash", cmdLS, ""), okRes(tsAt(2), "s1", "t1"),
		useIn(tsAt(3), "s1", "t2", "Read", `{"path":"a"}`, ""), okRes(tsAt(4), "s1", "t2"),
		result("ghost", `"orphan"`, true), // unmatched: audited, never patterned
	)
	env, err := FailuresEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("FailuresEnvelope: %v", err)
	}
	if got := metricValue(t, env, "retry_loops", "corpus", ""); got != int64(0) {
		t.Errorf("retry_loops = %v, want 0", got)
	}
	if got := metricValue(t, env, "repeated_failure_pairs", "corpus", ""); got != int64(0) {
		t.Errorf("repeated_failure_pairs = %v, want 0", got)
	}
	if got := metricValue(t, env, "errored_results", "corpus", ""); got != int64(0) {
		t.Errorf("errored_results = %v, want 0 — the ghost result never joined a call", got)
	}
	if got := metricValue(t, env, "unmatched_results", "corpus", ""); got != int64(1) {
		t.Errorf("unmatched_results = %v, want 1 — join audit stays corpus-wide", got)
	}
	for _, dim := range []string{"retry_loop", "repeated_failure", "attribution_skill"} {
		if rows := dimRows(env, dim); len(rows) != 0 {
			t.Errorf("%s table is not empty: %+v", dim, rows)
		}
	}
	if err := env.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestFailuresRetryLoop pins the trigger (≥3 consecutive) and its extension:
// a fourth error deepens the one occurrence, it does not start a second.
func TestFailuresRetryLoop(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := toolCorpus(t, failingLoop("s1", "a", "Bash", cmdLS, "", 4)...)
	env, err := FailuresEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("FailuresEnvelope: %v", err)
	}
	key := loopKey("s1", "Bash", cmdLS)
	for _, c := range []struct {
		name, dim, key string
		want           any
	}{
		{"consecutive_errors", "retry_loop", key, int64(4)},
		{"consecutive_denials", "retry_loop", key, int64(0)},
		{"retry_loops", "corpus", "", int64(1)},
	} {
		if got := metricValue(t, env, c.name, c.dim, c.key); got != c.want {
			t.Errorf("%s/%s/%s = %v, want %v", c.name, c.dim, c.key, got, c.want)
		}
	}
}

// TestFailuresStreakBelowThresholdAndBroken covers the two must-NOT-trigger
// shapes: a run of two (under the bar), and 2+clean+2 on one triple (each
// half under the bar — a broken streak is two streaks, never one of four).
func TestFailuresStreakBelowThresholdAndBroken(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	t.Run("two consecutive never fires", func(t *testing.T) {
		dir := toolCorpus(t, failingLoop("s1", "a", "Bash", cmdLS, "", 2)...)
		env, err := FailuresEnvelope(dir, "test", Window{})
		if err != nil {
			t.Fatalf("FailuresEnvelope: %v", err)
		}
		if got := metricOrNil(env, "consecutive_errors", "retry_loop", loopKey("s1", "Bash", cmdLS)); got != nil {
			t.Errorf("a 2-run emitted %v — the threshold is 3", got)
		}
		if got := metricValue(t, env, "retry_loops", "corpus", ""); got != int64(0) {
			t.Errorf("retry_loops = %v, want 0", got)
		}
	})

	t.Run("a clean call breaks the run", func(t *testing.T) {
		lines := failingLoop("s1", "a", "Bash", cmdLS, "", 2)
		lines = append(lines, cleanCall(tsAt(10), "s1", "c", "Bash", cmdLS)...)
		lines = append(lines, failingLoop("s1", "b", "Bash", cmdLS, "", 2)...)
		dir := toolCorpus(t, lines...)
		env, err := FailuresEnvelope(dir, "test", Window{})
		if err != nil {
			t.Fatalf("FailuresEnvelope: %v", err)
		}
		if rows := dimRows(env, "retry_loop"); len(rows) != 0 {
			t.Errorf("2+clean+2 emitted %+v — the clean result broke the streak", rows)
		}
	})
}

// TestFailuresTwoOccurrences checks the one-row-per-occurrence rule: 3, clean,
// 3 on one triple is a loop that happened twice, the second keyed with #2.
func TestFailuresTwoOccurrences(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	lines := failingLoop("s1", "a", "Bash", cmdLS, "", 3)
	lines = append(lines, cleanCall(tsAt(10), "s1", "c", "Bash", cmdLS)...)
	lines = append(lines, failingLoop("s1", "b", "Bash", cmdLS, "", 3)...)
	dir := toolCorpus(t, lines...)
	env, err := FailuresEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("FailuresEnvelope: %v", err)
	}
	k := loopKey("s1", "Bash", cmdLS)
	if got := metricValue(t, env, "consecutive_errors", "retry_loop", k); got != int64(3) {
		t.Errorf("first occurrence = %v, want 3", got)
	}
	if got := metricValue(t, env, "consecutive_errors", "retry_loop", k+"#2"); got != int64(3) {
		t.Errorf("second occurrence = %v, want 3 under key %s#2", got, k)
	}
	if got := metricValue(t, env, "retry_loops", "corpus", ""); got != int64(2) {
		t.Errorf("retry_loops = %v, want 2", got)
	}
}

// TestFailuresCrossSession pins the repeated_failure shape: one error per
// session in two sessions is no loop but IS a cross-session pair; a third
// session's clean call rides the same pair's calls count; and a pair that
// errors twice in one session only meets neither bar.
func TestFailuresCrossSession(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := toolCorpus(t,
		useIn(tsAt(1), "s1", "a1", "Bash", cmdLS, ""), errRes(tsAt(2), "s1", "a1", ""),
		useIn(tsAt(3), "s2", "b1", "Bash", cmdLS, ""), errRes(tsAt(4), "s2", "b1", ""),
		useIn(tsAt(5), "s3", "c1", "Bash", cmdLS, ""), okRes(tsAt(6), "s3", "c1"),
		// Two errors, one session: under the loop bar, under the pair bar.
		useIn(tsAt(7), "s4", "d1", "Read", cmdPathX1, ""), errRes(tsAt(8), "s4", "d1", ""),
		useIn(tsAt(9), "s4", "d2", "Read", cmdPathX1, ""), errRes(tsAt(10), "s4", "d2", ""),
	)
	env, err := FailuresEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("FailuresEnvelope: %v", err)
	}
	key := pairKey("Bash", cmdLS)
	for _, c := range []struct {
		name, dim, key string
		want           any
	}{
		{"calls", "repeated_failure", key, int64(3)},
		{"errors", "repeated_failure", key, int64(2)},
		{"failures", "repeated_failure", key, int64(2)},
		{"denied", "repeated_failure", key, int64(0)},
		{"error_sessions", "repeated_failure", key, int64(2)},
		{"repeated_failure_pairs", "corpus", "", int64(1)},
	} {
		if got := metricValue(t, env, c.name, c.dim, c.key); got != c.want {
			t.Errorf("%s/%s/%s = %v, want %v", c.name, c.dim, c.key, got, c.want)
		}
	}
	if rows := dimRows(env, "retry_loop"); len(rows) != 0 {
		t.Errorf("one error per session emitted a loop: %+v", rows)
	}
	if got := metricOrNil(env, "calls", "repeated_failure", pairKey("Read", cmdPathX1)); got != nil {
		t.Errorf("single-session double error emitted %v — the pair bar is 2 distinct sessions", got)
	}
}

// TestFailuresDenialKeepsTheSplit pins issue #4 inside the new tables: a
// denied result is an is_error for pattern detection (it cost the turn), but
// the denied/failures partition says which half is about a policy, and the
// warning says it out loud.
func TestFailuresDenialKeepsTheSplit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	attr := `"attributionSkill":"golang-cli",`
	lines := make([]string, 0, 6)
	for i := range 3 {
		u := fmt.Sprintf("t%d", i)
		lines = append(lines,
			useIn(tsAt(i+1), "s1", u, "Bash", cmdLS, attr),
			errRes(tsAt(i+30), "s1", u, "permission-rule"))
	}
	dir := toolCorpus(t, lines...)
	env, err := FailuresEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("FailuresEnvelope: %v", err)
	}
	key := loopKey("s1", "Bash", cmdLS)
	for _, c := range []struct {
		name, dim, key string
		want           any
	}{
		{"consecutive_errors", "retry_loop", key, int64(3)},
		{"consecutive_denials", "retry_loop", key, int64(3)},
		{"errors", "attribution_skill", "golang-cli", int64(3)},
		{"failures", "attribution_skill", "golang-cli", int64(0)},
		{"denied", "attribution_skill", "golang-cli", int64(3)},
	} {
		if got := metricValue(t, env, c.name, c.dim, c.key); got != c.want {
			t.Errorf("%s/%s/%s = %v, want %v", c.name, c.dim, c.key, got, c.want)
		}
	}
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "policy denials") {
		t.Errorf("a denial-fed pattern must be warned about: %v", env.Warnings)
	}
}

// TestFailuresAttributionRollup pins the join: attribution fields ride the
// assistant turn that made the call, not the result event, and every named
// field of that turn gets the error.
func TestFailuresAttributionRollup(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	attr := `"attributionSkill":"alpha","attributionPlugin":"desplega","attributionMcpServer":"codegraph",`
	dir := toolCorpus(t,
		useIn(tsAt(1), "s1", "t1", "mcp__codegraph__query", `{"q":"x"}`, attr),
		errRes(tsAt(2), "s1", "t1", ""),
		useIn(tsAt(3), "s1", "t2", "mcp__codegraph__query", `{"q":"y"}`, attr),
		errRes(tsAt(4), "s1", "t2", ""),
	)
	env, err := FailuresEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("FailuresEnvelope: %v", err)
	}
	for _, c := range []struct {
		dim, key string
		want     any
	}{
		{"attribution_skill", "alpha", int64(2)},
		{"attribution_plugin", "desplega", int64(2)},
		{"attribution_mcp_server", "codegraph", int64(2)},
	} {
		if got := metricValue(t, env, "errors", c.dim, c.key); got != c.want {
			t.Errorf("errors/%s/%s = %v, want %v", c.dim, c.key, got, c.want)
		}
	}
	// The contract this file leans on: the dimension names are the ones
	// `attribute` already emits — the JSON name is the contract.
	for _, d := range failuresDims {
		found := false
		for _, a := range attributionDims {
			if a.name == d.name {
				found = true
			}
		}
		if !found {
			t.Errorf("dimension %q is not in attributionDims — the name drifted", d.name)
		}
	}
}

// TestFailuresUnattributedMakesNoRow pins the absent rule: an errored call
// made on a turn with no attribution field produces no row in any attribution
// table, and the silence is named in a warning, not left to be inferred.
func TestFailuresUnattributedMakesNoRow(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := toolCorpus(t,
		useIn(tsAt(1), "s1", "t1", "Bash", cmdLS, ""), errRes(tsAt(2), "s1", "t1", ""),
		useIn(tsAt(3), "s2", "t2", "Bash", cmdLS, ""), errRes(tsAt(4), "s2", "t2", ""),
	)
	env, err := FailuresEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("FailuresEnvelope: %v", err)
	}
	for _, d := range failuresDims {
		if rows := dimRows(env, d.name); len(rows) != 0 {
			t.Errorf("%s has rows on an unattributed corpus: %+v", d.name, rows)
		}
	}
	w := strings.Join(env.Warnings, "\n")
	for _, field := range []string{"attributionSkill", "attributionPlugin", "attributionMcpServer"} {
		if !strings.Contains(w, field) {
			t.Errorf("the absent-field warning must name %s: %v", field, env.Warnings)
		}
	}
	// The pair itself still fires — attribution absence does not erase the
	// pattern, it only declines to blame a field.
	if got := metricValue(t, env, "errors", "repeated_failure", pairKey("Bash", cmdLS)); got != int64(2) {
		t.Errorf("repeated_failure errors = %v, want 2", got)
	}
}

// TestFailuresKeyOrderIsSamePayload pins the normalization: three retries of
// one semantic call whose JSON key order and whitespace differ are one loop
// under the canonical hash.
func TestFailuresKeyOrderIsSamePayload(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := toolCorpus(t,
		useIn(tsAt(1), "s1", "t1", "Bash", `{"command":"ls","timeout":30}`, ""),
		errRes(tsAt(2), "s1", "t1", ""),
		useIn(tsAt(3), "s1", "t2", "Bash", `{"timeout":30,"command":"ls"}`, ""),
		errRes(tsAt(4), "s1", "t2", ""),
		useIn(tsAt(5), "s1", "t3", "Bash", `{ "command" : "ls" , "timeout" : 30 }`, ""),
		errRes(tsAt(6), "s1", "t3", ""),
	)
	env, err := FailuresEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("FailuresEnvelope: %v", err)
	}
	key := loopKey("s1", "Bash", cmdLS2)
	if got := metricValue(t, env, "consecutive_errors", "retry_loop", key); got != int64(3) {
		t.Errorf("permuted identical payload = %v, want a 3-run under the canonical hash", got)
	}
}

func TestPayloadHash(t *testing.T) {
	h := func(s string) string { return payloadHash(json.RawMessage(s)) }
	cases := []struct {
		name  string
		a, b  string
		equal bool
	}{
		{"key order normalizes", `{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{"whitespace normalizes", `{"a": 1,"b" :2}`, `{"a":1, "b":2}`, true},
		{"nested order normalizes", `{"o":{"a":1,"b":2}}`, `{"o":{"b":2,"a":1}}`, true},
		{"different value differs", `{"a":1}`, `{"a":2}`, false},
		// UseNumber keeps 21-digit integers exact; float64 would merge these.
		{"big integers stay distinct", `{"n":123456789012345678901}`, `{"n":123456789012345678902}`, false},
		// 1.0 and 1 are the same number but different literals; merging them
		// would fire in the dangerous direction, so they stay distinct.
		{"number spelling stays distinct", `{"n":1.0}`, `{"n":1}`, false},
		{"broken input hashes its bytes", `{"a":`, `{"a":`, true},
		{"broken differs from valid", `{"a":`, `{}`, false},
		{"absent differs from empty object", ``, `{}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := h(c.a) == h(c.b); got != c.equal {
				t.Errorf("equal = %v, want %v (%q vs %q)", got, c.equal, c.a, c.b)
			}
			if len(h(c.a)) != hashLen {
				t.Errorf("hash %q is not %d chars", h(c.a), hashLen)
			}
		})
	}
}

// TestFailuresSortOrdersAreTotal pins the emission order two ways: loops by
// streak descending, attribution rows by failures before errors — a key whose
// only error was a denial must not outrank one that genuinely failed.
func TestFailuresSortOrdersAreTotal(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	lines := failingLoop("s1", "a", "Bash", cmdLS, "", 4)
	lines = append(lines, failingLoop("s2", "b", "Grep", cmdGrep, "", 3)...)
	lines = append(lines,
		useIn(tsAt(20), "s3", "d1", "Read", `{"path":"a"}`, `"attributionSkill":"only-denied",`),
		errRes(tsAt(21), "s3", "d1", "user-rejected"),
		useIn(tsAt(22), "s3", "d2", "Write", `{"path":"b"}`, `"attributionSkill":"one-failure",`),
		errRes(tsAt(23), "s3", "d2", ""),
	)
	dir := toolCorpus(t, lines...)
	env, err := FailuresEnvelope(dir, "test", Window{})
	if err != nil {
		t.Fatalf("FailuresEnvelope: %v", err)
	}

	var loops []string
	for _, m := range dimRows(env, "retry_loop") {
		if m.Name == "consecutive_errors" {
			loops = append(loops, m.Key)
		}
	}
	if len(loops) != 2 || !strings.HasPrefix(loops[0], "s1/Bash/") || !strings.HasPrefix(loops[1], "s2/Grep/") {
		t.Errorf("loop order = %v, want s1/Bash (4) before s2/Grep (3)", loops)
	}

	var skills []string
	for _, m := range dimRows(env, "attribution_skill") {
		if m.Name == "errors" {
			skills = append(skills, m.Key)
		}
	}
	if len(skills) != 2 || skills[0] != "one-failure" {
		t.Errorf("attribution_skill order = %v, want one-failure first — 1 failure outranks 1 denial", skills)
	}
}

// TestFailuresWindow gates rows the way corruption does: the loop is detected
// corpus-wide, but a row whose errored results all sit outside the window is
// omitted, never zeroed, while an in-window loop prints its full streak.
func TestFailuresWindow(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	july := make([]string, 0, 6)
	for i := range 3 {
		u := fmt.Sprintf("out%d", i)
		july = append(july,
			useIn("2026-07-0"+fmt.Sprint(i+1)+"T10:00:00.000Z", "s2", u, "Read", cmdPathZ, ""),
			errRes("2026-07-0"+fmt.Sprint(i+1)+"T10:00:01.000Z", "s2", u, ""))
	}
	dir := toolCorpus(t, append(failingLoop("s1", "a", "Bash", cmdLS, "", 3), july...)...)

	// An August window over a September and a July loop: both fully outside,
	// so the windowed rows are absent, not zero.
	w, err := NewWindow("2026-08-14", "2026-08-16")
	if err != nil {
		t.Fatal(err)
	}
	env, err := FailuresEnvelope(dir, "test", w)
	if err != nil {
		t.Fatalf("FailuresEnvelope: %v", err)
	}
	if got := metricOrNil(env, "consecutive_errors", "retry_loop", loopKey("s2", "Read", cmdPathZ)); got != nil {
		t.Errorf("July loop emitted %v under an August window — absent, not zero", got)
	}
	if got := metricOrNil(env, "consecutive_errors", "retry_loop", loopKey("s1", "Bash", cmdLS)); got != nil {
		t.Errorf("September loop emitted %v under an August window", got)
	}
	if got := metricOrNil(env, "retry_loops", "corpus", ""); got != nil {
		t.Errorf("retry_loops = %v under a window that matched nothing — must omit, not zero", got)
	}
	if got := metricValue(t, env, "unmatched_results", "corpus", ""); got != int64(0) {
		t.Errorf("unmatched_results = %v, want 0 — join audit stays corpus-wide", got)
	}

	// The matched-window path: only the September loop has in-window errors.
	w2, err := NewWindow("2026-09-01", "")
	if err != nil {
		t.Fatal(err)
	}
	env2, err := FailuresEnvelope(dir, "test", w2)
	if err != nil {
		t.Fatalf("FailuresEnvelope windowed: %v", err)
	}
	if got := metricValue(t, env2, "consecutive_errors", "retry_loop", loopKey("s1", "Bash", cmdLS)); got != int64(3) {
		t.Errorf("in-window loop streak = %v, want 3", got)
	}
	if got := metricOrNil(env2, "consecutive_errors", "retry_loop", loopKey("s2", "Read", cmdPathZ)); got != nil {
		t.Errorf("July loop emitted %v under a September window", got)
	}
}

// TestFailuresEnvelopeContractAndDeterminism is the two standing gates in one:
// every row measured, the command named, and four runs over an unchanged
// corpus producing byte-identical JSON — what TestReportReproducible guards
// for the merged report, pinned here for this envelope.
func TestFailuresEnvelopeContractAndDeterminism(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	lines := failingLoop("s1", "a", "Bash", cmdLS, "", 3)
	lines = append(lines, failingLoop("s2", "b", "Bash", cmdLS, "user-rejected", 3)...)
	lines = append(lines,
		useIn(tsAt(40), "s3", "x1", "Read", cmdLSDir, `"attributionSkill":"golang-naming",`),
		errRes(tsAt(41), "s3", "x1", ""),
		useIn(tsAt(42), "s4", "x2", "Read", cmdLSDir, ""),
		errRes(tsAt(43), "s4", "x2", ""),
	)
	dir := toolCorpus(t, lines...)

	var first []byte
	for run := range 4 {
		env, err := FailuresEnvelope(dir, "test", Window{})
		if err != nil {
			t.Fatalf("FailuresEnvelope: %v", err)
		}
		if env.Command != "failures" {
			t.Errorf("command = %q, want failures", env.Command)
		}
		for _, m := range env.Metrics {
			if m.Derivation != Measured {
				t.Errorf("metric %s/%s/%s is %s, not measured", m.Name, m.Dimension, m.Key, m.Derivation)
			}
		}
		if err := env.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		var buf bytes.Buffer
		if err := WriteJSON(&buf, env); err != nil {
			t.Fatalf("WriteJSON: %v", err)
		}
		if run == 0 {
			first = bytes.Clone(buf.Bytes())
			continue
		}
		if !bytes.Equal(first, buf.Bytes()) {
			t.Fatalf("run %d produced different JSON over an unchanged corpus", run)
		}
	}
}
