package transcript

import (
	"fmt"
	"testing"
)

// usageEvent is one assistant line carrying a usage record. The same id may be
// written more than once, which is precisely what the corpus does.
func usageEvent(id, model string, input, output, cacheRead, cc5m, cc1h int64) string {
	return fmt.Sprintf(`{"type":"assistant","sessionId":"s1","timestamp":"2026-09-01T10:00:00.000Z",`+
		`"message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d,`+
		`"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,`+
		`"output_tokens_details":{"thinking_tokens":7},`+
		`"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d}}}}`,
		id, model, input, output, cacheRead, cc5m+cc1h, cc5m, cc1h)
}

// TestUsageDedup is the phase gate on the token count itself. The transcript
// writes the same API response back repeatedly — measured 2.55x inside the
// largest session — so a naive sum over assistant events is inflated by
// whatever the file happened to repeat. Dedup by message.id must collapse it,
// and the totals must not double.
func TestUsageDedup(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"a.jsonl": usageEvent("msg_1", "claude-opus-5", 100, 20, 5000, 40, 0) + "\n" +
			usageEvent("msg_1", "claude-opus-5", 100, 20, 5000, 40, 0) + "\n" +
			usageEvent("msg_1", "claude-opus-5", 100, 20, 5000, 40, 0) + "\n" +
			usageEvent("msg_2", "claude-opus-5", 10, 2, 500, 0, 8) + "\n",
	})

	set := NewUsageSet()
	var input, output, cacheRead, cc5m, cc1h, thinking int64
	var naive int
	if _, err := Scan(dir, func(ev *Event) {
		u := ev.Usage()
		if u == nil {
			return
		}
		naive++
		if !set.Add(u) {
			return
		}
		input += u.Input
		output += u.Output
		cacheRead += u.CacheRead
		cc5m += u.CacheCreate5m
		cc1h += u.CacheCreate1h
		thinking += u.Thinking
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if naive != 4 {
		t.Fatalf("usage-bearing events = %d, want 4", naive)
	}
	if set.Distinct != 2 || set.Duplicates != 2 {
		t.Errorf("distinct/duplicates = %d/%d, want 2/2", set.Distinct, set.Duplicates)
	}
	// Every total is the two distinct responses, never the four events.
	want := map[string][2]int64{
		"input":      {input, 110},
		"output":     {output, 22},
		"cache_read": {cacheRead, 5500},
		"cc5m":       {cc5m, 40},
		"cc1h":       {cc1h, 8},
		"thinking":   {thinking, 14},
	}
	for name, pair := range want {
		if pair[0] != pair[1] {
			t.Errorf("%s = %d, want %d — the repeated message.id was summed twice", name, pair[0], pair[1])
		}
	}
}

// TestUsageCacheCreationFallback pins the older shape: a response that reports
// only a cache-creation total, with no ephemeral split, must land in the 5m
// bucket rather than vanishing.
func TestUsageCacheCreationFallback(t *testing.T) {
	ev := Event{Type: "assistant", Message: []byte(
		`{"id":"msg_old","model":"claude-opus-5","usage":{"input_tokens":1,` +
			`"cache_creation_input_tokens":900}}`)}
	u := ev.Usage()
	if u == nil {
		t.Fatal("Usage() returned nil for a response that carries one")
	}
	if u.CacheCreate5m != 900 || u.CacheCreate1h != 0 {
		t.Errorf("cache creation = %d/%d, want 900/0", u.CacheCreate5m, u.CacheCreate1h)
	}
}

// TestUsageSkipsUnusable proves the two ways a response is not counted: a
// non-assistant event, and a response with no message.id to deduplicate by.
func TestUsageSkipsUnusable(t *testing.T) {
	cases := map[string]Event{
		"user event": {Type: "user", Message: []byte(`{"id":"msg_1","usage":{"input_tokens":5}}`)},
		"no usage":   {Type: "assistant", Message: []byte(`{"id":"msg_1"}`)},
		"no id":      {Type: "assistant", Message: []byte(`{"usage":{"input_tokens":5}}`)},
	}
	for name, ev := range cases {
		if u := ev.Usage(); u != nil {
			t.Errorf("%s: Usage() = %+v, want nil", name, u)
		}
	}
}

// TestCostState pins the billing record: tokens and money in the same place,
// per model, read off the event's own top-level fields.
func TestCostState(t *testing.T) {
	ev := Event{Type: "cost-state", SessionID: "s1", TotalCostUSD: 0.64,
		ModelUsage: []byte(`{"claude-opus-5":{"inputTokens":514,"outputTokens":2514,` +
			`"thinkingTokens":1583,"cacheReadInputTokens":366682,` +
			`"cacheCreationInputTokens":39188,"costUSD":0.6406}}`)}
	cs := ev.CostState()
	if cs == nil {
		t.Fatal("CostState() returned nil for a cost-state event")
	}
	mu, ok := cs.ModelUsage["claude-opus-5"]
	if !ok {
		t.Fatal("model missing from ModelUsage")
	}
	if mu.CostUSD != 0.6406 {
		t.Errorf("costUSD = %v, want 0.6406", mu.CostUSD)
	}
	// Thinking is inside Output already; adding it here would double-count.
	if want := int64(514 + 2514 + 366682 + 39188); mu.Tokens() != want {
		t.Errorf("Tokens() = %d, want %d", mu.Tokens(), want)
	}
	if (&Event{Type: "assistant"}).CostState() != nil {
		t.Error("CostState() decoded a non cost-state event")
	}
}
