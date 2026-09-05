package transcript

import "encoding/json"

// ToolUse is a `tool_use` content block: the model asking for a tool.
type ToolUse struct {
	ID   string
	Name string
}

// ToolResult is a `tool_result` content block: the answer that came back.
//
// ContextBytes is the pinned byte definition — the decoded UTF-8 byte length
// of the text the model actually received. JSON-encoding length was the
// rejected alternative: measured on the corpus it runs 3.8% high overall and
// 12.0% high on the busiest MCP tool, which is escaping, not noise.
type ToolResult struct {
	ToolUseID    string
	ContextBytes int64
	IsError      bool
}

// Blocks decodes the tool_use and tool_result blocks out of ev.Message.
//
// A message whose `content` is a plain string — most user turns — carries no
// tool blocks and yields nothing. That is the common case, not an error.
func (ev *Event) Blocks() (uses []ToolUse, results []ToolResult) {
	if len(ev.Message) == 0 {
		return nil, nil
	}
	var msg struct {
		Content []struct {
			Type      string       `json:"type"`
			ID        string       `json:"id"`
			Name      string       `json:"name"`
			ToolUseID string       `json:"tool_use_id"`
			Content   contentBytes `json:"content"`
			IsError   bool         `json:"is_error"`
		} `json:"content"`
	}
	if json.Unmarshal(ev.Message, &msg) != nil {
		return nil, nil
	}
	for _, b := range msg.Content {
		switch b.Type {
		case "tool_use":
			uses = append(uses, ToolUse{ID: b.ID, Name: b.Name})
		case "tool_result":
			results = append(results, ToolResult{
				ToolUseID:    b.ToolUseID,
				ContextBytes: int64(b.Content),
				IsError:      b.IsError,
			})
		}
	}
	return uses, results
}

// contentBytes measures a `content` field without retaining it. The field is a
// string in most results and an array of blocks in a minority (measured
// 12,763 string / 1,363 array), so both shapes reduce to one byte count here
// rather than at every call site. Array blocks that carry no text — the
// `tool_reference` and `image` blocks on the corpus — contribute nothing,
// because nothing textual is what they put into context.
type contentBytes int64

// UnmarshalJSON never fails: a shape outside string-or-array measures zero
// rather than poisoning the whole event. Unknown shape, quiet counter.
func (c *contentBytes) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*c = contentBytes(len(s)) // len of a Go string is its UTF-8 byte count
		return nil
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(b, &blocks) != nil {
		return nil
	}
	n := 0
	for _, blk := range blocks {
		n += len(blk.Text)
	}
	*c = contentBytes(n)
	return nil
}
