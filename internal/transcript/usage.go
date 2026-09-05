package transcript

import "encoding/json"

// Usage is one API response's billed token counts, read off an `assistant`
// event's `message.usage`.
//
// The two ephemeral cache-creation tiers stay apart because they are priced
// differently; folding them would bake a wrong ratio into every cost estimate
// downstream. Thinking tokens are carried for reporting only — they are
// already inside Output, so adding them anywhere would double-count.
type Usage struct {
	MessageID string
	Model     string

	Input         int64
	Output        int64
	Thinking      int64
	CacheRead     int64
	CacheCreate5m int64
	CacheCreate1h int64
}

// Usage decodes the per-response usage off an assistant event, or returns nil
// when the event carries none. A response with no `message.id` is skipped:
// without one it cannot be deduplicated, and counting it would inflate the
// total by whatever the transcript happened to repeat.
func (ev *Event) Usage() *Usage {
	if ev.Type != "assistant" || len(ev.Message) == 0 {
		return nil
	}
	var msg struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreate   int64 `json:"cache_creation_input_tokens"`
			OutputDetails struct {
				Thinking int64 `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
			CacheCreation *struct {
				Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	}
	if json.Unmarshal(ev.Message, &msg) != nil || msg.Usage == nil || msg.ID == "" {
		return nil
	}

	u := &Usage{
		MessageID: msg.ID,
		Model:     msg.Model,
		Input:     msg.Usage.Input,
		Output:    msg.Usage.Output,
		Thinking:  msg.Usage.OutputDetails.Thinking,
		CacheRead: msg.Usage.CacheRead,
	}
	// The split is authoritative when it is there. A response that carries only
	// the total lands in the 5m bucket, which is the default tier — and the
	// cheaper of the two, so an unsplit response never inflates a cost estimate.
	if c := msg.Usage.CacheCreation; c != nil && c.Ephemeral5m+c.Ephemeral1h > 0 {
		u.CacheCreate5m, u.CacheCreate1h = c.Ephemeral5m, c.Ephemeral1h
	} else {
		u.CacheCreate5m = msg.Usage.CacheCreate
	}
	return u
}

// UsageSet deduplicates responses by `message.id`.
//
// The transcript writes the same API response back more than once: measured
// 2026-09-05, 26,768 usage-bearing assistant events against 11,611 distinct
// `message.id`, and 2.55x inflation inside the largest single session (1,775
// events, 697 ids). A repeated id is the same billed call written to the file
// again, not a second charge. `uuid` is unique per line and useless for this.
//
// The key is the bare id, not session+id: measured, zero ids span more than
// one session, so the simplest key is also the correct one.
type UsageSet struct {
	seen map[string]bool
	// Distinct and Duplicates are a corpus property worth reporting, not a
	// detail to swallow — the duplicate rate is what makes a naive sum wrong.
	Distinct   int
	Duplicates int
}

// NewUsageSet returns an empty set.
func NewUsageSet() *UsageSet { return &UsageSet{seen: map[string]bool{}} }

// Add reports whether u is the first sighting of its message.id. A false
// return is a duplicate: count it, never aggregate it.
func (s *UsageSet) Add(u *Usage) bool {
	if s.seen[u.MessageID] {
		s.Duplicates++
		return false
	}
	s.seen[u.MessageID] = true
	s.Distinct++
	return true
}

// ModelUsage is one model's billed totals inside a cost-state record.
type ModelUsage struct {
	Input       int64   `json:"inputTokens"`
	Output      int64   `json:"outputTokens"`
	Thinking    int64   `json:"thinkingTokens"`
	CacheRead   int64   `json:"cacheReadInputTokens"`
	CacheCreate int64   `json:"cacheCreationInputTokens"`
	CostUSD     float64 `json:"costUSD"`
}

// Tokens is the model's total billed token count, thinking excluded because
// it is already inside Output.
func (m ModelUsage) Tokens() int64 {
	return m.Input + m.Output + m.CacheRead + m.CacheCreate
}

// CostState is the per-session billing record, written once per transcript
// and carrying tokens and money in the same place.
//
// Measured 2026-09-05: only 57 of 134 top-level transcripts have one, so most
// sessions have tokens and no dollars — say so, never report zero. Do not
// substitute ~/.claude/stats-cache.json: every costUSD in it is zero.
type CostState struct {
	SessionID    string
	TotalCostUSD float64
	ModelUsage   map[string]ModelUsage
}

// CostState decodes the billing record off a `cost-state` event, or returns
// nil when the event is of another type.
func (ev *Event) CostState() *CostState {
	if ev.Type != "cost-state" || len(ev.ModelUsage) == 0 {
		return nil
	}
	var usage map[string]ModelUsage
	if json.Unmarshal(ev.ModelUsage, &usage) != nil {
		return nil
	}
	return &CostState{
		SessionID:    ev.SessionID,
		TotalCostUSD: ev.TotalCostUSD,
		ModelUsage:   usage,
	}
}
