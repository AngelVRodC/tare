package transcript

import (
	"encoding/json"
	"strings"
)

// Attachment is an `attachment` event's attribution, two levels deep.
//
// The top level is `attachment.type` — NOT `hookName`. Measured 2026-09-05,
// hookName is on 5,512 of 17,341 events and 32.9% of attachment bytes, so
// keying on it throws away two thirds of the largest single context cost in
// the corpus, and throws away exactly the rows this tool exists to un-redact:
// `skill_listing` (10.5 MB), `deferred_tools_delta` (6.6 MB) and
// `mcp_instructions_delta` (2.0 MB) carry no hookName at all. hookName is a
// sub-dimension of the `hook_*` types instead.
//
// The top-level `slug` is deliberately not a key. It looks like an identity
// field and is not: it is the *session* slug (`fluffy-dazzling-lightning`), so
// grouping by it yields a per-session table wearing a per-tool label. Checked
// and rejected 2026-09-05 — do not re-derive it.
type Attachment struct {
	Type string
	// KeyDimension names the second level, empty for a type that carries none.
	KeyDimension string
	// KeyBytes is each key's rendered byte cost inside this one event.
	KeyBytes map[string]int64
}

// untypedKey labels an attachment whose own `type` would not decode. Its
// bytes are still counted: an unreadable label is not a reason to lose the
// volume it stands for.
const untypedKey = "(untyped)"

// Sub-dimension names, one per attachment type that carries attribution keys.
const (
	DimHookName  = "hook_name"
	DimSkill     = "skill"
	DimMcpTool   = "mcp_tool"
	DimMcpServer = "mcp_server"
	DimAgent     = "agent"
)

// Attachment decodes the attribution off an `attachment` event, or returns nil
// for any other type.
//
// A hook event gets the whole line's bytes because it names exactly one hook;
// a listing event splits its bytes across the keys it names, measured off the
// rendered text each key actually contributed rather than shared out evenly.
func (ev *Event) Attachment() *Attachment {
	if ev.Type != "attachment" || len(ev.AttachmentRaw) == 0 {
		return nil
	}
	// Two decodes, on purpose. The head is the part every type spells the same
	// way; the keys are the parts that vary. `content` in particular is a
	// string on `skill_listing`, an array on `hook_additional_context` and an
	// object on `file` — decoding it as a string failed the whole event and
	// silently dropped four types and 3.3 MB of volume on the floor. A key
	// that will not decode costs its own attribution, never the event's bytes.
	var head struct {
		Type     string `json:"type"`
		HookName string `json:"hookName"`
	}
	_ = json.Unmarshal(ev.AttachmentRaw, &head)
	at := &Attachment{Type: head.Type}
	if at.Type == "" {
		at.Type = untypedKey
	}

	var keys struct {
		Names       []string        `json:"names"`
		Content     json.RawMessage `json:"content"`
		AddedNames  []string        `json:"addedNames"`
		AddedTypes  []string        `json:"addedTypes"`
		AddedLines  []string        `json:"addedLines"`
		AddedBlocks []string        `json:"addedBlocks"`
	}
	if json.Unmarshal(ev.AttachmentRaw, &keys) != nil {
		return at
	}

	switch {
	case head.HookName != "" && strings.HasPrefix(head.Type, "hook_"):
		at.KeyDimension = DimHookName
		at.KeyBytes = map[string]int64{head.HookName: ev.LineBytes}
	case head.Type == "skill_listing":
		at.KeyDimension = DimSkill
		at.KeyBytes = listingBytes(keys.Names, jsonString(keys.Content))
	case head.Type == "deferred_tools_delta":
		at.KeyDimension = DimMcpTool
		at.KeyBytes = pairedBytes(keys.AddedNames, keys.AddedLines)
	case head.Type == "mcp_instructions_delta":
		at.KeyDimension = DimMcpServer
		at.KeyBytes = pairedBytes(keys.AddedNames, keys.AddedBlocks)
	case head.Type == "agent_listing_delta":
		at.KeyDimension = DimAgent
		at.KeyBytes = pairedBytes(keys.AddedTypes, keys.AddedLines)
	}
	if len(at.KeyBytes) == 0 {
		at.KeyDimension = ""
	}
	return at
}

// jsonString reads a field that is a string on the types carrying a rendered
// listing and something else entirely on the types that do not.
func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// pairedBytes measures each key against the rendered text beside it. The two
// arrays come out of the same attachment written in the same order, so the
// index pairing is exact. A key with no counterpart falls back to its own
// length, which is what it cost.
func pairedBytes(keys, rendered []string) map[string]int64 {
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]int64, len(keys))
	for i, k := range keys {
		n := int64(len(k))
		if i < len(rendered) {
			n = int64(len(rendered[i]))
		}
		out[k] += n
	}
	return out
}

// listingBytes attributes each line of a rendered listing to the key it names.
//
// An index pairing would be wrong here: a description can carry its own
// newlines, and measured on the corpus `content` has two more lines than
// `names` on nearly every event. So a line opens a new entry only when it
// names a key the attachment itself declared, and continuation lines stay with
// the entry above them.
func listingBytes(names []string, content string) map[string]int64 {
	if len(names) == 0 || content == "" {
		return nil
	}
	declared := make(map[string]bool, len(names))
	for _, n := range names {
		declared[n] = true
	}

	out := make(map[string]int64, len(names))
	lines := strings.Split(content, "\n")
	current := ""
	for i, line := range lines {
		if rest, isEntry := strings.CutPrefix(line, "- "); isEntry {
			if k, _, found := strings.Cut(rest, ":"); found && declared[k] {
				current = k
			}
		}
		if current == "" {
			continue
		}
		out[current] += int64(len(line))
		if i < len(lines)-1 {
			out[current]++ // the newline that joined this line to the next
		}
	}
	return out
}
