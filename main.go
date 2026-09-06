// Command tare reports which installed tooling actually costs context, read
// from local Claude Code transcripts. Zero dependencies, no network.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	// Help is answered before the flag parse, and on stdout rather than stderr:
	// it was asked for, so it is this run's output and belongs in a pipe.
	switch cmd {
	case "--help", "-h", "help":
		usage(out)
		return nil
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	// Discarded, not stderr: a parse failure is returned as an error and main
	// prints it, and a help request is answered below on stdout. Letting the
	// flag package also write its own copy would double every message.
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", defaultDir(), "transcript root directory")
	asJSON := fs.Bool("json", false, "emit the JSON envelope instead of a table")
	// Registered only where it means something, so `tare scan --boost-deep`
	// is an error rather than a flag that silently does nothing.
	var deep bool
	if cmd == "corruption" {
		fs.BoolVar(&deep, "boost-deep", false,
			"join every Boost MCP call from its history DB via sqlite3, not the 100-row JSON sample")
	}
	if err := fs.Parse(args[1:]); err != nil {
		// `tare scan --help` is the same request as `tare --help`, and gets the
		// same answer: usage on stdout, exit 0. flag reports it as an error.
		if errors.Is(err, flag.ErrHelp) {
			usage(out)
			return nil
		}
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
	case "corruption":
		env, err := report.CorruptionEnvelope(*dir, version, deep)
		if err != nil {
			return err
		}
		if *asJSON {
			return report.WriteJSON(out, env)
		}
		return report.RenderCorruption(out, env)
	case "report":
		rep, err := report.BuildReport(*dir, version)
		if err != nil {
			return err
		}
		if *asJSON {
			return report.WriteJSON(out, rep.Envelope)
		}
		return report.RenderReport(out, rep, time.Now())
	default:
		usage(os.Stderr)
		// A leading flag is an ordering mistake, not an unknown command. The
		// README documents the ordering; the error should agree with it rather
		// than report `unknown command "--json"`.
		if strings.HasPrefix(cmd, "-") {
			return fmt.Errorf("flags go after the command: tare <command> %s", cmd)
		}
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
  corruption per-tool error, empty and truncation rates, plus the Boost counterfactual
  report     all four composed into one reproducible artifact (Markdown, or --json)

flags (given after the command):
  --dir string   transcript root (default ~/.claude/projects)
  --json         emit the JSON envelope instead of a table
  --boost-deep   corruption only: join every Boost MCP call, not the 100-row sample
`)
}
