package report

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// cmdTimeout caps every shell-out. The local binaries tare reads through
// answer in about a second; a hung child must not hang tare.
var cmdTimeout = 2 * time.Minute

// runCmd runs a child to completion under a timeout and returns its stdout.
// Nothing here touches the network; these are local binaries reading local
// files, and stderr is folded into the error so a failure is never silent.
func runCmd(bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", bin, err, msg)
		}
		return nil, fmt.Errorf("%s: %w", bin, err)
	}
	return out, nil
}
