package transcript

import "testing"

// TestAttachmentTwoLevel pins the corrected dimension. The top level is
// `attachment.type`; `hookName` is a sub-dimension of the hook_* types only.
// Keying the top level on hookName would discard `skill_listing`,
// `deferred_tools_delta` and `mcp_instructions_delta` — 31.9% of attachment
// bytes and the un-redacted names the product exists to report.
func TestAttachmentTwoLevel(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantTyp string
		wantDim string
		wantKey map[string]int64
	}{{
		// The hook's bytes are the attachment object's own raw JSON (66 bytes),
		// never the JSONL record line: LineBytes is set to 999 on purpose, so
		// any figure that survives from the line is envelope, not payload.
		name:    "hook_success keys on hookName and measures the payload",
		payload: `{"type":"hook_success","hookName":"boost-awareness","command":"x"}`,
		wantTyp: "hook_success",
		wantDim: DimHookName,
		wantKey: map[string]int64{"boost-awareness": 66},
	}, {
		name: "skill_listing keys on names, measured off the rendered line",
		payload: `{"type":"skill_listing","skillCount":2,"names":["alpha","beta"],` +
			`"content":"- alpha: does a thing\n- beta: does another"}`,
		wantTyp: "skill_listing",
		wantDim: DimSkill,
		wantKey: map[string]int64{"alpha": 22, "beta": 20},
	}, {
		name: "deferred_tools_delta keys on addedNames",
		payload: `{"type":"deferred_tools_delta","addedNames":["Monitor","WebFetch"],` +
			`"addedLines":["Monitor","WebFetch--"]}`,
		wantTyp: "deferred_tools_delta",
		wantDim: DimMcpTool,
		wantKey: map[string]int64{"Monitor": 7, "WebFetch": 10},
	}, {
		name:    "mcp_instructions_delta keys on addedNames, sized by its block",
		payload: `{"type":"mcp_instructions_delta","addedNames":["context7"],"addedBlocks":["1234567890"]}`,
		wantTyp: "mcp_instructions_delta",
		wantDim: DimMcpServer,
		wantKey: map[string]int64{"context7": 10},
	}, {
		name:    "a type with no attribution key still reports its type",
		payload: `{"type":"output_style","style":"Gentleman"}`,
		wantTyp: "output_style",
		wantDim: "",
		wantKey: nil,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := Event{Type: "attachment", LineBytes: 999, AttachmentRaw: []byte(tc.payload)}
			at := ev.Attachment()
			if at == nil {
				t.Fatal("Attachment() returned nil for an attachment event")
			}
			if at.Type != tc.wantTyp {
				t.Errorf("Type = %q, want %q", at.Type, tc.wantTyp)
			}
			if at.KeyDimension != tc.wantDim {
				t.Errorf("KeyDimension = %q, want %q", at.KeyDimension, tc.wantDim)
			}
			if len(at.KeyBytes) != len(tc.wantKey) {
				t.Fatalf("KeyBytes = %v, want %v", at.KeyBytes, tc.wantKey)
			}
			for k, want := range tc.wantKey {
				if at.KeyBytes[k] != want {
					t.Errorf("KeyBytes[%q] = %d, want %d", k, at.KeyBytes[k], want)
				}
			}
		})
	}

	if (&Event{Type: "user"}).Attachment() != nil {
		t.Error("Attachment() decoded a non-attachment event")
	}
}

// TestAttachmentTolerantContent is the regression test for a measured
// under-count. `content` is a string on skill_listing, an array on
// hook_additional_context and an object on `file`; decoding it as a string
// failed the whole event, so four attachment types and 3.3 MB of volume left
// the report silently. A field that will not decode may cost its own
// attribution — never the event's bytes.
func TestAttachmentTolerantContent(t *testing.T) {
	cases := map[string]string{
		"array content":  `{"type":"hook_additional_context","hookName":"h","content":[{"type":"text"}]}`,
		"object content": `{"type":"file","filename":"x.go","content":{"text":"…"}}`,
		"number content": `{"type":"skill_listing","names":["alpha"],"content":42}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			ev := Event{Type: "attachment", LineBytes: 500, AttachmentRaw: []byte(payload)}
			at := ev.Attachment()
			if at == nil {
				t.Fatal("the event was dropped — its bytes left the report")
			}
			if at.Type == "" || at.Type == untypedKey {
				t.Errorf("Type = %q, want the type the payload declares", at.Type)
			}
		})
	}

	// An attachment whose own type will not decode still carries its bytes.
	ev := Event{Type: "attachment", LineBytes: 500, AttachmentRaw: []byte(`{"type":[1,2]}`)}
	if at := ev.Attachment(); at == nil || at.Type != untypedKey {
		t.Errorf("undecodable type yielded %+v, want the %q bucket", at, untypedKey)
	}
}

// TestListingBytesLongestPrefix is the defect-4 gate. A bullet opens an entry
// only when the text after "- " begins with a declared name followed by ":",
// and the LONGEST declared name wins — "desplega" and "desplega:brainstorm"
// both prefix the third bullet, the longer one owns it. The description after
// a second colon belongs to the entry. A bullet matching nothing declared
// continues the open entry (its bytes stay with sdd-verify) and never opens
// one of its own; the preamble before any match lands on no key at all.
func TestListingBytesLongestPrefix(t *testing.T) {
	ev := Event{Type: "attachment", LineBytes: 1, AttachmentRaw: []byte(
		`{"type":"skill_listing","names":["commit","desplega","desplega:brainstorm","sdd-verify"],` +
			`"content":"- note: preamble that matches nothing\n` +
			`- commit: plain skill\n` +
			`- desplega:brainstorm: does X: with colons\n` +
			`- sdd-verify: runs verification\n` +
			`- unmatched: detail continuing sdd-verify"}`)}
	at := ev.Attachment()
	if at == nil {
		t.Fatal("Attachment() returned nil")
	}
	if at.KeyDimension != DimSkill {
		t.Errorf("KeyDimension = %q, want %q", at.KeyDimension, DimSkill)
	}
	want := map[string]int64{
		"commit":              22,
		"desplega:brainstorm": 43,
		"sdd-verify":          73,
	}
	if len(at.KeyBytes) != len(want) {
		t.Fatalf("KeyBytes = %v, want %v", at.KeyBytes, want)
	}
	for k, n := range want {
		if at.KeyBytes[k] != n {
			t.Errorf("KeyBytes[%q] = %d, want %d", k, at.KeyBytes[k], n)
		}
	}
	if _, leaked := at.KeyBytes["desplega"]; leaked {
		t.Errorf("desplega got bytes — the shorter prefix won over desplega:brainstorm")
	}
	if _, leaked := at.KeyBytes["note"]; leaked {
		t.Errorf("an unmatched preamble opened an entry of its own")
	}
}

// TestInstructionsKeyedByFile is the defect-2 gate: `instructions` — the
// largest unattributed attachment type on the corpus at 8.9 MB — keys on
// files[].path, measured against its rendered content; a file with no content
// costs its path's own length (the pairedBytes fallback). Missing or empty
// `files` yields no keys and no dimension: absent stays absent, never zero.
func TestInstructionsKeyedByFile(t *testing.T) {
	t.Run("paths become keys at their rendered cost", func(t *testing.T) {
		ev := Event{Type: "attachment", LineBytes: 1, AttachmentRaw: []byte(
			`{"type":"instructions","files":[{"path":"a.md","content":"AAA"},{"path":"b.md","content":"BBBB"}]}`)}
		at := ev.Attachment()
		if at == nil {
			t.Fatal("Attachment() returned nil")
		}
		if at.KeyDimension != DimInstructionFile {
			t.Errorf("KeyDimension = %q, want %q", at.KeyDimension, DimInstructionFile)
		}
		want := map[string]int64{"a.md": 3, "b.md": 4}
		if len(at.KeyBytes) != len(want) {
			t.Fatalf("KeyBytes = %v, want %v", at.KeyBytes, want)
		}
		for k, n := range want {
			if at.KeyBytes[k] != n {
				t.Errorf("KeyBytes[%q] = %d, want %d", k, at.KeyBytes[k], n)
			}
		}
	})

	t.Run("missing content falls back to the path length", func(t *testing.T) {
		ev := Event{Type: "attachment", LineBytes: 1, AttachmentRaw: []byte(
			`{"type":"instructions","files":[{"path":"c.md"},{"path":"d.md","content":"x"}]}`)}
		at := ev.Attachment()
		if at == nil || at.KeyDimension != DimInstructionFile {
			t.Fatalf("KeyDimension = %v, want %q", at, DimInstructionFile)
		}
		if at.KeyBytes["c.md"] != 4 || at.KeyBytes["d.md"] != 1 {
			t.Errorf("KeyBytes = %v, want c.md at its path length 4 and d.md at 1", at.KeyBytes)
		}
	})

	t.Run("absent files stay absent", func(t *testing.T) {
		for _, payload := range []string{
			`{"type":"instructions"}`,
			`{"type":"instructions","files":[]}`,
		} {
			ev := Event{Type: "attachment", LineBytes: 1, AttachmentRaw: []byte(payload)}
			at := ev.Attachment()
			if at == nil || at.KeyDimension != "" || len(at.KeyBytes) != 0 {
				t.Errorf("payload %s: KeyDimension = %q, KeyBytes = %v, want none",
					payload, at.KeyDimension, at.KeyBytes)
			}
		}
	})
}

// TestDimensionNamesDistinct is the collision gate (defect 2): the new
// attachment dimension must not equal any mechanism-1 dimension name or the
// session dimension, or key rows would land in the wrong table.
func TestDimensionNamesDistinct(t *testing.T) {
	mechanism1 := []string{
		"attribution_skill", "attribution_plugin", "attribution_agent",
		"attribution_mcp_server", "attribution_mcp_tool", "session",
	}
	for _, name := range mechanism1 {
		if DimInstructionFile == name {
			t.Errorf("DimInstructionFile collides with %q", name)
		}
	}
}

// TestDeferredToolsRecordKeyedByName is the defect-3 gate: the record event
// keys on entries[].name under DimMcpTool, mirroring its `deferred_tools_delta`
// sibling — each name costs its own raw entry JSON, the biggest single upfront
// tool-definition load on the corpus at 348.5 kB. An entry with no name costs
// nothing to nobody; empty or missing `entries` yields no keys and no
// dimension.
func TestDeferredToolsRecordKeyedByName(t *testing.T) {
	t.Run("entry names become mcp_tool keys at raw entry length", func(t *testing.T) {
		ev := Event{Type: "attachment", LineBytes: 1, AttachmentRaw: []byte(
			`{"type":"deferred_tools_record","entries":[{"name":"tool_a","description":"d1"},{"name":"tool_b"}]}`)}
		at := ev.Attachment()
		if at == nil {
			t.Fatal("Attachment() returned nil")
		}
		if at.KeyDimension != DimMcpTool {
			t.Errorf("KeyDimension = %q, want %q", at.KeyDimension, DimMcpTool)
		}
		want := map[string]int64{"tool_a": 36, "tool_b": 17}
		if len(at.KeyBytes) != len(want) {
			t.Fatalf("KeyBytes = %v, want %v", at.KeyBytes, want)
		}
		for k, n := range want {
			if at.KeyBytes[k] != n {
				t.Errorf("KeyBytes[%q] = %d, want %d", k, at.KeyBytes[k], n)
			}
		}
	})

	t.Run("unnamed entries cost nothing", func(t *testing.T) {
		ev := Event{Type: "attachment", LineBytes: 1, AttachmentRaw: []byte(
			`{"type":"deferred_tools_record","entries":[{"description":"no name here"}]}`)}
		at := ev.Attachment()
		if at == nil || at.KeyDimension != "" || len(at.KeyBytes) != 0 {
			t.Errorf("unnamed-only entries: KeyDimension = %q, KeyBytes = %v, want none",
				at.KeyDimension, at.KeyBytes)
		}
	})

	t.Run("absent entries stay absent", func(t *testing.T) {
		for _, payload := range []string{
			`{"type":"deferred_tools_record"}`,
			`{"type":"deferred_tools_record","entries":[]}`,
		} {
			ev := Event{Type: "attachment", LineBytes: 1, AttachmentRaw: []byte(payload)}
			at := ev.Attachment()
			if at == nil || at.KeyDimension != "" || len(at.KeyBytes) != 0 {
				t.Errorf("payload %s: KeyDimension = %q, KeyBytes = %v, want none",
					payload, at.KeyDimension, at.KeyBytes)
			}
		}
	})
}

// TestSkillListingMultilineDescription is the reason names and content are not
// paired by index: measured on the corpus, `content` has two more lines than
// `names` on nearly every event, because a description carries its own
// newlines. A continuation line belongs to the skill above it.
func TestSkillListingMultilineDescription(t *testing.T) {
	ev := Event{Type: "attachment", LineBytes: 1, AttachmentRaw: []byte(
		`{"type":"skill_listing","names":["alpha","beta"],` +
			`"content":"- alpha: one\nTRIGGER: two\n- beta: three"}`)}
	at := ev.Attachment()
	if at == nil {
		t.Fatal("Attachment() returned nil")
	}
	// "- alpha: one" (12) + "TRIGGER: two" (12), each plus its newline.
	if got := at.KeyBytes["alpha"]; got != 26 {
		t.Errorf("alpha = %d, want 26 — the continuation line was lost or misfiled", got)
	}
	if got := at.KeyBytes["beta"]; got != 13 {
		t.Errorf("beta = %d, want 13 — the last line carries no trailing newline", got)
	}
}
