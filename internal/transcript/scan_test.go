package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCorpus lays out named .jsonl files in a temp dir and returns its path.
func writeCorpus(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestLongLine is the regression test for the bufio.Scanner rejection. The
// longest line measured on the real corpus is 514,199 bytes; this fixture is
// past 1 MB, so any reintroduced cap fails here instead of in production.
func TestLongLine(t *testing.T) {
	const payload = 1 << 20 // 1 MiB of content, before JSON escaping
	line, err := json.Marshal(map[string]any{
		"type":      "user",
		"sessionId": "s1",
		"cwd":       strings.Repeat("x", payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(line) <= payload {
		t.Fatalf("fixture line is %d bytes, want > %d", len(line), payload)
	}

	dir := writeCorpus(t, map[string]string{"long.jsonl": string(line) + "\n"})

	var got []Event
	stats, err := Scan(dir, func(ev *Event) { got = append(got, *ev) })
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stats.ParseErrors != 0 {
		t.Errorf("ParseErrors = %d, want 0", stats.ParseErrors)
	}
	if stats.Lines != 1 {
		t.Fatalf("Lines = %d, want 1", stats.Lines)
	}
	if len(got) != 1 {
		t.Fatalf("visited %d events, want 1", len(got))
	}
	if len(got[0].Cwd) != payload {
		t.Errorf("cwd truncated: got %d bytes, want %d", len(got[0].Cwd), payload)
	}
}

// TestUnknownType pins the "quietly on the unknown" half of the contract: an
// unrecognized top-level type is counted and still delivered, never fatal.
func TestUnknownType(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"a.jsonl": strings.Join([]string{
			`{"type":"user","sessionId":"s1"}`,
			`{"type":"quantum-flux","sessionId":"s1"}`,
			`{"type":"quantum-flux","sessionId":"s1"}`,
			``, // blank lines are skipped, not counted
		}, "\n") + "\n",
	})

	var seen []string
	stats, err := Scan(dir, func(ev *Event) { seen = append(seen, ev.Type) })
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stats.ParseErrors != 0 {
		t.Errorf("ParseErrors = %d, want 0 — an unknown type is not a parse error", stats.ParseErrors)
	}
	if stats.Lines != 3 {
		t.Errorf("Lines = %d, want 3", stats.Lines)
	}
	if got := stats.UnknownTypes["quantum-flux"]; got != 2 {
		t.Errorf("UnknownTypes[quantum-flux] = %d, want 2", got)
	}
	if len(stats.UnknownTypes) != 1 {
		t.Errorf("UnknownTypes = %v, want only the unknown one", stats.UnknownTypes)
	}
	if len(seen) != 3 {
		t.Errorf("visited %v, want all three events delivered", seen)
	}
}

// TestScanWalk covers the file-level bookkeeping the inventory reports on:
// .jsonl only, and the top-level vs subagents/ split.
func TestScanWalk(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"top.jsonl":                `{"type":"user","sessionId":"s1"}` + "\n",
		"subagents/kid.jsonl":      `{"type":"user","sessionId":"s1","isSidechain":true}` + "\n",
		"subagents/other.jsonl":    `{"type":"assistant","sessionId":"s1"}` + "\n",
		"notes.txt":                "ignored\n",
		"tool-results/out.jsonl.x": "ignored\n",
	})

	subagent := 0
	stats, err := Scan(dir, func(ev *Event) {
		if ev.InSubagentDir {
			subagent++
		}
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stats.Files != 3 {
		t.Errorf("Files = %d, want 3", stats.Files)
	}
	if stats.SubagentFiles != 2 {
		t.Errorf("SubagentFiles = %d, want 2", stats.SubagentFiles)
	}
	if subagent != 2 {
		t.Errorf("events flagged InSubagentDir = %d, want 2", subagent)
	}
}

// TestWorkflowJournalIsNotATranscript is the regression test for issue #3.
//
// Claude Code writes a Workflow-tool journal at
// <session>/subagents/workflows/wf_*/journal.jsonl — a sibling of the subagent
// transcripts, holding the resume cache rather than a conversation. Counting
// it as a transcript inflated four inventory rows and reported its `failed`
// record as an unrecognised transcript event type.
//
// Measured on a 41-file corpus: 4 journals, 34 records, and every occurrence
// of `started`, `result` and `failed` in the event-type table came from one.
func TestWorkflowJournalIsNotATranscript(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"proj/sess.jsonl": `{"type":"user","sessionId":"s1","timestamp":"2026-09-01T00:00:00.000Z"}` + "\n",
		"proj/sess/subagents/workflows/wf_x/journal.jsonl": `{"type":"started","key":"v2:abc","agentId":"a1"}` + "\n" +
			`{"type":"result","key":"v2:abc","agentId":"a1","result":{}}` + "\n" +
			`{"type":"failed","key":"v2:def","agentId":"a2"}` + "\n",
	})

	visited := 0
	stats, err := Scan(dir, func(*Event) { visited++ })
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stats.Files != 1 {
		t.Errorf("Files = %d, want 1 — a journal is not a transcript", stats.Files)
	}
	if stats.SubagentFiles != 0 {
		t.Errorf("SubagentFiles = %d, want 0 — a journal under subagents/ is not a subagent transcript", stats.SubagentFiles)
	}
	if stats.Lines != 1 {
		t.Errorf("Lines = %d, want 1 — the 3 journal records are not transcript events", stats.Lines)
	}
	if visited != 1 {
		t.Errorf("visited %d events, want 1 — a journal record must not reach a command", visited)
	}
	if len(stats.UnknownTypes) != 0 {
		t.Errorf("UnknownTypes = %v, want empty — those are journal record types", stats.UnknownTypes)
	}
	if stats.NonTranscriptFiles != 1 {
		t.Errorf("NonTranscriptFiles = %d, want 1 — the journal is reported, not dropped", stats.NonTranscriptFiles)
	}
	if stats.NonTranscriptRecords != 3 {
		t.Errorf("NonTranscriptRecords = %d, want 3", stats.NonTranscriptRecords)
	}
	// Bytes stay whole: the row they feed is labelled bytes on disk.
	if stats.Bytes == 0 {
		t.Error("Bytes = 0, want the journal's bytes still counted on disk")
	}
}

// TestJournalClassifierNeedsEveryConjunct is the false-positive gate on the
// classifier, and the reason the rule keys on `type` as well as on shape.
//
// `key` is a generic JSON name. A rule of no-sessionId + agentId + key alone
// would rest on a claim about every field Claude Code might yet add, and a
// false positive there drops real events from every count in silence. Each
// fixture here is one conjunct short of a journal record and must survive as a
// transcript event.
func TestJournalClassifierNeedsEveryConjunct(t *testing.T) {
	for name, line := range map[string]string{
		// The case the type conjunct exists for: journal shape, foreign type.
		"foreign type with key and agentId": `{"type":"checkpoint","key":"v2:abc","agentId":"a1"}`,
		"journal type but has a sessionId":  `{"type":"result","key":"v2:abc","agentId":"a1","sessionId":"s1"}`,
		"journal type but no agentId":       `{"type":"result","key":"v2:abc"}`,
		"journal type but no key":           `{"type":"result","agentId":"a1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeCorpus(t, map[string]string{"a.jsonl": line + "\n"})
			visited := 0
			stats, err := Scan(dir, func(*Event) { visited++ })
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if visited != 1 {
				t.Errorf("visited %d events, want 1 — this is not a journal record", visited)
			}
			if stats.Files != 1 || stats.Lines != 1 {
				t.Errorf("Files = %d, Lines = %d, want 1 and 1", stats.Files, stats.Lines)
			}
			if stats.NonTranscriptRecords != 0 {
				t.Errorf("NonTranscriptRecords = %d, want 0", stats.NonTranscriptRecords)
			}
		})
	}
}

// TestEmptyAndBrokenFilesStayTranscripts is the other half of the rule above:
// a file is reclassified only when it holds journal records AND no transcript
// line. An empty transcript and an undecodable one are findings, and quietly
// moving either out of the transcript count would hide them.
func TestEmptyAndBrokenFilesStayTranscripts(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"empty.jsonl":  "",
		"broken.jsonl": `{"type":` + "\n",
		// A journal record beside a real event: the file is still a transcript.
		"mixed.jsonl": `{"type":"user","sessionId":"s1"}` + "\n" +
			`{"type":"started","key":"v2:abc","agentId":"a1"}` + "\n",
	})
	stats, err := Scan(dir, func(*Event) {})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stats.Files != 3 {
		t.Errorf("Files = %d, want 3 — empty and broken files are transcripts", stats.Files)
	}
	if stats.NonTranscriptFiles != 0 {
		t.Errorf("NonTranscriptFiles = %d, want 0", stats.NonTranscriptFiles)
	}
	if stats.ParseErrors != 1 {
		t.Errorf("ParseErrors = %d, want 1 — a journal rule must not excuse a broken line", stats.ParseErrors)
	}
	if stats.NonTranscriptRecords != 1 {
		t.Errorf("NonTranscriptRecords = %d, want 1 — the record in mixed.jsonl is still excluded", stats.NonTranscriptRecords)
	}
}

// TestMalformedLineCounted pins the other half: a line json cannot decode is a
// counted defect, not a crash and not a silent drop.
func TestMalformedLineCounted(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"a.jsonl": `{"type":"user"}` + "\n" + `{"type":` + "\n",
	})
	visited := 0
	stats, err := Scan(dir, func(*Event) { visited++ })
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stats.ParseErrors != 1 {
		t.Errorf("ParseErrors = %d, want 1", stats.ParseErrors)
	}
	if visited != 1 {
		t.Errorf("visited %d events, want 1", visited)
	}
}
