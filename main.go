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

// defaultTop is how many rows per dimension the tables print unless told
// otherwise. It lives here rather than in internal/report because the cap is a
// terminal convenience the CLI decides: the renderers obey whatever they are
// handed, and `tare report` hands them nothing at all.
const defaultTop = 15

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
	// Same reason as --boost-deep: registered only on the two commands whose
	// tables are capped, so `tare scan --all` and `tare report --top 3` are
	// errors rather than flags that silently do nothing.
	var all bool
	top := defaultTop
	if cmd == "attribute" || cmd == "corruption" {
		fs.BoolVar(&all, "all", false, "print every row of every table, not just the top --top")
		fs.IntVar(&top, "top", defaultTop, "rows per dimension in the tables; 0 prints every row")
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
	if top < 0 {
		return fmt.Errorf("--top needs 0 or more rows, got %d — 0 means every row", top)
	}
	// --all is --top 0 under another name, so it needs no sentinel of its own:
	// a cap of zero or less truncates nothing. Given both, --all wins.
	if all {
		top = 0
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
		return report.RenderAttribute(out, env, top)
	case "corruption":
		env, err := report.CorruptionEnvelope(*dir, version, deep)
		if err != nil {
			return err
		}
		if *asJSON {
			return report.WriteJSON(out, env)
		}
		return report.RenderCorruption(out, env, top)
	case "report":
		// Four passes over a quarter-gigabyte corpus take about four seconds
		// with nothing printed, which reads as a hang. One line per pass to
		// stderr fixes that without ever touching the artifact on stdout, so
		// `> out.md` still yields a file that is only the report while the
		// terminal shows progress. Gated on stderr being that terminal, so a
		// piped or captured stderr stays silent. The other four commands
		// finish fast enough to need none.
		var progress io.Writer
		if isTTY() {
			progress = os.Stderr
		}
		rep, err := report.BuildReport(*dir, version, progress)
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

// isTTY reports whether stderr is a terminal. This is the only TTY check in
// the program; it gates a progress line and nothing else. Correct on macOS and
// Linux, which is where this tool runs — it does not handle Cygwin/MSYS2 the
// way mattn/go-isatty does, and adding a dependency to cover that would cost
// more than the line is worth.
//
// Stderr, not stdout, because stderr is where the progress goes: the question
// is whether the stream being written to can be seen, and stdout is not that
// stream. Gating on stdout printed nothing for `tare report > out.md` run from
// a real terminal — measured under a PTY — which is the four seconds of
// silence this check exists to remove, in the most common invocation. It also
// dropped a false positive: /dev/null is a character device, so a stdout gate
// fired on `> /dev/null` for no reason. Stderr stays correctly silent for
// `2>&1 | rg`, for `2>err.txt`, and in CI, where nothing is attached.
//
// It lives here rather than in internal/report because this is the only caller
// and this file already owns which stream each command writes to. BuildReport
// honours any writer it is handed, so it stays testable with a buffer from a
// test process that has no terminal at all.
func isTTY() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
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
  --top N        attribute, corruption only: rows per dimension (default 15, 0 for every row)
  --all          attribute, corruption only: same as --top 0
`)
}
