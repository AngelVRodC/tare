package report

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AngelVRodC/tare/internal/transcript"
)

// reportCorpus writes a corpus whose dollar rows are each a sum of many
// terms: eight skills used across seven separately-billed sessions, so every
// `cost_usd` row adds seven shares together and the corpus total adds seven
// session bills. Token counts and bills are chosen so no share is an exact
// binary fraction — added in a different order they land on a different last
// bit. One skill in one session would sum a single term and prove nothing.
func reportCorpus(t *testing.T) string {
	t.Helper()
	const model = "claude-opus-5"

	var lines []string
	for s := range 7 {
		session := fmt.Sprintf("s%d", s)
		bill := 0.7 + 0.13*float64(s)
		var total transcript.ModelUsage
		for i := range 8 {
			tk := tokens{
				in:   int64(101 + 37*i + 7*s),
				out:  int64(23 + 11*i + 3*s),
				read: int64(3_001 + 997*i + 131*s),
				c5m:  int64(701 + 313*i + 29*s),
			}
			lines = append(lines, usageLine(t, session, fmt.Sprintf("msg_%d_%d", s, i), model, tk,
				map[string]string{"attributionSkill": fmt.Sprintf("skill-%d", i)}))
			total.Input += tk.in
			total.Output += tk.out
			total.CacheRead += tk.read
			total.CacheCreate += tk.c5m + tk.c1h
		}
		total.CostUSD = bill
		lines = append(lines, costLine(t, session, bill,
			map[string]transcript.ModelUsage{model: total}))
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestReportReproducible is the phase gate and the product's whole claim: a
// skeptic re-running tare over the same bytes must get the same artifact.
//
// It caught a real defect. Allocation summed float shares while ranging over a
// map, and Go randomises map iteration — two runs over an unchanged corpus
// disagreed in the last bit of every dollar figure. The Markdown hid it behind
// six-significant-digit formatting; the JSON did not.
func TestReportReproducible(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no boost, no sqlite3: the corpus is the only input
	dir := reportCorpus(t)

	var first []byte
	for run := range 8 {
		rep, err := BuildReport(dir, "test")
		if err != nil {
			t.Fatalf("BuildReport: %v", err)
		}
		var buf bytes.Buffer
		if err := WriteJSON(&buf, rep.Envelope); err != nil {
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

// TestReportMarkdownVariesOnlyByTimestamp is the same claim for the human half:
// two renders of one report differ in the generated line and nowhere else.
func TestReportMarkdownVariesOnlyByTimestamp(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	rep, err := BuildReport(reportCorpus(t), "test")
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	render := func(at time.Time) []string {
		var buf bytes.Buffer
		if err := RenderReport(&buf, rep, at); err != nil {
			t.Fatalf("RenderReport: %v", err)
		}
		return strings.Split(buf.String(), "\n")
	}
	a := render(time.Unix(1_700_000_000, 0).UTC())
	b := render(time.Unix(1_800_000_000, 0).UTC())

	if len(a) != len(b) {
		t.Fatalf("renders differ in line count: %d vs %d", len(a), len(b))
	}
	var differing []int
	for i := range a {
		if a[i] != b[i] {
			differing = append(differing, i)
		}
	}
	if len(differing) != 1 {
		t.Fatalf("renders differ on %d lines, want exactly 1 (the timestamp): %v", len(differing), differing)
	}
	if !strings.HasPrefix(a[differing[0]], "| generated |") {
		t.Errorf("the differing line is %q, want the generated row", a[differing[0]])
	}
}

// TestReportHeaderIsSelfContained checks the header carries what a third party
// needs to re-run it. A number with no stated provenance is the failure mode
// this whole command exists to avoid.
func TestReportHeaderIsSelfContained(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := reportCorpus(t)
	rep, err := BuildReport(dir, "test")
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	var buf bytes.Buffer
	if err := RenderReport(&buf, rep, time.Now()); err != nil {
		t.Fatalf("RenderReport: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"| tool | tare test |",
		"| corpus | `" + dir + "` |",
		"| files | 1 |",
		"| date range |",
		"| claude code versions |",
		"| reproduce | `tare report --dir " + dir + "` |",
		"## Derivations",
		"| cost_usd | estimated | " + AllocationMethod + " |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q", want)
		}
	}
	// Every section is the verbatim output of the command that produced it.
	for _, cmd := range []string{"scan", "tools", "attribute", "corruption"} {
		if !strings.Contains(out, "— `tare "+cmd+"`") {
			t.Errorf("report has no %s section", cmd)
		}
	}
}

// TestReportSurfacesDisagreement is the honesty gate on the merge. Two
// commands measuring the same thing must agree; when they do not, the artifact
// says so rather than quietly reporting whichever ran first.
func TestReportSurfacesDisagreement(t *testing.T) {
	a := Envelope{Command: "tools", Metrics: []Metric{
		MeasuredMetric("calls", "tool", "Bash", int64(10), "calls"),
	}}
	b := Envelope{Command: "corruption", Metrics: []Metric{
		MeasuredMetric("calls", "tool", "Bash", int64(11), "calls"),
		MeasuredMetric("errors", "tool", "Bash", int64(2), "errors"),
	}}

	env := merge("/corpus", "test", []Envelope{a, b})
	if got := len(env.Metrics); got != 2 {
		t.Fatalf("merged %d rows, want 2 — the duplicate must collapse", got)
	}
	if got := mustMetric(t, env, "calls", "tool", "Bash").Value; got != int64(10) {
		t.Errorf("kept %v, want the first command's 10", got)
	}
	joined := strings.Join(env.Warnings, "\n")
	if !strings.Contains(joined, "disagrees") || !strings.Contains(joined, "tools") ||
		!strings.Contains(joined, "corruption") {
		t.Errorf("a disagreement must be warned about, naming both commands: %v", env.Warnings)
	}
}

// TestReportWarningsNameTheirCommand keeps a composed warning traceable: a
// reader has to know which of the four raised it.
func TestReportWarningsNameTheirCommand(t *testing.T) {
	env := merge("/corpus", "test", []Envelope{
		{Command: "scan", Warnings: []string{"unknown event type"}},
		{Command: "attribute"},
	})
	if len(env.Warnings) != 1 || !strings.HasPrefix(env.Warnings[0], "scan: ") {
		t.Errorf("warnings = %v, want one prefixed with its command", env.Warnings)
	}
}
