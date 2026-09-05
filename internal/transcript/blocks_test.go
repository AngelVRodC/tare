package transcript

import (
	"strings"
	"testing"
)

// resultEvent wraps a tool_result content payload in the event shape the
// corpus actually uses, so the fixtures exercise the real decode path.
func resultEvent(id, content string) string {
	return `{"type":"user","sessionId":"s1","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"` + id + `","content":` + content + `}]}}`
}

// TestContentShapes pins the pinned byte definition against both shapes the
// corpus contains: a string `content` (12,763 measured) and an array of blocks
// (1,363 measured). Array blocks with no text — `tool_reference` and `image`
// on the corpus — contribute nothing, because nothing textual is what they
// put into context.
func TestContentShapes(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    int64
	}{
		{"string", `"hello"`, 5},
		{"string is counted in utf-8 bytes, not runes", `"héllo·ñ"`, 10},
		{"array of text blocks sums their text", `[{"type":"text","text":"ab"},{"type":"text","text":"cde"}]`, 5},
		{"array text is counted in utf-8 bytes", `[{"type":"text","text":"héllo"}]`, 6},
		{"non-text array blocks contribute nothing",
			`[{"type":"text","text":"abc"},{"type":"tool_reference","name":"Read"},{"type":"image","source":{"data":"AAAA"}}]`, 3},
		{"empty array", `[]`, 0},
		{"null content is zero, not an error", `null`, 0},
		{"unknown shape is zero, not an error", `{"weird":true}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeCorpus(t, map[string]string{
				"a.jsonl": resultEvent("toolu_1", tc.content) + "\n",
			})
			var got []ToolResult
			stats, err := Scan(dir, func(ev *Event) {
				_, results := ev.Blocks()
				got = append(got, results...)
			})
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if stats.ParseErrors != 0 {
				t.Fatalf("ParseErrors = %d, want 0", stats.ParseErrors)
			}
			if len(got) != 1 {
				t.Fatalf("decoded %d results, want 1", len(got))
			}
			if got[0].ContextBytes != tc.want {
				t.Errorf("ContextBytes = %d, want %d", got[0].ContextBytes, tc.want)
			}
			if got[0].ToolUseID != "toolu_1" {
				t.Errorf("ToolUseID = %q, want toolu_1", got[0].ToolUseID)
			}
		})
	}
}

// TestBlocksIgnoresNonToolMessages covers the common case: a plain string
// `content` is most user turns and carries no tool blocks. That is not an
// error, and it must not become a parse error either.
func TestBlocksIgnoresNonToolMessages(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"a.jsonl": strings.Join([]string{
			`{"type":"user","message":{"role":"user","content":"just text"}}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"hi"}]}}`,
			`{"type":"cost-state","totalCostUSD":1.5}`,
		}, "\n") + "\n",
	})
	stats, err := Scan(dir, func(ev *Event) {
		uses, results := ev.Blocks()
		if len(uses) != 0 || len(results) != 0 {
			t.Errorf("%s yielded %d uses / %d results, want none", ev.Type, len(uses), len(results))
		}
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stats.ParseErrors != 0 {
		t.Errorf("ParseErrors = %d, want 0", stats.ParseErrors)
	}
}

// TestBlocksToolUse checks the other half of the join key.
func TestBlocksToolUse(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"a.jsonl": `{"type":"assistant","message":{"role":"assistant","content":[` +
			`{"type":"text","text":"running"},` +
			`{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]}}` + "\n",
	})
	var got []ToolUse
	if _, err := Scan(dir, func(ev *Event) {
		uses, _ := ev.Blocks()
		got = append(got, uses...)
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 || got[0].ID != "toolu_1" || got[0].Name != "Bash" {
		t.Fatalf("uses = %+v, want one {toolu_1 Bash}", got)
	}
}

// TestIsError pins the error flag the rollup counts on.
func TestIsError(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"a.jsonl": strings.Join([]string{
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"ok"}]}}`,
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b","content":"boom","is_error":true}]}}`,
		}, "\n") + "\n",
	})
	errs := 0
	if _, err := Scan(dir, func(ev *Event) {
		_, results := ev.Blocks()
		for _, r := range results {
			if r.IsError {
				errs++
			}
		}
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if errs != 1 {
		t.Errorf("errors = %d, want 1", errs)
	}
}
