# tare

![Go](https://img.shields.io/badge/go-1.27%2B-00ADD8?logo=go&logoColor=white)
![Dependencies](https://img.shields.io/badge/dependencies-0-success)
![Network](https://img.shields.io/badge/network-none-success)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

*The weight of the empty container, excluded from the payload.*

`tare` reads your local agent transcripts and reports which of your installed
tooling actually costs context: per-tool byte volume, per-skill / per-plugin /
per-MCP-server attribution, and where the context is re-billed. It says what
your tooling **costs**, not whether it was worth it — see
[What it deliberately does not do](#what-it-deliberately-does-not-do). Every
number is tagged with how it was arrived at, and a number the harness never
recorded is named as unavailable rather than printed as zero.

Two harnesses are read: Claude Code, and [OpenCode](#opencode) behind
`tare tools --harness opencode`.

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
   event stream                      21 event types here, from 29 CLI versions
        │
        ├─ tool_use ⟷ tool_result    joined on tool_use_id → bytes per tool
        ├─ message.id                deduplicated → fresh vs. re-billed tokens
        ├─ attachment                rolled up by type → the tare itself
        └─ cost-state                the session bill, recorded once per session
```

Not every `.jsonl` under that root is a transcript. The Workflow tool writes
its own journal beside the subagent transcripts, at
`<session>/subagents/workflows/wf_*/journal.jsonl` — a resume cache, not a
conversation. A record carrying an `agentId` and a resume `key` with no
`sessionId`, whose `type` is one the journal writes, is classified as a journal
entry and kept out of the event counts. Those files and records get their own
`scan` rows — `files_non_transcript` and `non_transcript_records` — plus a
warning, so they are never dropped in silence. `bytes` still counts them,
because they are on the disk.

Event shapes are sniffed, never trusted to a version field: tolerant structs,
with `json.RawMessage` over the regions that changed across those 29 versions.
An unknown event type increments a counter and is reported; it is never fatal.
An unmatched `tool_use_id` is the opposite — the join is the product, so a
failure there is a reportable defect, not a rounding error.

`tare report` runs the four commands as four independent passes and merges
them. They overlap on purpose — `tools` and `corruption` both count calls per
tool — and where two passes disagree about the same metric the artifact emits a
warning instead of quoting the first one as the truth.

All of the above is the Claude Code path. OpenCode keeps its transcripts in one
SQLite database rather than a tree of `.jsonl`, and only `tare tools` reads it —
see [OpenCode](#opencode).

## Install

Go 1.27 or later.

```bash
go install github.com/AngelVRodC/tare@latest
```

Puts `tare` in `$(go env GOPATH)/bin`. Add it to PATH if it is not there
already.

Build from source instead:

```bash
git clone https://github.com/AngelVRodC/tare.git
cd tare && go build -o tare .
```

`--harness opencode` also needs the `sqlite3` binary on PATH.

## Updating

```bash
go install github.com/AngelVRodC/tare@latest
```

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

Tool names shaped `mcp__plugin_<plugin>_<server>__<tool>` are MCP servers
provided by a plugin, and get a per-plugin rollup in a third table, `PLUGIN`,
after `MCP_SERVER`. The plugin names come from `enabledPlugins` in
`~/.claude/settings.json` — user-scope config, read from home and never from
`--dir`, and a name authority only: the value behind an entry (including a
disabled plugin's `false`) never filters, because a disabled plugin's tools
still ran and still cost bytes. It is the same kind of split as `MCP_SERVER` —
a subset of the tool rows, so its shares do not sum to 100 either.

A segment no configured name claims lands in one `plugin (unresolved)` row,
with a warning naming the segments — the bytes still arrive, never dropped.
Project- and marketplace-scoped plugins are absent from that file, so this is
how they show up: unresolved, not missing. Longest-name-first matching can
mis-split a contrived name (a server literally named `plugin_notion` under a
plugin called `my`), but every byte still lands in some plugin bucket or in
unresolved.

That output is one live run. `~/.claude/projects` grows while you read it, so
your own numbers will differ — see [Reproducibility](#reproducibility).

## Commands

Flags come *after* the subcommand: `tare scan --json`, not `tare --json scan`.
`tare --help`, `tare -h` and `tare help` all print this list to stdout and exit
0, so `tare --help | head` works. `tare --version`, `tare -version` and
`tare version` print the version the same way.

| Command | What it answers | Own flags |
|---|---|---|
| `tare scan` | What is in the corpus at all — files, bytes, date range, event types, CLI versions, retention gap, and any `.jsonl` under the root that is not a transcript | — |
| `tare tools` | What each tool cost — calls and context bytes in; errors and produced bytes where recorded | `--harness` |
| `tare attribute` | Which skill / plugin / agent / MCP server the tokens belong to, and how much prior context was re-billed | `--top`, `--all` |
| `tare corruption` | What share of calls failed, how many the harness denied instead, and what returned nothing or carried a truncation marker | `--top`, `--all` |
| `tare report` | All four, composed into one reproducible artifact | — |

| Flag given alone | Effect |
|---|---|
| `--help` | Print this message and exit |
| `--version` | Print the version and exit |

| Global flag | Default | Effect |
|---|---|---|
| `--dir` | `~/.claude/projects`; `~/.local/share/opencode` with `--harness opencode` | Transcript root to read |
| `--json` | off | Emit the JSON envelope instead of the table |

`--harness` is `tools` only: which harness to read, claude-code (default) or
opencode. Registered there and nowhere else, so `tare scan --harness opencode`
is an error rather than a flag that silently does nothing. `scan`, `attribute`,
`corruption` and `report` read Claude Code and nothing else. See
[OpenCode](#opencode) for what the second reader can and cannot measure.

`--top N` sets how many rows each dimension prints — 15 by default, `0` for all
of them — and `--all` is `--top 0` under another name. A table that was cut says
so and names the flag: `showing top 15 of 67 skill rows — use --all`. Both are
registered on `attribute` and `corruption` only, for the same reason
`--harness` is registered on `tools` only: `scan` and `tools` print every row
already, and the Markdown `tare report` never truncates at all — a file is not
a terminal, and `--all` is not spellable after the fact by whoever reads the
file.

`tare report` writes the artifact: Markdown for a reader, `--json` for a
machine. Both are self-contained — the header records the tool version, the
corpus path, its file and byte count, its date range, every Claude Code version
that wrote it, and the exact command to re-run.

## OpenCode

```bash
$ tare tools --harness opencode
```

```
tare tools — /Users/you/.local/share/opencode
2026-08-16 .. 2026-08-16

CORPUS
Distinct tools                18
Tool calls                    94
Bytes returned into context   189.6 kB
Calls that returned an error  1
all rows measured

TOOL                         CALLS  CONTEXT   IMAGES  PRODUCED  ERRORS  SHARE
read                         24     103.5 kB                    0       54.6%  !!
webfetch                     2      37.2 kB                     0       19.6%  *
bash                         21     20.6 kB                     0       10.9%  *
context7_query-docs          2      9.8 kB                      0       5.1%   *
task                         5      6.9 kB                      1       3.7%
engram_mem_search            6      3.2 kB                      0       1.7%
context7_resolve-library-id  1      1.8 kB                      0       1.0%
engram_mem_save              5      1.4 kB                      0       0.8%
glob                         2      1.4 kB                      0       0.7%
question                     3      1.4 kB                      0       0.7%
engram_mem_context           1      585 B                       0       0.3%
engram_mem_current_project   2      471 B                       0       0.2%
engram_mem_session_summary   2      347 B                       0       0.2%
write                        13     312 B                       0       0.2%
grep                         1      274 B                       0       0.1%
engram_mem_save_prompt       1      234 B                       0       0.1%
engram_mem_review            1      130 B                       0       0.1%
edit                         2      52 B                        0       0.0%

MCP_SERVER  CALLS  CONTEXT  IMAGES  PRODUCED  ERRORS  SHARE
context7    3      11.6 kB                    0       6.1%   *
engram      18     6.4 kB                     0       3.4%

warning: image_bytes and image_results are not reported for opencode: it stores a tool result as one output string with no image payload broken out, so the figure is unmeasured — it is not a measurement of zero
warning: produced_bytes, externalised_results, externalised_produced_bytes and externalised_context_bytes are not reported for opencode: it records no pre-truncation output size and writes no side files, so what a tool produced before it reached the context is unmeasured — it is not a measurement of zero
warning: tool_use_blocks, tool_result_blocks, unmatched_results and unanswered_uses are not reported for opencode: the call and its result share one row, so the join those counters audit does not exist here — they have no meaning rather than a value of zero
warning: corpus bytes is the size of opencode.db on disk, which includes indices and tables this command does not read — it is not comparable to a Claude Code corpus byte count
```

The envelope is the same one the Claude Code path emits — same command name,
same metric names — so the table, `--json` and the derivation contract are
unchanged. Only the reader differs.

`--dir` defaults to `~/.local/share/opencode`, which has to contain
`opencode.db`. Reading it shells out to the system `sqlite3` — **required**
here, not optional; see [Dependencies](#dependencies) — and the rollup runs
*inside* SQLite, so no message content ever enters the process.

**Reading this database writes to its directory.** Measured, and it is the one
thing about this reader that is not obvious: `opencode.db` is in WAL mode, and
a `sqlite3 -readonly` open needs `opencode.db-wal` beside it — that file, not
`-shm`, is what decides whether the open succeeds. `-shm` is a file SQLite
builds for itself, so when it is absent SQLite creates it, and a `-readonly`
read therefore needs a **writable directory**. `opencode.db` and
`opencode.db-wal` are never modified — byte-for-byte identical afterwards; the
only change on disk is a new `opencode.db-shm`. Consequences before you point `--dir`
somewhere:

- To freeze a snapshot, copy all three of `opencode.db`, `opencode.db-wal` and
  `opencode.db-shm`. With all three present nothing is created and a read-only
  directory works.
- Copy the `.db` alone — or the `.db` with only `-shm` — and the read **fails
  loudly** rather than quietly reporting a stale number. The error names the
  file to copy.
- An archive on read-only media fails unless `-shm` was archived with it.

MCP server names come from `~/.config/opencode/opencode.json` under `.mcp`.
They have to: OpenCode joins server and tool with a single `_`, and tool names
contain `_` of their own, so `engram_mem_search` splits as plausibly into
`engram_mem` / `search` as into `engram` / `mem_search`. The configured list is
the only authority, matched longest name first so a server whose name prefixes
another cannot claim its tools. Claude Code's uglier `mcp__server__tool` needs
no config — that is the one place it is the better design. With no config file
there is no `MCP_SERVER` block at all, and a warning says so rather than the
table quietly shrinking.

### What OpenCode does not record

A blank cell above is **unmeasured, never a measured zero** — the four warnings
in that run name every one of them. The rule holds even where the zero would be
*true*: OpenCode writes no side files, so `externalised_*` really is nothing,
and tare still withholds it rather than printing a `0` it did not measure.

| Absent | Cause |
|---|---|
| `image_bytes`, `image_results` | A result is one output string, with no image payload broken out |
| `produced_bytes` | No pre-truncation output size is recorded |
| `externalised_results`, `externalised_produced_bytes`, `externalised_context_bytes` | No side files are written |
| `tool_use_blocks`, `tool_result_blocks`, `unmatched_results`, `unanswered_uses` | `callID` sits on the result's own row, so the join these audit does not exist |
| `errors`, per tool | Withheld, with the corpus total, for any tool whose rows carry no call status |

`corpus.bytes` here is the size of `opencode.db` on disk — indices and unread
tables included — so it is not comparable to a Claude Code corpus byte count.

Tokens, dollars and OpenCode's own `agent` field are all present in the
database and none of them are read yet. `tare attribute` stays Claude Code
only until it is decided what a table covering one of its five dimensions is
allowed to claim.

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
  "version": "0.4.0",
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
- **An adapter interface.** Two harnesses are read, and neither reaches the
  other through an interface or a plugin registry. `tare tools` picks one of two
  functions and hands both results to the same renderer — that is the entire
  seam. Two readers, one of which supplies one of the five commands, is not yet
  a shape worth inventing.
- **Anything over the network.** No pricing API, no telemetry, no update check.
  The only external process it ever starts is a local binary — `sqlite3`, to
  read the OpenCode database. A missing `sqlite3` fails `--harness opencode`
  outright, because there it is the only data source.

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
| Plugin names come from user-scope `settings.json` only | Project- and marketplace-scoped plugins are invisible to the authority, so their segments report as `plugin (unresolved)` rather than by name |
| OpenCode records no image payload and no pre-truncation output size | Those columns are blank under `--harness opencode`, and blank means unmeasured — see [What OpenCode does not record](#what-opencode-does-not-record) |

## Dependencies

Two different things get called a dependency, and conflating them is how a
"zero dependencies" badge starts lying. Both are stated:

| | |
|---|---|
| **Go modules** | **Zero.** Standard library only: `encoding/json`, `bufio`, `os`, `os/exec`, `text/tabwriter`, `flag`. No CLI framework, no table library, no HTTP client. |
| **Local binaries invoked** | `sqlite3` — **required** for `--harness opencode`, which fails outright without it because it is that reader's only data source. It is a binary tare shells out to, not a Go module compiled in. Nothing else, ever, and never over a network. |

```bash
go list -m all | wc -l   # 1 — the module itself, nothing else
```

A tool whose argument is *"your tooling costs more than it returns"* ships with
no dependencies or it argues against itself. That argument is about what gets
compiled in and shipped to you; a system binary you already have is a different
claim, so it gets its own row rather than being quietly folded into the zero.

## A run on the author's corpus

One `tare report` over a frozen snapshot, 2026-09-07, taken with v0.4.0 — the
first release where attachment bytes measure the attachment payload, not the
JSONL record around it. A live corpus moves, so these are one run, not a
constant.

**Corpus** — 495 files, 334.0 MB, 2026-08-10 to 2026-09-08, written by 29 Claude
Code versions. 97,519 events across 19 event types, 0 parse errors.

**Tools** — 64 distinct tools, 18,401 calls. 44.2 MB of context sent into them,
49.5 MB produced back. 18,401 `tool_use` blocks against 18,401 `tool_result`: 0
unmatched. Two tools carry the corpus: `Bash` at 47.2% of all context bytes,
graded `***`, and `Read` at 34.4%, graded `**`.

**Re-billing** — 71,915,548 fresh tokens were re-billed as 2,067,469,945 cached
reads: a **29× multiplier**. Prior context charged again is where the money
goes, and it is measured, not modelled.

**Attachments** — 64.4 MB across 38 attachment types, **19.3% of the corpus**.
That is the tare: weight that is not payload. (v0.3.0 counted the same corpus
at 24.8% because its unit was the whole JSONL record — envelope included. The
share did not shrink; the ruler got honest. See the v0.4.0 release notes.)

**Corruption** — 2.5% of calls returned an error, 1.5% returned nothing at all,
0.2% carried a truncation marker.

**What the numbers cannot cover** — 56.2% of assistant responses (19,657 of
34,948) repeat a `message.id` already seen and are deduplicated. Only 86 of 174
sessions (49.4%) carry a billing record. Claude Code's own stats cache still
counts 144 sessions against 174 transcripts on disk — the cache itself trails
its corpus by 30.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `tare: no command given` | A subcommand is required. `tare` with no arguments prints the list. |
| `tare: flags go after the command` | Exactly that: `tare scan --json`, not `tare --json scan`. |
| `tare: unknown command "tool"` | Not a subcommand. `tare --help` prints the five that are. |
| `tare: flag provided but not defined: -all` | `--all` and `--top` are registered on `attribute` and `corruption` only — the two commands whose tables are capped. |
| `tare: --top needs 0 or more rows` | `--top` counts rows. `0` means every row, which is what `--all` asks for. |
| `files 0` and an empty date range | `--dir` is not a transcript root. It should contain per-project subdirectories of `*.jsonl`. |
| Two runs disagree | The corpus is live. Copy it and point `--dir` at the copy — see [Reproducibility](#reproducibility). |
| Dollars read `unavailable` | That session has no `cost-state` event. Reporting the gap is deliberate; reporting `$0.00` would be a lie. |
| `retention_gap` is non-zero | Claude Code pruned transcripts its own cache still counts. Those sessions cannot be measured at all. |
| `tare: flag provided but not defined: -harness` | `--harness` is registered on `tools` only. The other four commands read Claude Code and nothing else. |
| `tare: unknown --harness "…"` | The two values are `claude-code` and `opencode`. The error names both. |
| `tare: sqlite3 is not on PATH …` | The OpenCode reader has no other data source, so it fails rather than returning a partial answer. `sqlite3` ships with macOS. |
| `tare: opencode database not readable` | `--dir` has to be the directory holding `opencode.db`, not the file itself. It defaults to `~/.local/share/opencode`. |
| `tare: opencode query failed: the opencode.db-wal sidecar is missing` | `opencode.db` is in WAL mode and cannot be opened read-only without `-wal`. Copy all three of `.db`, `.db-wal` and `.db-shm`. Copying `.db` plus `-shm` does *not* help — `-wal` is the one that matters. |
| `tare: opencode query failed: opencode.db-shm is absent, so sqlite3 has to create it` | The directory is not writable. Reading needs to create `-shm`; copy all three files somewhere writable, or archive `-shm` alongside the other two. |
| The `IMAGES` and `PRODUCED` columns are blank under `--harness opencode` | Working as intended — blank is unmeasured, and printing `0` would be a claim tare cannot support. See [What OpenCode does not record](#what-opencode-does-not-record). |

## Using the skill

This repo ships an agent skill: a short playbook that tells a coding agent how to
drive `tare` — probe the corpus, pick the command, read the output, and turn a
finding into an actionable. It is plain Markdown living at
[`.agents/skills/tare/SKILL.md`](.agents/skills/tare/SKILL.md) (under 500 lines),
with metric vocabulary and per-harness notes in its `references/` folder.

### Install — copy-paste (primary)

From a clone of this repo, link the skill into any project:

```bash
git clone https://github.com/AngelVRodC/tare
cd /path/to/your-project
mkdir -p .agents/skills
ln -s /path/to/tare/.agents/skills/tare .agents/skills/tare
```

Claude Code reads only `.claude/skills/`, so link there too:

```bash
mkdir -p .claude/skills
ln -s ../../.agents/skills/tare .claude/skills/tare
```

The relative form matters: a relative symlink survives moving the project; an
absolute one does not.

### Install — one command

```bash
npx skills add AngelVRodC/tare --skill tare
```

This resolves the tracked `.agents/skills/tare` tree and installs a copy of
it into `.claude/skills/` for the harnesses it detects — a copy, not a
symlink, so it does not follow this repository's future updates; re-run the
command after upgrading.

### Version drift

The skill carries `metadata.version` in its frontmatter. If you upgrade `tare`
and the CLI's output columns or metric names change, re-sync the skill — it
quotes `usage()` strings, and a stale skill quotes stale flags.

### What the skill runs

Every command it prescribes parses against `usage()`:

| Question | Command |
|---|---|
| Which tooling costs the most context bytes? | `tare tools` |
| What share of tokens does a skill/plugin own? | `tare attribute --all` |
| Where are the empty and truncated tool results? | `tare corruption --all` |

Flags go after the command. `--harness` exists on `tools` only; `--top N` and
`--all` exist on `attribute` and `corruption` only. The skill never hardcodes a
percentage: shares change with your corpus, so it reads them from your run.

## Contributing

Issues and pull requests are welcome. The most useful contribution is a
transcript shape this tool gets wrong — Claude Code has written this corpus in
29 versions so far, and the next one will move something again.

If you work with a coding agent, [`AGENTS.md`](AGENTS.md) is the project's decision
record — the reasoning behind the invariants below, the traps that cost a
measurement to find, and the gates a change must not break. `CLAUDE.md` is a
real three-line file whose only directive is `@AGENTS.md`, Anthropic's
documented import syntax, so Claude Code reads the same record under its own
name.

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
| Sniff the shape, never trust a version field | 29 CLI versions wrote this corpus and none of them promised a schema | [How it works](#how-it-works) |
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
