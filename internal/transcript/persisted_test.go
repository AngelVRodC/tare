package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// persistedEvent builds an externalised Bash result in the shape the corpus
// actually uses. The important detail is that `content` is a non-null
// placeholder string: the earlier detection rule keyed on a null `content` and
// found zero of the 39 real records.
func persistedEvent(t *testing.T, id, sideFile string, size int64, placeholder string) string {
	t.Helper()
	ev := map[string]any{
		"type":      "user",
		"sessionId": "s1",
		"message": map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": id,
				"content":     placeholder,
				"is_error":    false,
			}},
		},
		"toolUseResult": map[string]any{
			"stdout":              "",
			"stderr":              "",
			"interrupted":         false,
			"isImage":             false,
			"persistedOutputPath": sideFile,
			"persistedOutputSize": size,
			"noOutputExpected":    false,
		},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPersistedNotNull is the regression test for the corrected detection
// rule. `content` is a `<persisted-output>` placeholder string, never null, so
// the record must be found by a non-empty persistedOutputPath.
func TestPersistedNotNull(t *testing.T) {
	dir := t.TempDir()
	side := filepath.Join(dir, "tool-results", "br2ouksl2.txt")
	if err := os.MkdirAll(filepath.Dir(side), 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", 66382)
	if err := os.WriteFile(side, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	placeholder := "<persisted-output>\nOutput too large (64.8KB). Full output saved to: " +
		side + "\n\nPreview (first 2KB):\nsome preview text"

	corpus := writeCorpus(t, map[string]string{
		"a.jsonl": persistedEvent(t, "toolu_1", side, int64(len(body)), placeholder) + "\n",
	})

	var found *Persisted
	var result ToolResult
	if _, err := Scan(corpus, func(ev *Event) {
		if p := ev.Persisted(); p != nil {
			found = p
		}
		if _, results := ev.Blocks(); len(results) == 1 {
			result = results[0]
		}
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if found == nil {
		t.Fatal("externalised result not detected — detection must key on persistedOutputPath, not a null content")
	}
	if found.Path != side {
		t.Errorf("Path = %q, want %q", found.Path, side)
	}
	if found.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", found.Size, len(body))
	}
	if result.ContextBytes != int64(len(placeholder)) {
		t.Errorf("ContextBytes = %d, want the placeholder length %d", result.ContextBytes, len(placeholder))
	}

	// The measured baseline is 39 of 39 byte-exact, so persistedOutputSize
	// agreeing with the side file is part of the contract, not a coincidence.
	fi, err := os.Stat(found.Path)
	if err != nil {
		t.Fatalf("stat side file: %v", err)
	}
	if fi.Size() != found.Size {
		t.Errorf("on-disk size %d disagrees with persistedOutputSize %d", fi.Size(), found.Size)
	}
}

// TestPersistedAbsent covers the shapes toolUseResult takes on the rest of the
// corpus — a map without the field, a bare string, an array, and nothing at
// all. None of them is an externalised result and none may error.
func TestPersistedAbsent(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"a.jsonl": strings.Join([]string{
			`{"type":"user","toolUseResult":{"stdout":"hi","stderr":""}}`,
			`{"type":"user","toolUseResult":"a plain string result"}`,
			`{"type":"user","toolUseResult":[{"type":"text","text":"x"}]}`,
			`{"type":"user","toolUseResult":null}`,
			`{"type":"user"}`,
			`{"type":"user","toolUseResult":{"persistedOutputPath":"","persistedOutputSize":0}}`,
		}, "\n") + "\n",
	})
	stats, err := Scan(dir, func(ev *Event) {
		if p := ev.Persisted(); p != nil {
			t.Errorf("%s: detected an externalised result where there is none: %+v", ev.Type, p)
		}
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stats.ParseErrors != 0 {
		t.Errorf("ParseErrors = %d, want 0", stats.ParseErrors)
	}
}

// TestByteKinds is the phase's byte-definition gate: for an externalised
// result the produced bytes and the bytes that reached context diverge — that
// gap is the saving the harness performs and the product exists to measure —
// while an inline result has no side file and the two agree.
func TestByteKinds(t *testing.T) {
	dir := t.TempDir()
	side := filepath.Join(dir, "big.txt")
	body := strings.Repeat("y", 40000)
	if err := os.WriteFile(side, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	placeholder := "<persisted-output>\nOutput too large (39.1KB). Full output saved to: " + side

	corpus := writeCorpus(t, map[string]string{
		"a.jsonl": persistedEvent(t, "toolu_ext", side, int64(len(body)), placeholder) + "\n" +
			resultEvent("toolu_inline", `"a short inline result"`) + "\n",
	})

	type kinds struct{ context, produced int64 }
	got := map[string]kinds{}
	if _, err := Scan(corpus, func(ev *Event) {
		_, results := ev.Blocks()
		if len(results) != 1 {
			return
		}
		r := results[0]
		produced := r.ContextBytes // inline: what was produced is what arrived
		if p := ev.Persisted(); p != nil {
			produced = p.Size
		}
		got[r.ToolUseID] = kinds{context: r.ContextBytes, produced: produced}
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	ext := got["toolu_ext"]
	if ext.produced != int64(len(body)) {
		t.Errorf("externalised produced = %d, want %d", ext.produced, len(body))
	}
	if ext.context != int64(len(placeholder)) {
		t.Errorf("externalised context = %d, want the placeholder length %d", ext.context, len(placeholder))
	}
	if ext.produced <= ext.context {
		t.Errorf("externalised produced (%d) must exceed context (%d)", ext.produced, ext.context)
	}

	inline := got["toolu_inline"]
	if inline.context == 0 {
		t.Fatal("inline result measured zero bytes")
	}
	if inline.produced != inline.context {
		t.Errorf("inline produced (%d) and context (%d) must agree", inline.produced, inline.context)
	}
}
