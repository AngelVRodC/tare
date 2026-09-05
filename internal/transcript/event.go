// Package transcript parses Claude Code transcript files (`*.jsonl`) into a
// tolerant, normalized event model. It streams; it never loads a corpus.
package transcript

import "encoding/json"

// Event is one transcript line, decoded tolerantly.
//
// The corpus spans many CLI versions and 21 top-level `type` values with heavy
// optional-field variance, so this struct sniffs shape rather than trusting a
// version field: only the fields actually needed are named, everything else is
// dropped by encoding/json for free, and the two variable regions stay as
// json.RawMessage to be decoded lazily by later phases.
type Event struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	ParentUUID  string `json:"parentUuid"`
	SessionID   string `json:"sessionId"`
	AgentID     string `json:"agentId"`
	IsSidechain bool   `json:"isSidechain"`
	Version     string `json:"version"`
	Timestamp   string `json:"timestamp"`
	Cwd         string `json:"cwd"`
	GitBranch   string `json:"gitBranch"`

	AttributionSkill     string `json:"attributionSkill"`
	AttributionPlugin    string `json:"attributionPlugin"`
	AttributionAgent     string `json:"attributionAgent"`
	AttributionMcpServer string `json:"attributionMcpServer"`
	AttributionMcpTool   string `json:"attributionMcpTool"`

	// Message, ToolUseResult and AttachmentRaw vary in shape across event
	// types. Phases 2-3 decode them; Phase 1 only carries them. AttachmentRaw
	// wears the suffix because Attachment is the decoded type it yields.
	Message       json.RawMessage `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	AttachmentRaw json.RawMessage `json:"attachment"`

	// TotalCostUSD and ModelUsage are the `cost-state` billing record. It is
	// written once per session and these are zero on every other event.
	TotalCostUSD float64         `json:"totalCostUSD"`
	ModelUsage   json.RawMessage `json:"modelUsage"`

	// File is the transcript this event was read from. Not a transcript field.
	File string `json:"-"`
	// LineBytes is the length of the JSONL line this event was decoded from,
	// newline excluded. It is how attachment volume is measured — the cost of
	// an attachment is the line it occupies. Not a transcript field.
	LineBytes int64 `json:"-"`
	// InSubagentDir reports whether File sits under a `subagents/` directory.
	// One file is not one session: subagent transcripts share the parent
	// sessionId with their own agentId. Not a transcript field.
	InSubagentDir bool `json:"-"`
}

// knownTypes is the set of top-level `type` values measured on the corpus on
// 2026-09-05 (21 values, 381 files). Anything outside it is counted as unknown
// and never treated as an error — the corpus is live and Claude Code adds
// types without warning.
var knownTypes = map[string]bool{
	"agent-name":                true,
	"agent-setting":             true,
	"ai-title":                  true,
	"artifact-autoreact-ledger": true,
	"artifact-comment-monitor":  true,
	"assistant":                 true,
	"atis-latch":                true,
	"attachment":                true,
	"cost-state":                true,
	"file-history-delta":        true,
	"file-history-snapshot":     true,
	"frame-link":                true,
	"last-prompt":               true,
	"mode":                      true,
	"permission-mode":           true,
	"pr-link":                   true,
	"queue-operation":           true,
	"result":                    true,
	"started":                   true,
	"system":                    true,
	"user":                      true,
}

// KnownTypeCount is how many top-level event types this build recognizes.
func KnownTypeCount() int { return len(knownTypes) }
