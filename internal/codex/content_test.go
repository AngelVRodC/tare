package codex

import (
	"encoding/json"
	"testing"
)

func TestMeasureDeliveredContent(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      Content
	}{
		{"unicode", `"é\n🙂"`, Content{TextBytes: 7, TextKnown: true, ImageKnown: true, ImagesKnown: true}},
		{"JSON text remains text", `"{\"output\":\"hi\"}"`, Content{TextBytes: 15, TextKnown: true, ImageKnown: true, ImagesKnown: true}},
		{"blocks", `[{"type":"input_text","text":"é"},{"type":"input_image","image_url":"data:image/png;base64,aGk="},{"type":"input_text","text":"x"}]`, Content{TextBytes: 3, ImageBytes: 4, HasImage: true, TextKnown: true, ImageKnown: true, ImagesKnown: true}},
		{"remote image", `[{"type":"input_image","image_url":"https://example.invalid/image"}]`, Content{HasImage: true, TextKnown: true, ImagesKnown: true}},
		{"missing image", `[{"type":"input_image"}]`, Content{HasImage: true, TextKnown: true, ImagesKnown: true}},
		{"non-base64 image", `[{"type":"input_image","image_url":"data:image/svg+xml,abc"}]`, Content{HasImage: true, TextKnown: true, ImagesKnown: true}},
		{"missing text", `[{"type":"input_text"}]`, Content{ImageKnown: true, ImagesKnown: true}},
		{"unknown block", `[{"type":"audio","data":"abc"}]`, Content{}},
		{"unknown object", `{"text":"abc"}`, Content{}},
		{"null", ` null `, Content{}},
		{"absent", ``, Content{}},
		{"empty string", `""`, Content{TextKnown: true, ImageKnown: true, ImagesKnown: true}},
		{"empty array", `[]`, Content{TextKnown: true, ImageKnown: true, ImagesKnown: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Measure(json.RawMessage(tc.raw)); got != tc.want {
				t.Fatalf("Measure = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestToolIdentity(t *testing.T) {
	for _, tc := range []struct{ namespace, name, key, server string }{
		{"", "exec", "exec", ""},
		{"functions", "exec", "functions.exec", ""},
		{"", "functions.exec", `functions\.exec`, ""},
		{"a.b", `c\d`, `a\.b.c\\d`, ""},
		{"mcp__sample", "read", "mcp__sample.read", "sample"},
		{"", "mcp__sample__read__more", "mcp__sample__read__more", "sample"},
		{"other", "mcp__sample__read", "other.mcp__sample__read", ""},
		{"", "mcp____read", "mcp____read", ""},
	} {
		if got := ToolName(tc.namespace, tc.name); got != tc.key {
			t.Errorf("ToolName(%q, %q) = %q, want %q", tc.namespace, tc.name, got, tc.key)
		}
		if got := MCPServer(tc.namespace, tc.name); got != tc.server {
			t.Errorf("MCPServer(%q, %q) = %q, want %q", tc.namespace, tc.name, got, tc.server)
		}
	}
}
