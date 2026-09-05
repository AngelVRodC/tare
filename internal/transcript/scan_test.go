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
