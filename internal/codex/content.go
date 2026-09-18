package codex

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Content measures the delivered text and inline base64 separately. The Known
// flags prevent partial/unknown payloads becoming measured zeroes or totals.
type Content struct {
	TextBytes, ImageBytes              int64
	HasImage                           bool
	TextKnown, ImageKnown, ImagesKnown bool
}

func Measure(raw json.RawMessage) Content {
	raw = bytes.TrimSpace(raw)
	c := Content{TextKnown: true, ImageKnown: true, ImagesKnown: true}
	var text string
	if len(raw) == 0 || string(raw) == "null" {
		return Content{}
	}
	if json.Unmarshal(raw, &text) == nil {
		c.TextBytes = int64(len(text))
		return c
	}
	var blocks []struct {
		Type     string  `json:"type"`
		Text     *string `json:"text"`
		ImageURL *string `json:"image_url"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return Content{}
	}
	for _, block := range blocks {
		switch block.Type {
		case "input_text":
			if block.Text == nil {
				c.TextKnown = false
			} else {
				c.TextBytes += int64(len(*block.Text))
			}
		case "input_image":
			c.HasImage = true
			if block.ImageURL == nil {
				c.ImageKnown = false
				continue
			}
			header, data, ok := strings.Cut(*block.ImageURL, ",")
			if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
				c.ImageKnown = false
			} else {
				c.ImageBytes += int64(len(data))
			}
		default:
			c.TextKnown, c.ImageKnown, c.ImagesKnown = false, false, false
		}
	}
	return c
}

// ToolName uses an escaped dot separator. Escaping dots and backslashes in
// BOTH components distinguishes namespace=a,name=b from name=a.b. Legacy
// names without these characters (including mcp__server__tool) stay verbatim.
func ToolName(namespace, name string) string {
	escape := func(s string) string {
		return strings.NewReplacer(`\`, `\\`, `.`, `\.`).Replace(s)
	}
	if namespace == "" {
		return escape(name)
	}
	return escape(namespace) + "." + escape(name)
}

// MCPServer accepts only the explicit namespaced or legacy MCP forms.
// Codex plugin ownership is not encoded using Claude Code's plugin convention.
func MCPServer(namespace, name string) string {
	if namespace != "" {
		if server, ok := strings.CutPrefix(namespace, "mcp__"); ok && server != "" {
			return server
		}
		return ""
	}
	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		server, tool, found := strings.Cut(rest, "__")
		if found && server != "" && tool != "" {
			return server
		}
	}
	return ""
}
