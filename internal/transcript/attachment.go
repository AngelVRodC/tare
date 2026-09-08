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
	// DimInstructionFile keys the `instructions` type by files[].path. It is
	// an attachment sub-dimension, deliberately named apart from every
	// mechanism-1 attribution dimension and from session — a collision would
	// file rendered-file bytes under the wrong table.
	DimInstructionFile = "instruction_file"
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
		Files       []struct {
			Path    string  `json:"path"`
			Content *string `json:"content"`
		} `json:"files"`
		Entries []json.RawMessage `json:"entries"`
	}
	if json.Unmarshal(ev.AttachmentRaw, &keys) != nil {
		return at
	}

	switch {
	case head.HookName != "" && strings.HasPrefix(head.Type, "hook_"):
		at.KeyDimension = DimHookName
		at.KeyBytes = map[string]int64{head.HookName: ev.PayloadBytes()}
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
	case head.Type == "instructions":
		at.KeyDimension = DimInstructionFile
		names := make([]string, 0, len(keys.Files))
		rendered := make([]string, 0, len(keys.Files))
		for _, f := range keys.Files {
			names = append(names, f.Path)
			// No content field means the rendered text never existed: the
			// path's own length is what it cost. An empty content that IS
			// present renders as zero, as it should.
			if f.Content != nil {
				rendered = append(rendered, *f.Content)
			} else {
				rendered = append(rendered, f.Path)
			}
		}
		at.KeyBytes = pairedBytes(names, rendered)
	case head.Type == "deferred_tools_record":
		// The record is the delta's sibling: same key, same dimension, but it
		// ships whole entries rather than name/line pairs, so the entry's own
		// raw JSON is what each name cost.
		at.KeyDimension = DimMcpTool
		at.KeyBytes = entryBytes(keys.Entries)
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

// entryBytes keys each entry's raw JSON length by its name. An entry with no
// name has nothing to attach its bytes to, so it costs its own attribution
// only — never the event's bytes, which the type row already carries.
func entryBytes(entries []json.RawMessage) map[string]int64 {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]int64, len(entries))
	for _, raw := range entries {
		var e struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &e) != nil || e.Name == "" {
			continue
		}
		out[e.Name] += int64(len(raw))
	}
	return out
}

// listingBytes attributes each line of a rendered listing to the key it names.
//
// An index pairing would be wrong here: a description can carry its own
// newlines, and measured on the corpus `content` has two more lines than
// `names` on nearly every event. So a line opens a new entry only when it
// names a key the attachment itself declared — the text after "- " beginning
// with name+":" — and continuation lines stay with the entry above them.
//
// Cutting at the first colon would be wrong: declared names themselves contain
// colons (89% of corpus listings carry plugin-namespaced skills), and cutting
// at the last would be wrong too, because descriptions contain colons. No cut
// position works; matching the prefix against the declared set does, with the
// longest declared name winning when one name prefixes another.
func listingBytes(names []string, content string) map[string]int64 {
	if len(names) == 0 || content == "" {
		return nil
	}
	declared := make(map[string]bool, len(names))
	longest := 0
	for _, n := range names {
		declared[n] = true
		if len(n) > longest {
			longest = len(n)
		}
	}

	out := make(map[string]int64, len(names))
	lines := strings.Split(content, "\n")
	current := ""
	for i, line := range lines {
		if rest, isEntry := strings.CutPrefix(line, "- "); isEntry {
			if k := matchDeclared(rest, declared, longest); k != "" {
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

// matchDeclared returns the longest declared name that prefixes rest followed
// by a colon, or "" when none does. Candidates are cut at colon positions
// rather than the first colon because declared names themselves carry colons;
// prefixes only grow, so the last matching candidate is the longest.
func matchDeclared(rest string, declared map[string]bool, longest int) string {
	if len(rest) > longest+1 {
		rest = rest[:longest+1] // the longest name plus its colon
	}
	match := ""
	for i := 0; i < len(rest); i++ {
		if rest[i] == ':' && declared[rest[:i]] {
			match = rest[:i]
		}
	}
	return match
}
