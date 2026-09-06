# tare

![Go](https://img.shields.io/badge/go-1.27%2B-00ADD8?logo=go&logoColor=white)
![Dependencies](https://img.shields.io/badge/dependencies-0-success)
![Network](https://img.shields.io/badge/network-none-success)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

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
2026-08-10 .. 2026-09-06

CORPUS
Tool calls requested                    15,194
Tool results returned                   15,193
Results with no matching call           0
Calls still awaiting a result           1
Distinct tools                          64
Tool calls                              15,193
Bytes returned into context             37.0 MB
Image bytes returned                    2.3 MB
Results carrying an image               15
Bytes tools produced                    41.3 MB
Calls that returned an error            377
Results written to a side file          39
Bytes produced into side files          2.1 MB
Context bytes those results still cost  85.8 kB
all rows measured

TOOL                                                        CALLS  CONTEXT   IMAGES  PRODUCED  ERRORS  SHARE
Bash                                                        9,149  17.9 MB   0 B     20.0 MB   262     48.5%  ***
Read                                                        1,406  12.6 MB   2.3 MB  15.0 MB   15      34.2%  **
mcp__plugin_sre_grafana-prod__query_loki_logs               644    1.9 MB    0 B     1.9 MB    26      5.0%   *
```

Then `tare attribute` for the same volume rolled up by skill, plugin, agent and
MCP server, and `tare report` for all of it in one artifact.

The `SHARE` column is each row's share of the corpus total, and the marks grade
it: `*` ≥5%, `**` ≥20%, `***` ≥35%, `!!` ≥50%. `TOOL` partitions the corpus, so
its shares sum to 100%; `MCP_SERVER` is a *subset* of those same tools, so its
shares are of all context and sum to far less. A table whose every row would
grade blank gets no `SHARE` column at all — the ranking already answers it. The
grade is a rendering of the share, not a separate measurement: it is not in
`--json`, and you can recompute it yourself from `context_bytes`.

Every row still carries a `measured` / `estimated` derivation, but the table
prints it as a column only where a block actually mixes the two. Where every
row agrees it collapses to the single footer line you see above — a column that
repeats one word on all sixty rows says nothing. `--json` tags every row either
way.

That output is one live run. `~/.claude/projects` grows while you read it, so
your own numbers will differ — see [Reproducibility](#reproducibility).

## Commands

Flags come *after* the subcommand: `tare scan --json`, not `tare --json scan`.
`tare --help`, `tare -h` and `tare help` all print this list to stdout and exit
0, so `tare --help | head` works.

| Command | What it answers | Own flags |
|---|---|---|
| `tare scan` | What is in the corpus at all — files, bytes, date range, event types, CLI versions, retention gap | — |
| `tare tools` | What each tool cost — calls, context bytes in, produced bytes out, errors | — |
| `tare attribute` | Which skill / plugin / agent / MCP server the tokens belong to, and how much prior context was re-billed | `--top`, `--all` |
| `tare corruption` | What share of calls errored, returned nothing, or carried a truncation marker | `--boost-deep`, `--top`, `--all` |
| `tare report` | All four, composed into one reproducible artifact | — |

| Global flag | Default | Effect |
|---|---|---|
| `--dir` | `~/.claude/projects` | Transcript root to read |
| `--json` | off | Emit the JSON envelope instead of the table |

`--boost-deep` joins every Boost MCP call from its history DB via `sqlite3`
rather than the 100-row JSON sample. It is registered on `corruption` only, on
purpose, so `tare scan --boost-deep` is an error rather than a flag that
silently does nothing.

`--top N` sets how many rows each dimension prints — 15 by default, `0` for all
of them — and `--all` is `--top 0` under another name. A table that was cut says
so and names the flag: `showing top 15 of 67 skill rows — use --all`. Both are
registered on `attribute` and `corruption` only, for the same reason
`--boost-deep` is: `scan` and `tools` print every row already, and the
Markdown `tare report` never truncates at all — a file is not a terminal, and
`--all` is not spellable after the fact by whoever reads the file.

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

`--json` is untouched by any of the table formatting above: `bytes` is the exact
integer `253603993`, never the `253.6 MB` the table shows. Rendering is a table
concern and the envelope is the contract.

## Reproducibility

Two runs over the same bytes produce the same artifact, down to the last bit of
every float. The Markdown artifact differs in its `generated` row and nowhere
else; the JSON envelope carries no timestamp at all, so it is byte-identical.
That is the point of the tool: a skeptic has to be able to re-run it.

Byte figures in the tables are **SI** — divided by 1000 and labelled `kB`, `MB`,
`GB`. `254.4 MB`, never the IEC `242.6 MiB`; the two are never mixed. `--json`
carries the exact integer, so a script never has to parse a rounded label.

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

One `tare report` over a frozen snapshot, 2026-09-06. A live corpus moves, so
these are one run, not a constant.

**Corpus** — 396 files, 257.8 MB, 2026-08-06 to 2026-09-06, written by 28 Claude
Code versions. 76,395 events across 21 event types, 0 parse errors.

**Tools** — 63 distinct tools, 14,756 calls. 36.4 MB of context sent into them,
40.8 MB produced back. 14,756 `tool_use` blocks against 14,756 `tool_result`: 0
unmatched. Two tools carry the corpus: `Bash` at 47.2% of all context bytes and
`Read` at 35.4%, both graded `***`.

**Re-billing** — 59,351,143 fresh tokens were re-billed as 1,668,436,665 cached
reads: a **28× multiplier**. Prior context charged again is where the money
goes, and it is measured, not modelled.

**Attachments** — 64.0 MB across 35 attachment types, **24.8% of the corpus**.
That is the tare: weight that is not payload.

**Corruption** — 2.5% of calls returned an error, 1.5% returned nothing at all,
0.1% carried a truncation marker.

**What the numbers cannot cover** — 56.4% of assistant responses (15,664 of
27,788) repeat a `message.id` already seen and are deduplicated. Only 63 of 141
sessions (44.7%) carry a billing record. Claude Code's own cache counted 144
sessions against 141 transcripts still on disk: 3 gone.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `tare: no command given` | A subcommand is required. `tare` with no arguments prints the list. |
| `tare: flags go after the command` | Exactly that: `tare scan --json`, not `tare --json scan`. |
| `tare: unknown command "tool"` | Not a subcommand. `tare --help` prints the five that are. |
| `tare: flag provided but not defined: -boost-deep` | `--boost-deep` is registered on `corruption` only, so that it cannot silently do nothing elsewhere. |
| `tare: flag provided but not defined: -all` | `--all` and `--top` are registered on `attribute` and `corruption` only — the two commands whose tables are capped. |
| `tare: --top needs 0 or more rows` | `--top` counts rows. `0` means every row, which is what `--all` asks for. |
| `files 0` and an empty date range | `--dir` is not a transcript root. It should contain per-project subdirectories of `*.jsonl`. |
| Two runs disagree | The corpus is live. Copy it and point `--dir` at the copy — see [Reproducibility](#reproducibility). |
| Dollars read `unavailable` | That session has no `cost-state` event. Reporting the gap is deliberate; reporting `$0.00` would be a lie. |
| `retention_gap` is non-zero | Claude Code pruned transcripts its own cache still counts. Those sessions cannot be measured at all. |
| No Boost rows under `corruption` | `boost` or `sqlite3` is not on `PATH`. The counterfactual degrades to a warning; every other metric still runs. |

## Contributing

Issues and pull requests are welcome. The most useful contribution is a
transcript shape this tool gets wrong — Claude Code has written this corpus in
28 versions so far, and the next one will move something again.

### Getting set up

```bash
git clone https://github.com/AngelVRodC/tare
cd tare
go build ./...
go test ./...
```

Go 1.27 or later, and nothing else. Tests read fixture corpora written to
`t.TempDir()`, never your real transcripts, so the suite is safe to run on any
machine.

### Before you open a pull request

```bash
go build ./...
go test ./...
go vet ./...
gofmt -l .               # expect no output
go list -m all | wc -l   # expect 1
```

### The invariants a change must not break

These are load-bearing, not preferences. A pull request that breaks one needs
to argue the case in its description, not quietly work around it.

| Invariant | Why | Where it is explained |
|---|---|---|
| Standard library only | A tool arguing that your tooling costs more than it returns ships with no dependencies or it argues against itself | [Dependencies](#dependencies) |
| Stream, never load | The corpus is a quarter of a gigabyte and grows | [How it works](#how-it-works) |
| `bufio.Reader`, never `bufio.Scanner` | The longest measured line is 514,199 bytes; any cap is a knob to re-tune later | [How it works](#how-it-works) |
| Sniff the shape, never trust a version field | 28 CLI versions wrote this corpus and none of them promised a schema | [How it works](#how-it-works) |
| Loud on the join, quiet on the unknown | An unmatched `tool_use_id` is a defect; an unrecognised event type is a counter | [How it works](#how-it-works) |
| Every metric carries a `derivation` | An untagged number cannot be argued with | [Every number is tagged](#every-number-is-tagged) |
| No network, ever | Transcript content does not leave the machine | [What it deliberately does not do](#what-it-deliberately-does-not-do) |

`go list -m all | wc -l` is not a formality. Before a module is added it has to
be shown that the standard library is insufficient.

### Commits

Conventional commits — `fix(report): …`, `docs: …`, `refactor(transcript): …`.
Work that implements a numbered plan phase commits as `[phase N] <description>`.

A change to parsing or metrics comes with a test. The fixtures live beside the
code they exercise, and a new transcript shape is worth more as a fixture than
as a bug report.

## License

[MIT](LICENSE) © Angel Rodriguez
