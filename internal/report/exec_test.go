package report

import (
	"strings"
	"testing"
	"time"
)

// TestRunCmd covers the shell-out helper every adapter that reads a local
// binary depends on: stdout comes back, a non-zero exit is an error naming
// the binary, and the deadline actually kills a child that outlives it.
func TestRunCmd(t *testing.T) {
	t.Run("captures stdout", func(t *testing.T) {
		out, err := runCmd("echo", "hello")
		if err != nil {
			t.Fatalf("runCmd(echo): %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != "hello" {
			t.Errorf("stdout = %q, want %q", got, "hello")
		}
	})

	t.Run("non-zero exit is an error", func(t *testing.T) {
		out, err := runCmd("false")
		if err == nil {
			t.Fatalf("runCmd(false) = %q, want an error", out)
		}
		if !strings.Contains(err.Error(), "false") {
			t.Errorf("error must name the binary that failed: %v", err)
		}
	})

	t.Run("timeout kills a hung child", func(t *testing.T) {
		restore := cmdTimeout
		cmdTimeout = 50 * time.Millisecond
		t.Cleanup(func() { cmdTimeout = restore })

		start := time.Now()
		if _, err := runCmd("sleep", "30"); err == nil {
			t.Fatal("runCmd(sleep 30) returned nil, want the deadline to fire")
		}
		if waited := time.Since(start); waited > 10*time.Second {
			t.Errorf("runCmd waited %v: the deadline did not fire", waited)
		}
	})
}
