package main

import (
	"bytes"
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
// where it means something" rule the --boost-deep precedent set: a flag that
// silently does nothing is worse than one that errors. `tare report` is in the
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
