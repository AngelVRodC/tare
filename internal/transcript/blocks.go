package transcript

import (
	"encoding/json"
	"strings"
)

// truncationMarkers are literal strings a *tool* writes when it cuts its own
// output before the model ever sees it.
//
// Claude Code itself does not truncate — past roughly 30 KB it externalises
// losslessly to a side file (see Persisted). So a marker inside a tool_result
// did not come from the harness: it came from the tool, and it is evidence
// that the agent reasoned over an altered answer. That is the whole signal.
//
// The set is kept literal and narrow on purpose. A looser pattern such as a
// bare "lines truncated" matches ordinary prose whenever a tool returns a file
// that happens to discuss truncation, and a false corruption finding is worse
// than a missed one.
var truncationMarkers = []string{
	"[truncated]",
	"... (truncated)",
	"(truncated; call ", // boost's boostgraph_explore / codegraph_explore suffix
	"<response clipped>",
	"[Output truncated",
	"[Result truncated",
}

// truncationMarker returns the first marker present in s, or "" for clean
// output. Matching is case-sensitive: these are emitted strings, not prose.
func truncationMarker(s string) string {
	for _, m := range truncationMarkers {
		if strings.Contains(s, m) {
			return m
		}
	}
	return ""
}

// ToolUse is a `tool_use` content block: the model asking for a tool.
type ToolUse struct {
	ID   string
	Name string
}

// ToolResult is a `tool_result` content block: the answer that came back.
//
// ContextBytes is the pinned byte definition — the decoded UTF-8 byte length
// of the *text* the model actually received. JSON-encoding length was the
// rejected alternative: measured on the corpus it runs 3.8% high overall and
// 12.0% high on the busiest MCP tool, which is escaping, not noise.
//
// ImageBytes is counted apart from it, never folded in. See contentBytes.
//
// TruncationMarker is the first truncation marker found in the result text, or
// "" for clean output. See truncationMarkers.
type ToolResult struct {
	ToolUseID        string
	ContextBytes     int64
	ImageBytes       int64
	IsError          bool
	TruncationMarker string
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
				ToolUseID:        b.ToolUseID,
				ContextBytes:     b.Content.Text,
				ImageBytes:       b.Content.Image,
				IsError:          b.IsError,
				TruncationMarker: b.Content.Marker,
			})
		}
	}
	return uses, results
}

// contentBytes measures a `content` field without retaining it. The field is a
// string in most results and an array of blocks in a minority (measured
// 12,763 string / 1,363 array), so both shapes reduce to byte counts here
// rather than at every call site.
//
// Text and image payloads are counted apart, on purpose. An `image` block —
// a screenshot, a PDF page — carries no `text` field at all, so a text-only
// sum reports *zero* for the most expensive thing a tool can put into the
// window. Measured on the corpus: 15 image blocks holding 2,348,764 base64
// bytes, every one of them silently zero under a text-only rule, which is
// what made the plan's `Read` baseline look unreproducible.
//
// They are not folded into ContextBytes because the two do not convert at the
// same rate: text is billed per token off its bytes, an image is billed by its
// dimensions (~w*h/750 tokens) regardless of how long its base64 happens to
// be. Phase 3 attributes tokens, so mixing them here would distort it.
//
// Marker is scanned here for the same reason the bytes are: this is the one
// place the text exists, and Phase 4 needs the finding, not the payload.
type contentBytes struct {
	Text   int64
	Image  int64
	Marker string
}

// UnmarshalJSON never fails: a shape outside string-or-array measures zero
// rather than poisoning the whole event. Unknown shape, quiet counter.
func (c *contentBytes) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		c.Text = int64(len(s)) // len of a Go string is its UTF-8 byte count
		c.Marker = truncationMarker(s)
		return nil
	}
	var blocks []struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Source struct {
			Data string `json:"data"`
		} `json:"source"`
	}
	if json.Unmarshal(b, &blocks) != nil {
		return nil
	}
	for _, blk := range blocks {
		c.Text += int64(len(blk.Text))
		if c.Marker == "" {
			c.Marker = truncationMarker(blk.Text)
		}
		// A url-sourced image carries no inline data and measures zero here;
		// its bytes never entered the transcript to be counted.
		if blk.Type == "image" {
			c.Image += int64(len(blk.Source.Data))
		}
	}
	return nil
}
