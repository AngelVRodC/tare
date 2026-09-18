package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const codexTestTime = "2026-09-18T10:00:00.000Z"

func cxEvent(ts, kind string, payload any) string {
	b, err := json.Marshal(map[string]any{"timestamp": ts, "type": kind, "payload": payload})
	if err != nil {
		panic(err)
	}
	return string(b)
}
func cxMeta(id string) string {
	return cxEvent(codexTestTime, "session_meta", map[string]any{"id": id})
}
func cxCall(ts, id, namespace, name string, custom bool) string {
	kind := "function_call"
	if custom {
		kind = "custom_tool_call"
	}
	return cxEvent(ts, "response_item", map[string]any{"type": kind, "call_id": id, "namespace": namespace, "name": name, "arguments": ""})
}
func cxResult(ts, id string, output any, custom bool) string {
	kind := "function_call_output"
	if custom {
		kind = "custom_tool_call_output"
	}
	return cxEvent(ts, "response_item", map[string]any{"type": kind, "call_id": id, "output": output})
}
func cxEnvelope(t *testing.T, dir string, w Window) Envelope {
	t.Helper()
	e, err := CodexToolsEnvelope(dir, "test", w)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	return e
}
func cxWant(t *testing.T, e Envelope, dim, key, name string, want int64) {
	t.Helper()
	if got := asInt64(metricValue(t, e, name, dim, key)); got != want {
		t.Errorf("%s/%s/%s = %d, want %d", dim, key, name, got, want)
	}
}

func TestCodexToolsMeasuredResultsAndMirrors(t *testing.T) {
	dir := toolCorpus(t, cxMeta("parent"),
		cxCall(codexTestTime, "one", "mcp__sample", "read", false),
		cxResult(codexTestTime, "one", []any{
			map[string]any{"type": "input_text", "text": "é\n🙂"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aGk="},
		}, false),
		cxCall(codexTestTime, "two", "", "exec", true),
		cxResult(codexTestTime, "two", "abc", true),
		cxEvent(codexTestTime, "event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "McpToolCall", "id": "one", "result": "do not count again"}}),
		cxEvent(codexTestTime, "compacted", map[string]any{"replacement_history": []any{map[string]any{"type": "function_call_output", "call_id": "one", "output": "old history"}}}),
		cxCall(codexTestTime, "waiting", "", "pending", false),
		cxResult(codexTestTime, "orphan", "unattributed", false),
		cxEvent(codexTestTime, "response_item", map[string]any{"type": "web_search_call", "status": "completed"}),
		cxEvent(codexTestTime, "future_event", map[string]any{}),
	)
	e := cxEnvelope(t, dir, Window{})
	if e.Command != "tools" || e.SchemaVersion != SchemaVersion {
		t.Fatalf("wrong contract: %+v", e)
	}
	for name, want := range map[string]int64{"calls": 2, "context_bytes": 10, "image_bytes": 4, "image_results": 1, "tool_use_blocks": 3, "tool_result_blocks": 2, "unanswered_uses": 1, "unmatched_results": 1} {
		cxWant(t, e, "corpus", "", name, want)
	}
	cxWant(t, e, "tool", "mcp__sample.read", "context_bytes", 7)
	cxWant(t, e, "mcp_server", "sample", "context_bytes", 7)
	cxWant(t, e, "tool", "exec", "context_bytes", 3)
	for _, m := range e.Metrics {
		if m.Name == "errors" || m.Name == "produced_bytes" || m.Name == "rent_bytes" || strings.HasPrefix(m.Name, "externalised_") || m.Dimension == "plugin" {
			t.Errorf("unavailable metric emitted: %+v", m)
		}
	}
	warnings := strings.Join(e.Warnings, "\n")
	for _, want := range []string{"orphan", "orchestration", "direct calls only", "web_search_call", "future_event", "errors, produced_bytes", "rent_bytes"} {
		if !strings.Contains(warnings, want) {
			t.Errorf("missing warning %q", want)
		}
	}
	var table bytes.Buffer
	if err := RenderTools(&table, e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(table.String(), "mcp__sample.read") || !strings.Contains(table.String(), "7 B") {
		t.Fatalf("missing table row:\n%s", table.String())
	}
}

func TestCodexUnknownPayloadOmitsContainingTotals(t *testing.T) {
	dir := toolCorpus(t, cxMeta("s"),
		cxCall(codexTestTime, "a", "mcp__sample", "read", false), cxResult(codexTestTime, "a", "known", false),
		cxCall(codexTestTime, "b", "mcp__sample", "read", false), cxResult(codexTestTime, "b", map[string]any{"unknown": "shape"}, false),
		cxCall(codexTestTime, "c", "", "other", false), cxResult(codexTestTime, "c", "é", false),
	)
	e := cxEnvelope(t, dir, Window{})
	for _, location := range [][2]string{{"corpus", ""}, {"tool", "mcp__sample.read"}, {"mcp_server", "sample"}} {
		for _, metric := range []string{"context_bytes", "image_bytes"} {
			if got := metricOrNil(e, metric, location[0], location[1]); got != nil {
				t.Errorf("partial total %s/%v = %v", metric, location, got)
			}
		}
	}
	if metricOrNil(e, "image_results", "corpus", "") != nil {
		t.Error("unknown output cannot establish total image_results")
	}
	cxWant(t, e, "tool", "other", "context_bytes", 2)
	cxWant(t, e, "corpus", "", "calls", 3)
	var table bytes.Buffer
	if err := RenderTools(&table, e); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(table.String(), "%") {
		t.Error("unknown corpus denominator must not produce percentages")
	}
}

func TestCodexRemoteImagesPreserveTextAndImageCount(t *testing.T) {
	dir := toolCorpus(t, cxMeta("s"), cxCall(codexTestTime, "a", "", "image", false),
		cxResult(codexTestTime, "a", []any{map[string]any{"type": "input_image", "image_url": "https://example.invalid/image"}}, false))
	e := cxEnvelope(t, dir, Window{})
	cxWant(t, e, "corpus", "", "context_bytes", 0)
	cxWant(t, e, "corpus", "", "image_results", 1)
	if metricOrNil(e, "image_bytes", "corpus", "") != nil || metricOrNil(e, "image_bytes", "tool", "image") != nil {
		t.Fatal("remote image size is unavailable, not zero")
	}
}

func TestCodexSessionScopeDuplicatesAndCrossFileOrder(t *testing.T) {
	call := cxCall(codexTestTime, "same", "", "read", false)
	result := cxResult(codexTestTime, "same", "x", false)
	dir := toolCorpus(t, cxMeta("parent"), result, // result file sorts before its call file
		cxMeta("child"), call, cxResult(codexTestTime, "same", "yy", false))
	if err := os.WriteFile(filepath.Join(dir, "z.jsonl"), []byte(strings.Join([]string{cxMeta("parent"), call, result, call}, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	e := cxEnvelope(t, dir, Window{})
	cxWant(t, e, "corpus", "", "calls", 2)
	cxWant(t, e, "corpus", "", "context_bytes", 3)
	cxWant(t, e, "corpus", "", "unmatched_results", 0)
	if !strings.Contains(strings.Join(e.Warnings, "\n"), "2 identical") {
		t.Fatal("duplicate copies were not diagnosed")
	}
	var baseline bytes.Buffer
	if err := WriteJSON(&baseline, e); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		var next bytes.Buffer
		if err := WriteJSON(&next, cxEnvelope(t, dir, Window{})); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(baseline.Bytes(), next.Bytes()) {
			t.Fatal("Codex JSON is not reproducible")
		}
	}
}

func TestCodexConflictingIDsFailLoudly(t *testing.T) {
	for _, records := range [][]string{
		{cxCall(codexTestTime, "a", "", "one", false), cxCall(codexTestTime, "a", "", "two", false)},
		{cxResult(codexTestTime, "a", "one", false), cxResult(codexTestTime, "a", "two", false)},
		{cxCall(codexTestTime, "a", "", "one", false), cxResult(codexTestTime, "a", "one", true)},
	} {
		dir := toolCorpus(t, append([]string{cxMeta("s")}, records...)...)
		if _, err := CodexToolsEnvelope(dir, "test", Window{}); err == nil {
			t.Fatal("ambiguous call/result identity accepted")
		}
	}
}

func TestCodexForkHistoryOwnership(t *testing.T) {
	parent := cxMeta("parent")
	child := cxEvent(codexTestTime, "session_meta", map[string]any{"id": "child", "forked_from_id": "parent"})
	call := cxCall(codexTestTime, "a", "", "read", false)
	result := cxResult(codexTestTime, "a", "old", false)
	dir := toolCorpus(t, parent, call, result)
	// A fork copies parent-scoped records before switching to child metadata.
	copy := strings.Join([]string{parent, call, result, child,
		cxCall(codexTestTime, "b", "", "read", false), cxResult(codexTestTime, "b", "new", false)}, "\n")
	p := filepath.Join(dir, "child.jsonl")
	if err := os.WriteFile(p, []byte(copy), 0600); err != nil {
		t.Fatal(err)
	}
	e := cxEnvelope(t, dir, Window{})
	cxWant(t, e, "corpus", "", "calls", 2)
	cxWant(t, e, "corpus", "", "context_bytes", 6)
	// Reusing that ID under child ownership is ambiguous, not another call.
	if err := os.WriteFile(p, []byte(strings.Join([]string{child, call, result}, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CodexToolsEnvelope(dir, "test", Window{}); err == nil || !strings.Contains(err.Error(), "ambiguous inherited") {
		t.Fatalf("ambiguous inherited ID: %v", err)
	}
}

func TestCodexMalformedAndUnscopedRecordsAreVisible(t *testing.T) {
	dir := toolCorpus(t,
		cxCall(codexTestTime, "unscoped", "", "read", false),
		cxMeta("s"),
		cxEvent(codexTestTime, "response_item", map[string]any{"type": "function_call", "name": "missing-id"}),
		cxCall("invalid", "a", "", "read", false), cxResult("invalid", "a", "x", false),
		`{"type":"response_item","payload":`,
	)
	if err := os.WriteFile(filepath.Join(dir, "unrelated.jsonl"), []byte(`{"type":"assistant","message":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	e := cxEnvelope(t, dir, Window{})
	cxWant(t, e, "corpus", "", "calls", 1)
	for _, want := range []string{"1 malformed tool records", "1 records without session identity", "1 files without Codex metadata", "2 invalid timestamps", "2 lines failed to decode"} {
		if !strings.Contains(strings.Join(e.Warnings, "\n"), want) {
			t.Errorf("missing %q in %v", want, e.Warnings)
		}
	}
}

func TestCodexWindowJoinsPrecisionAndEmptyWindow(t *testing.T) {
	dir := toolCorpus(t, cxMeta("s"),
		cxCall("2026-09-17T00:00:00Z", "a", "", "read", false), cxResult("2026-09-18T11:00:00+01:00", "a", "abc", false),
		cxCall(codexTestTime, "b", "", "read", false), cxResult("2026-09-19T00:00:00Z", "b", "out", false),
		cxCall(codexTestTime, "c", "", "read", false), cxResult("2026-09-18T10:00:00.000000001Z", "c", "nano", false),
		cxCall(codexTestTime, "pending", "", "read", false),
		cxCall("", "no-time", "", "untimed", false), cxResult("", "no-time", "unknown time", false),
	)
	w, err := NewWindow(codexTestTime, codexTestTime)
	if err != nil {
		t.Fatal(err)
	}
	e := cxEnvelope(t, dir, w)
	cxWant(t, e, "corpus", "", "calls", 1)
	cxWant(t, e, "corpus", "", "context_bytes", 3)
	cxWant(t, e, "corpus", "", "tool_use_blocks", 3)
	cxWant(t, e, "corpus", "", "unanswered_uses", 1)
	cxWant(t, e, "corpus", "", "unmatched_results", 0)
	if !strings.Contains(strings.Join(e.Warnings, "\n"), "2 \"response_item\" events have no valid timestamp") {
		t.Fatal("partial timestamp coverage must be disclosed")
	}
	w, _ = NewWindow("2026-09-18", "2026-09-18")
	e = cxEnvelope(t, dir, w)
	cxWant(t, e, "corpus", "", "calls", 2)
	cxWant(t, e, "corpus", "", "context_bytes", 7)
	w, _ = NewWindow("2030-01-01", "")
	e = cxEnvelope(t, dir, w)
	if metricOrNil(e, "calls", "corpus", "") != nil || metricOrNil(e, "context_bytes", "corpus", "") != nil {
		t.Fatal("empty window emitted measured zeros")
	}
	if e.Corpus.Files != 1 || e.Corpus.Bytes == 0 {
		t.Fatal("window discarded corpus inventory")
	}
}
