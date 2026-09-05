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
		name:    "hook_success keys on hookName and takes the whole line",
		payload: `{"type":"hook_success","hookName":"boost-awareness","command":"x"}`,
		wantTyp: "hook_success",
		wantDim: DimHookName,
		wantKey: map[string]int64{"boost-awareness": 999},
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
