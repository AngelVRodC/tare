# tare

*The weight of the empty container, excluded from the payload.*

`tare` reads your local Claude Code transcripts and reports which of your
installed tooling actually costs context: per-tool byte volume, per-skill /
per-plugin / per-MCP-server attribution, and the evidence that a tool changed
an answer. Every number is tagged with how it was arrived at.

## Why it exists

Anthropic's own OpenTelemetry export redacts third-party plugin and skill names
to `"third-party"` and user-configured MCP servers to `"custom"`. The same
attribution sits un-redacted in `~/.claude/projects/**/*.jsonl` on your own
disk. `tare` reads it there. Nothing leaves the machine.

## Install

```bash
go build -o tare .
```

Go 1.27 or later. There is nothing else to install — see Dependencies.

## Usage

Flags come *after* the subcommand: `tare scan --json`, not `tare --json scan`.

```bash
tare scan          # corpus inventory: files, bytes, date range, event types
tare tools         # per-tool calls, context bytes, produced bytes, errors
tare attribute     # tokens by skill/plugin/agent/MCP, re-billing, attachments
tare corruption    # per-tool error, empty and truncation rates
tare report        # all four composed into one reproducible artifact
```

Global flags: `--dir` (default `~/.claude/projects`) and `--json`.
`tare corruption` also takes `--boost-deep`.

`tare report` writes the artifact: Markdown for a reader, `--json` for a
machine. Both are self-contained — the header records the tool version, the
corpus path, its file and byte count, its date range, every Claude Code version
that wrote it, and the exact command to re-run.

## Every number is tagged

Each metric carries a `derivation`:

- `measured` — read straight off the transcript.
- `estimated` — derived, and the row names the `method` it was derived by.

Not one dollar figure is `measured`. Claude Code records cost once per session,
so every per-tool or per-skill dollar is an allocation of a session bill across
weighted tokens, and says so.

Anything a command could not compute goes into `warnings[]` — never omitted
silently, and never reported as zero. A session with no billing record shows
`unavailable`, not `$0.00`; the difference is the finding.

## Reproducibility

Two runs over the same bytes produce the same artifact, down to the last bit of
every float. Only the `generated` line differs. That is the point of the tool:
a skeptic has to be able to re-run it.

`~/.claude/projects` is a *live* directory — Claude Code appends to it while
`tare` reads it — so two runs minutes apart legitimately differ. Point `--dir`
at a frozen copy to reproduce a figure exactly. When the passes disagree about
what they read, the artifact says so in a warning rather than quoting the first
pass as the whole truth.

## What it deliberately does not do

- **Aggregate spend reporting.** `ccusage` owns that; use it. `tare` answers a
  different question — *which of my tooling costs the context* — which `ccusage`
  closed as not planned.
- **Outcome measurement.** `tare` says what your tooling costs. It cannot say
  whether it paid for itself; that needs a counterfactual (replay the task with
  the skill disabled), and it is deliberately not in this version.
- **Other harnesses.** Claude Code only. No adapter interface, no plugin
  registry — one implementation is not an abstraction.
- **Anything over the network.** No pricing API, no telemetry, no update check.
  The only external processes it ever starts are local binaries (`boost`,
  `sqlite3`) for the optional Boost counterfactual, and their absence degrades
  to a stated warning.

## Known ceilings

Stated up front, because a tool that argues about cost has to be honest about
its own error bars.

| Ceiling | Consequence |
|---|---|
| Cost is billed once per session — not per turn, and not per tool | Every dollar figure is `estimated`, allocated by weighted tokens |
| Not every session carries a billing record | Sessions without one report dollars as unavailable, never as zero |
| Claude Code prunes transcripts | Sessions Claude Code counted are gone from disk; `tare scan` reports the gap |
| The same API response is written to the transcript many times | Responses are deduplicated by `message.id` before any token is summed |
| A truncation marker is a literal substring match | A result that quotes one is a false positive; the named tools have to be checked |

## Dependencies

Zero.

```bash
go list -m all | wc -l   # 1 — the module itself, nothing else
```

Standard library only: `encoding/json`, `bufio`, `os`, `os/exec`,
`text/tabwriter`, `flag`. No CLI framework, no table library, no HTTP client. A
tool whose argument is *"your tooling costs more than it returns"* ships with no
dependencies or it argues against itself.

## A run on the author's corpus

One `tare report` over a frozen snapshot, 2026-09-05. A live corpus moves, so
these are one run, not a constant.

**Corpus** — 389 files, 250,735,113 bytes, 2026-08-06 to 2026-09-06, written by
28 Claude Code versions. 74,172 events across 21 event types, 0 parse errors.

**Tools** — 63 distinct tools, 14,436 calls. 35,858,323 bytes of context sent
into them, 40,228,610 bytes produced back. 14,437 `tool_use` blocks against
14,436 `tool_result`: 0 unmatched, 1 call never answered.

**Re-billing** — 58,478,953 fresh tokens were re-billed as 1,636,600,446 cached
reads: a **27.99x multiplier**. Prior context charged again is where the money
goes, and it is measured, not modelled.

**Attachments** — 60,464,564 bytes across 35 attachment types, **24.1% of the
corpus**. That is the tare: weight that is not payload.

**Corruption** — 2.52% of calls returned an error, 1.48% returned nothing at
all, 0.055% carried a truncation marker.

**What the numbers cannot cover** — 56.4% of assistant responses (15,322 of
27,162) repeat a `message.id` already seen and are deduplicated. Only 59 of 136
sessions (43.4%) carry a billing record. Claude Code's own cache counted 144
sessions against 136 transcripts still on disk: 8 gone.
