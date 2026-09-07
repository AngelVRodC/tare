package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestHelpExitsZeroOnStdout pins the most universal CLI convention there is:
// all three spellings print the same usage to stdout and succeed.
func TestHelpExitsZeroOnStdout(t *testing.T) {
	var first string
	for _, spelling := range []string{"--help", "-h", "help"} {
		var buf bytes.Buffer
		if err := run([]string{spelling}, &buf); err != nil {
			t.Errorf("run(%q) returned %v, want nil", spelling, err)
		}
		if !strings.Contains(buf.String(), "usage: tare <command>") {
			t.Errorf("run(%q) printed no usage:\n%s", spelling, buf.String())
		}
		if first == "" {
			first = buf.String()
		} else if buf.String() != first {
			t.Errorf("run(%q) printed different text than --help", spelling)
		}
	}
}

// TestVersion pins the surface a release depends on: all three spellings print
// the same line on stdout and succeed. `tare scan --version` stays an error
// because the check runs before the parse rather than as a registered flag —
// the same scoping rule --harness and --top follow.
func TestVersion(t *testing.T) {
	var first string
	for _, spelling := range []string{"--version", "-version", "version"} {
		var buf bytes.Buffer
		if err := run([]string{spelling}, &buf); err != nil {
			t.Errorf("run(%q) returned %v, want nil", spelling, err)
		}
		if got := strings.TrimSpace(buf.String()); got == "" || !strings.Contains(got, version) {
			t.Errorf("run(%q) printed %q, want a line carrying %q", spelling, buf.String(), version)
		}
		if first == "" {
			first = buf.String()
		} else if buf.String() != first {
			t.Errorf("run(%q) printed different text than --version", spelling)
		}
	}

	var buf bytes.Buffer
	err := run([]string{"scan", "--version"}, &buf)
	if err == nil {
		t.Fatal("run([scan --version]) returned nil, want an error — the flag means nothing there")
	}
	if !strings.Contains(err.Error(), "not defined") {
		t.Errorf("run([scan --version]) failed with %q, want the flag to be undefined there", err)
	}
}

// TestLeadingFlagSaysOrder covers the error the README already contradicted: a
// flag before the command is an ordering mistake, not an unknown command.
func TestLeadingFlagSaysOrder(t *testing.T) {
	var buf bytes.Buffer
	err := run([]string{"--json", "scan"}, &buf)
	if err == nil {
		t.Fatal("run([--json scan]) returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "flags go after the command") {
		t.Errorf("error is %q, want it to name the flag ordering", err)
	}

	// An actually-unknown command must still say so.
	if err := run([]string{"tool"}, &buf); err == nil || !strings.Contains(err.Error(), `unknown command "tool"`) {
		t.Errorf("run([tool]) error is %v, want unknown command", err)
	}
}

// TestTruncationFlagsAreScoped covers the other half of the "registered only
// where it means something" rule --harness follows: a flag that silently does
// nothing is worse than one that errors. `tare report` is in the
// list because the artifact never truncates, so a cap there would be dead.
//
// It also pins the negative cap, which has to be rejected before a pass reads
// a quarter of a gigabyte to render nothing.
func TestTruncationFlagsAreScoped(t *testing.T) {
	for _, args := range [][]string{
		{"scan", "--all"}, {"tools", "--all"}, {"report", "--all"},
		{"scan", "--top", "3"}, {"report", "--top", "3"},
	} {
		var buf bytes.Buffer
		if err := run(args, &buf); err == nil {
			t.Errorf("run(%v) returned nil, want an error — the flag means nothing there", args)
		}
	}

	var buf bytes.Buffer
	err := run([]string{"attribute", "--top", "-1"}, &buf)
	if err == nil {
		t.Fatal("run([attribute --top -1]) returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "--top needs 0 or more rows") {
		t.Errorf("error is %q, want it to say what a valid --top is", err)
	}
}

// TestPerCommandHelpAlsoExitsZero covers the half of the convention the first
// pass missed: flag reports --help as an error, so `tare scan --help` exited 1
// to stderr while `tare --help` exited 0 to stdout.
func TestPerCommandHelpAlsoExitsZero(t *testing.T) {
	for _, args := range [][]string{{"scan", "--help"}, {"tools", "-h"}, {"corruption", "--help"}} {
		var buf bytes.Buffer
		if err := run(args, &buf); err != nil {
			t.Errorf("run(%v) returned %v, want nil", args, err)
		}
		if !strings.Contains(buf.String(), "usage: tare <command>") {
			t.Errorf("run(%v) printed no usage to stdout:\n%s", args, buf.String())
		}
	}
}

// TestHarnessFlagRejectedOnOtherCommands is the same scoping rule read from the
// other side: `tools` is the one command a second harness supplies, so naming it
// anywhere else has to be an error rather than a flag that reads Claude Code
// and says nothing.
//
// It asserts the parse error specifically, not merely err != nil: dropping the
// registration guard makes `scan --harness opencode` resolve --dir to the
// OpenCode root, which errors on its own wherever that directory is absent. A
// bare nil-check would then pass on a machine without OpenCode installed and
// fail to catch the mutation on one that has it.
func TestHarnessFlagRejectedOnOtherCommands(t *testing.T) {
	for _, cmd := range []string{"scan", "attribute", "corruption", "report"} {
		var buf bytes.Buffer
		err := run([]string{cmd, "--harness", "opencode"}, &buf)
		if err == nil {
			t.Errorf("run([%s --harness opencode]) returned nil, want an error — the flag means nothing there", cmd)
			continue
		}
		if !strings.Contains(err.Error(), "not defined") {
			t.Errorf("run([%s --harness opencode]) failed with %q, want the flag to be undefined there", cmd, err)
		}
	}
}

// TestUnknownHarnessErrors pins that a misspelling is rejected before any
// corpus is read, and that the message names what would have worked. `codex` is
// the case that matters: it is a real harness tare does not read yet, so the
// error has to say which two it does.
func TestUnknownHarnessErrors(t *testing.T) {
	var buf bytes.Buffer
	err := run([]string{"tools", "--harness", "codex"}, &buf)
	if err == nil {
		t.Fatal("run([tools --harness codex]) returned nil, want an error")
	}
	for _, want := range []string{harnessClaudeCode, harnessOpenCode} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is %q, want it to name the valid value %q", err, want)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("an unknown harness wrote %d bytes to stdout, want none", buf.Len())
	}
}

// TestDefaultDirPerHarness covers the trap the --dir default became once it
// depended on a flag parsed in the same pass: registered as "" and resolved
// afterwards, so a resolution that silently returned "" would read the working
// directory instead of the harness's own root.
func TestDefaultDirPerHarness(t *testing.T) {
	for _, tc := range []struct{ harness, suffix string }{
		{harnessClaudeCode, filepath.Join(".claude", "projects")},
		{harnessOpenCode, filepath.Join(".local", "share", "opencode")},
	} {
		got, err := resolveDir(tc.harness, "")
		if err != nil {
			t.Fatalf("resolveDir(%q, \"\") returned %v, want nil", tc.harness, err)
		}
		if !strings.HasSuffix(got, tc.suffix) {
			t.Errorf("resolveDir(%q, \"\") = %q, want it to end in %q", tc.harness, got, tc.suffix)
		}
	}

	// An explicit --dir wins over both defaults, which is what every QA run
	// against a frozen corpus copy depends on.
	for _, harness := range []string{harnessClaudeCode, harnessOpenCode} {
		got, err := resolveDir(harness, "/somewhere/else")
		if err != nil || got != "/somewhere/else" {
			t.Errorf("resolveDir(%q, /somewhere/else) = %q, %v — want the flag to win", harness, got, err)
		}
	}

	if _, err := resolveDir("codex", "/somewhere/else"); err == nil {
		t.Error("resolveDir rejected nothing for an unknown harness with --dir set")
	}
}

// TestSinceUntilAcceptedOnAllCommands pins time-window-1: --since/--until are
// registered globally, like --dir/--json, so no command rejects them as an
// unknown flag. Over an empty dir every command must succeed with a window set.
func TestSinceUntilAcceptedOnAllCommands(t *testing.T) {
	for _, cmd := range []string{"scan", "tools", "attribute", "corruption", "report"} {
		var buf bytes.Buffer
		err := run([]string{cmd, "--dir", t.TempDir(), "--since", "2026-08-01", "--until", "2026-08-15"}, &buf)
		if err != nil {
			t.Errorf("run([%s --since --until]) returned %v, want nil", cmd, err)
		}
	}
}

// TestUsageDocumentsWindowFlags pins the other half of time-window-1: usage()
// is the only flag documentation a user ever sees, so it must name both flags
// and both accepted shapes.
func TestUsageDocumentsWindowFlags(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	for _, want := range []string{"--since", "--until", "YYYY-MM-DD", "YYYY-MM-DDTHH:MM:SS.sssZ"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("usage does not mention %q:\n%s", want, buf.String())
		}
	}
}

// TestWindowBoundFailsBeforeCorpusRead pins time-window-2's ordering: a
// malformed bound fails loudly, naming the two accepted shapes, before
// resolveDir ever runs. The first case passes codex, an unknown harness — if
// resolveDir ran first, the error would name the harness instead of the
// shapes. The second passes a real harness over an empty dir, where the
// OpenCode reader's own missing-database error would fire if the window
// check came late. Neither needs a corpus fixture.
func TestWindowBoundFailsBeforeCorpusRead(t *testing.T) {
	for _, args := range [][]string{
		{"tools", "--harness", "codex", "--since", "not-a-date"},
		{"tools", "--harness", "opencode", "--dir", t.TempDir(), "--until", "2026-08-01T12:00:00Z"},
	} {
		var buf bytes.Buffer
		err := run(args, &buf)
		if err == nil {
			t.Errorf("run(%v) returned nil, want a validation error", args)
			continue
		}
		if strings.Contains(err.Error(), "harness") {
			t.Errorf("run(%v) failed with %q — resolveDir ran before the window validation", args, err)
		}
		for _, want := range []string{"YYYY-MM-DD", "YYYY-MM-DDTHH:MM:SS.sssZ"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("run(%v) error %q does not name the accepted shape %q", args, err, want)
			}
		}
	}
}

// TestHarnessSelectsTheReader is the only check that --harness opencode reaches
// OpenCodeToolsEnvelope at all. Without it, replacing the dispatch branch with
// report.ToolsEnvelope prints a Claude Code table under an OpenCode header,
// exits 0, and every package still passes — the resolveDir and unknown-value
// tests both pass on a mutant that never calls the OpenCode reader.
//
// An empty directory is the discriminator, so this needs no sqlite3 and no
// fixture: the Claude Code reader treats an empty transcript root as an empty
// corpus, while the OpenCode reader has a required file and says the database
// is not readable.
func TestHarnessSelectsTheReader(t *testing.T) {
	empty := t.TempDir()

	var buf bytes.Buffer
	if err := run([]string{"tools", "--dir", empty}, &buf); err != nil {
		t.Fatalf("claude-code over an empty dir returned %v, want nil — the discriminator this test relies on is gone", err)
	}

	buf.Reset()
	err := run([]string{"tools", "--harness", "opencode", "--dir", empty}, &buf)
	if err == nil {
		t.Fatal("opencode over an empty dir returned nil — the dispatch read the Claude Code corpus instead")
	}
	if !strings.Contains(err.Error(), "opencode database not readable") {
		t.Errorf("error is %q, want the OpenCode reader's own missing-database message", err)
	}
}
