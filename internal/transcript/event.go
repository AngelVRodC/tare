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
	// ResumeKey is the Workflow tool's content hash for one agent dispatch. It
	// appears on a journal record and never on a transcript event, so it is
	// half of what tells the two apart. See IsWorkflowJournal.
	ResumeKey string `json:"key"`
	Version   string `json:"version"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`

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

// IsWorkflowJournal reports whether this line is a Workflow-tool journal
// record rather than a transcript event.
//
// Claude Code writes one journal per workflow run at
// `<session>/subagents/workflows/wf_*/journal.jsonl`, which is a sibling of
// the subagent transcripts and carries the resume cache, not a conversation.
// Measured on a 41-file corpus: 4 such files, 34 records, three `type` values
// — `started`, `result` and `failed`.
//
// The test is deliberately positive — what a journal record *is*, not what a
// transcript event is not. A bare "no sessionId" rule would silently swallow
// any future transcript type that happened to omit it.
//
// The definition this excludes against is already in AGENTS.md: a subagent
// transcript shares the parent sessionId with its own agentId and
// isSidechain: true. A journal record has none of the three.
//
// The `type` conjunct is the load-bearing one, because the two ways this can
// be wrong are not symmetric. `key` is a generic JSON name, so a rule without
// the type set rests on a no-collision claim about every field Claude Code
// might yet add, and a false positive there removes real events from every
// count in silence. A journal type missing from the set fails the other way:
// the record stays a transcript event, knownTypes does not recognise it, and
// `scan` reports it as an unknown type with a warning. A loud miss is
// recoverable and a silent exclusion is not, so the set is closed.
func (ev *Event) IsWorkflowJournal() bool {
	return ev.SessionID == "" && ev.AgentID != "" && ev.ResumeKey != "" && journalTypes[ev.Type]
}

// journalTypes are the record types a Workflow journal writes.
//
// Measured on a 41-file, 7-version corpus of 4,388 records: 34 carry a
// top-level `key` and every one of them is one of these three types. No record
// carrying a sessionId carried a `key` at all. That sample does not cover the
// 381-file corpus knownTypes rests on, which is why the type conjunct exists
// rather than the measurement being treated as sufficient on its own.
var journalTypes = map[string]bool{
	"started": true,
	"result":  true,
	"failed":  true,
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
