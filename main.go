// Command tare reports which installed tooling actually costs context, read
// from local Claude Code transcripts. Zero dependencies, no network.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/AngelVRodC/tare/internal/report"
)

// version is the build version reported in the --json envelope.
const version = "0.1.0"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "tare:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return fmt.Errorf("no command given")
	}

	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", defaultDir(), "transcript root directory")
	asJSON := fs.Bool("json", false, "emit the JSON envelope instead of a table")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	switch cmd {
	case "scan":
		env, err := report.ScanEnvelope(*dir, version)
		if err != nil {
			return err
		}
		if *asJSON {
			return report.WriteJSON(out, env)
		}
		return report.RenderScan(out, env)
	case "tools":
		env, err := report.ToolsEnvelope(*dir, version)
		if err != nil {
			return err
		}
		if *asJSON {
			return report.WriteJSON(out, env)
		}
		return report.RenderTools(out, env)
	case "attribute":
		env, err := report.AttributeEnvelope(*dir, version)
		if err != nil {
			return err
		}
		if *asJSON {
			return report.WriteJSON(out, env)
		}
		return report.RenderAttribute(out, env)
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// defaultDir is where Claude Code keeps its transcripts.
func defaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".claude", "projects")
	}
	return filepath.Join(home, ".claude", "projects")
}

func usage(w io.Writer) {
	fmt.Fprint(w, `tare — what your tooling costs, measured from local Claude Code transcripts.

usage: tare <command> [flags]

commands:
  scan       corpus inventory: files, bytes, date range, per-type event counts
  tools      per-tool call counts, context bytes, produced bytes and errors
  attribute  tokens by skill/plugin/agent/MCP, context re-billing, attachment volume

flags (given after the command):
  --dir string   transcript root (default ~/.claude/projects)
  --json         emit the JSON envelope instead of a table
`)
}
