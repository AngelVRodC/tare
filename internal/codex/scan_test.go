package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanStreamsLargeRecordsAndSwitchesSession(t *testing.T) {
	dir := t.TempDir()
	data := `{"type":"session_meta","payload":{"id":"parent"}}` + "\n" +
		`{"timestamp":"2026-09-18T01:02:03+01:00","type":"response_item","payload":{"type":"function_call","call_id":"1","name":"test","arguments":"` + strings.Repeat("x", 9<<20) + `"}}` + "\n" +
		`{"type":"session_meta","payload":{"id":"child","forked_from_id":"parent"}}` + "\n" +
		`{"type":"future_type","payload":{}}` + "\n" +
		`{"type":"response_item","payload":` // interrupted live tail
	if err := os.WriteFile(filepath.Join(dir, "rollout.jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var scopes []string
	stats, err := Scan(dir, func(ev Event) error {
		scopes = append(scopes, ev.Session)
		if ev.Type == "response_item" && ev.Timestamp != "2026-09-18T00:02:03.000000000Z" {
			t.Errorf("normalized timestamp = %q", ev.Timestamp)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(scopes, ",") != "parent,parent,child,child" || stats.Files != 1 || stats.Bytes != int64(len(data)) || stats.ParseErrors != 1 || stats.UnknownTypes["future_type"] != 1 {
		t.Fatalf("scopes=%v stats=%+v", scopes, stats)
	}
}

func TestScanRejectsWrongOrMissingCorpus(t *testing.T) {
	dir := t.TempDir()
	if _, err := Scan(filepath.Join(dir, "missing"), nil); err == nil || !strings.Contains(err.Error(), "codex corpus") {
		t.Fatalf("missing corpus: %v", err)
	}
	if _, err := Scan(dir, nil); err == nil || !strings.Contains(err.Error(), "no JSONL") {
		t.Fatalf("empty corpus: %v", err)
	}
	p := filepath.Join(dir, "claude.jsonl")
	if err := os.WriteFile(p, []byte(`{"type":"assistant","sessionId":"other","message":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(p, nil); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("file as root: %v", err)
	}
	if _, err := Scan(dir, nil); err == nil || !strings.Contains(err.Error(), "no Codex session_meta") {
		t.Fatalf("wrong corpus: %v", err)
	}
}

func TestScanMalformedIdentityDoesNotInheritPreviousSession(t *testing.T) {
	dir := t.TempDir()
	data := `{"type":"session_meta","payload":{"id":"a"}}
{"type":"session_meta","payload":{"id":123}}
{"type":"response_item","payload":{"type":"function_call","call_id":"x","name":"test"}}
{"type":"session_meta","payload":{"id":"b"}}
{"timestamp":"invalid","type":"response_item","payload":{"type":"message"}}`
	if err := os.WriteFile(filepath.Join(dir, "rollout.jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var events int
	stats, err := Scan(dir, func(ev Event) error { events++; return nil })
	if err != nil || events != 3 || stats.UnscopedRecords != 2 || stats.InvalidTimes != 1 || stats.ParseErrors != 1 {
		t.Fatalf("events=%d stats=%+v err=%v", events, stats, err)
	}
}
