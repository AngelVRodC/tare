# tare

![Go](https://img.shields.io/badge/go-1.27%2B-00ADD8?logo=go&logoColor=white)
![Dependencies](https://img.shields.io/badge/dependencies-0-success)
![Network](https://img.shields.io/badge/network-none-success)

*The weight of the empty container, excluded from the payload.*

`tare` reads your local Claude Code transcripts and reports which of your
installed tooling actually costs context: per-tool byte volume, per-skill /
per-plugin / per-MCP-server attribution, and the evidence that a tool changed
an answer. Every number is tagged with how it was arrived at.

> ```bash
> go build -o tare . && ./tare report
> ```

## Why it exists

Anthropic's own OpenTelemetry export redacts third-party plugin and skill names
to `"third-party"` and user-configured MCP servers to `"custom"`. The same
attribution sits un-redacted in `~/.claude/projects/**/*.jsonl` on your own
disk. `tare` reads it there. Nothing leaves the machine.

## How it works

Claude Code appends one JSON object per line to a transcript file per session.
`tare` walks that tree and streams it — a quarter of a gigabyte is never held
in memory, and the longest line measured so far is 514,199 bytes, so the reader
is a `bufio.Reader` rather than a `bufio.Scanner` with a limit to re-tune
later.

```
~/.claude/projects/**/*.jsonl        live — Claude Code appends while tare reads
        │
        │  bufio.Reader, one line at a time; the corpus is never loaded
        ▼
   event stream                      21 event types here, from 28 CLI versions
        │
        ├─ tool_use ⟷ tool_result    joined on tool_use_id → bytes per tool
        ├─ message.id                deduplicated → fresh vs. re-billed tokens
        ├─ attachment                rolled up by type → the tare itself
        └─ cost-state                the session bill, recorded once per session
```

Event shapes are sniffed, never trusted to a version field: tolerant structs,
with `json.RawMessage` over the regions that changed across those 28 versions.
An unknown event type increments a counter and is reported; it is never fatal.
An unmatched `tool_use_id` is the opposite — the join is the product, so a
failure there is a reportable defect, not a rounding error.

`tare report` runs the four commands as four independent passes and merges
them. They overlap on purpose — `tools` and `corruption` both count calls per
tool — and where two passes disagree about the same metric the artifact emits a
warning instead of quoting the first one as the truth.

## Install

```bash
go build -o tare .
```

Go 1.27 or later. There is nothing else to install — see [Dependencies](#dependencies).

## Quick start

```bash
$ tare tools
```

```
tare tools — /Users/you/.claude/projects
2026-08-06 .. 2026-09-06

CORPUS
tool_use_blocks              14,583      measured
tool_result_blocks           14,582      measured
unmatched_results            0           measured
unanswered_uses              1           measured
distinct_tools               63          measured
calls                        14,582      measured
context_bytes                36,127,457  measured
image_bytes                  2,348,764   measured
produced_bytes               40,497,744  measured
errors                       367         measured

TOOL                                           CALLS  CONTEXT     IMAGES     PRODUCED    ERRORS
Bash                                           8,618  17,018,358  0          19,039,881  253
Read                                           1,443  12,813,216  2,348,764  15,161,980  15
mcp__plugin_sre_grafana-prod__query_loki_logs  644    1,857,636   0          1,857,636   26
Grep                                           502    1,045,774   0          1,045,774   4
WebSearch                                      282    792,390     0          792,390     4
```

Then `tare attribute` for the same volume rolled up by skill, plugin, agent and
MCP server, and `tare report` for all of it in one artifact. Every row carries
the `measured` / `estimated` tag you see in the right-hand column.

That output is one live run. `~/.claude/projects` grows while you read it, so
your own numbers will differ — see [Reproducibility](#reproducibility).

## Commands

Flags come *after* the subcommand: `tare scan --json`, not `tare --json scan`.

| Command | What it answers | Own flags |
|---|---|---|
| `tare scan` | What is in the corpus at all — files, bytes, date range, event types, CLI versions, retention gap | — |
| `tare tools` | What each tool cost — calls, context bytes in, produced bytes out, errors | — |
| `tare attribute` | Which skill / plugin / agent / MCP server the tokens belong to, and how much prior context was re-billed | — |
| `tare corruption` | What share of calls errored, returned nothing, or carried a truncation marker | `--boost-deep` |
| `tare report` | All four, composed into one reproducible artifact | — |

| Global flag | Default | Effect |
|---|---|---|
| `--dir` | `~/.claude/projects` | Transcript root to read |
| `--json` | off | Emit the JSON envelope instead of the table |

`--boost-deep` joins every Boost MCP call from its history DB via `sqlite3`
rather than the 100-row JSON sample. It is registered on `corruption` only, on
purpose, so `tare scan --boost-deep` is an error rather than a flag that
silently does nothing.

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

`--json` puts the same tag on every row, so the claim is checkable by a script
and not only by eye:

```json
{
  "tool": "tare",
  "version": "0.1.0",
  "command": "scan",
  "corpus": {
    "dir": "/Users/you/.claude/projects",
    "files": 391,
    "bytes": 253603993,
    "from": "2026-08-06",
    "to": "2026-09-06"
  },
  "metrics": [
    {
      "name": "bytes",
      "dimension": "corpus",
      "key": "",
      "value": 253603993,
      "unit": "bytes",
      "derivation": "measured",
      "method": null
    }
  ],
  "warnings": []
}
```

## Reproducibility

Two runs over the same bytes produce the same artifact, down to the last bit of
every float. The Markdown artifact differs in its `generated` row and nowhere
else; the JSON envelope carries no timestamp at all, so it is byte-identical.
That is the point of the tool: a skeptic has to be able to re-run it.

`~/.claude/projects` is a *live* directory — Claude Code appends to it while
`tare` reads it — so two runs minutes apart legitimately differ. Point `--dir`
at a frozen copy to reproduce a figure exactly. When the passes disagree about
what they read, the artifact says so in a warning rather than quoting the first
pass as the whole truth.

```bash
cp -R ~/.claude/projects /tmp/frozen
tare report --dir /tmp/frozen --json > a.json
tare report --dir /tmp/frozen --json > b.json
diff a.json b.json      # no output — the two runs are byte-identical
```

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

## Troubleshooting

| Symptom | Fix |
|---|---|
| `tare: no command given` | A subcommand is required. `tare` with no arguments prints the list. |
| `tare: unknown command "--json"` | Flags go after the subcommand: `tare scan --json`, not `tare --json scan`. |
| `tare: flag provided but not defined: -boost-deep` | `--boost-deep` is registered on `corruption` only, so that it cannot silently do nothing elsewhere. |
| `files 0` and an empty date range | `--dir` is not a transcript root. It should contain per-project subdirectories of `*.jsonl`. |
| Two runs disagree | The corpus is live. Copy it and point `--dir` at the copy — see [Reproducibility](#reproducibility). |
| Dollars read `unavailable` | That session has no `cost-state` event. Reporting the gap is deliberate; reporting `$0.00` would be a lie. |
| `retention_gap` is non-zero | Claude Code pruned transcripts its own cache still counts. Those sessions cannot be measured at all. |
| No Boost rows under `corruption` | `boost` or `sqlite3` is not on `PATH`. The counterfactual degrades to a warning; every other metric still runs. |

## Contributing

```bash
go build ./...
go test ./...
go vet ./...
gofmt -l .               # expect no output
go list -m all | wc -l   # expect 1
```

The last line is not a formality. A dependency added here costs the tool its
argument, so it has to be shown that the standard library is insufficient
before a module is added.

Tests read fixture corpora written to `t.TempDir()`, never your real
transcripts.
