# AGENTS.md

Guidance for any coding agent working in this repository.

`AGENTS.md` is the canonical file. `CLAUDE.md` is a symlink to it, because Claude Code reads
`CLAUDE.md` and not `AGENTS.md`; OpenCode, Codex and Cursor read `AGENTS.md` directly, and
OpenCode falls back to `CLAUDE.md` only when `AGENTS.md` is absent. **Edit `AGENTS.md`, never
`CLAUDE.md`** — Claude Code reads through the symlink but refuses to write through it
(`anthropics/claude-code#66559`, open), so an edit aimed at `CLAUDE.md` fails with
`Refusing to write through symlink`. Machine-specific notes go in `CLAUDE.local.md`, which
stays gitignored.

`README.md` is for humans and is the user-facing contract; this file is the decision record.
Where they overlap — the invariants, the verification commands — `README.md` carries the
public wording and this file carries the reasoning.


## Repository state

Shipped and working. All five subcommands are implemented, two harnesses are read, `go.mod`
is at Go 1.27, and there are zero dependencies. Never record commit, file or test **counts**
here — they are wrong within the session that writes them. Ask git and go instead.


## What tare is

A zero-dependency Go CLI that reads local agent transcripts and reports which installed
tooling actually costs context: per-tool byte volume, per-skill/plugin/MCP attribution,
and corruption detection. Claude Code (`~/.claude/projects/**/*.jsonl`) feeds all five
commands; OpenCode (`~/.local/share/opencode/opencode.db`) feeds `tools` only.

The premise is that Anthropic's OTel export redacts third-party plugin and skill names to
`"third-party"` and user MCP servers to `"custom"`, while the un-redacted attribution sits
in the local `.jsonl`. Reading it is the product.

**tare is harness-agnostic by design.** The v1 release gate was two working adapters, on the
argument that no harness vendor will ever measure a competitor. OpenCode shipped 2026-09-06
and met it.

**tare names no third-party tool.** Not Boost, not rtk, not Caveman. Detecting what a user
has installed belongs to the LLM driving the CLI, because a compiled-in list is closed-set
and cannot be agnostic to a tool it does not know about. The surface that broke this rule was
deleted in v0.1.0; `runCmd`, the one helper the OpenCode reader needed from it, now lives in
`internal/report/exec.go`.

## Commands

```bash
go build ./...                          # compile check
go build -o tare .                      # build the CLI — NOT `-o tare ./...`, which fails:
                                        # "cannot write multiple packages to non-directory tare"
go test ./...                           # all tests
go test -run TestScanCorpus ./internal/transcript/   # one test
go vet ./...
gofmt -l . | tee /dev/stderr | wc -l    # format check, expect 0
go list -m all | wc -l                  # dependency count, expect 1
```

CLI surface: `scan`, `tools`, `attribute`, `corruption`, `report`. Flags go **after** the
command. `--dir` and `--json` are global; `--harness` is `tools` only; `--top N` and `--all`
are `attribute` and `corruption` only. A flag is registered only where it means something, so
`tare scan --all` is an error rather than a flag that silently does nothing. `--version` and
`--help` are bare words matched before `fs.Parse`, not registered flags, which is why
`tare scan --version` errors too.

**`usage()` is the only flag documentation a user ever sees.** `main.go` calls
`fs.SetOutput(io.Discard)` and never calls `fs.PrintDefaults()`, so every description string
passed to `fs.String`/`fs.Bool`/`fs.IntVar` is **dead text**. Reasoning about what `flag`
auto-prints — its `(default X)` suffix, its column layout — aims at output nobody reads.
When the README has to match the CLI, match the string in `usage()`.

`--dir` is registered with an empty default because the real default depends on `--harness`,
parsed in the same pass, and `resolveDir` applies it afterwards. Side effect: `tare scan
--dir=` (explicit empty) resolves to `~/.claude/projects` where it previously errored.
Harmless, but no test pins it.

**QA against a frozen corpus, never the live one.** The live corpus grows while you work —
this session writes to it — so before/after diffs are meaningless against it. Copy it once
and point `--dir` at the copy.

## Architecture

Module `github.com/AngelVRodC/tare`, Go 1.27, single `main.go` with `flag`-based subcommand
dispatch. `main` package at the root, `internal/` only — no `cmd/`.

```
main.go                 subcommand dispatch, underHome, resolveDir, usage, isTTY
internal/transcript/    parsing: event.go, scan.go, blocks.go, usage.go, persisted.go, attachment.go
internal/report/        metrics: scan, tools, attribute, cost, rebilling, corruption, json, artifact, exec
internal/report/opencode.go   the second adapter: sqlite3 rollup → the same `tools` envelope
```

There is **no adapter interface and no registry**. `case "tools"` picks between
`report.ToolsEnvelope` and `report.OpenCodeToolsEnvelope` — two functions with the same
signature — and hands either result to the same `RenderTools`. Both emit `command: "tools"`,
so the JSON contract does not fork per harness. `internal/transcript` was deliberately *not*
renamed to `internal/adapter/claudecode`: zero behaviour change.

Every renderer reads only the envelope, so a table and its `--json` can never disagree.
`BuildReport` runs the four passes sequentially and `merge`s them, prefixing each warning
with its command; it is deliberately not fused (see the comment at the top of `artifact.go`).

## Design constraints (from the plan, not preferences)

These are load-bearing. A tool arguing "your tooling costs more than it returns" ships with
zero dependencies or it argues against itself.

- **Stdlib only.** `encoding/json`, `bufio`, `os`, `os/exec`, `text/tabwriter`, `flag`. No CLI
  framework, no table library. Adding a module requires proving stdlib insufficient in the
  plan first. `sqlite3` is a **local binary invoked**, not a module — a distinction the README
  states explicitly, because conflating the two is how a zero-dependency claim starts lying.
- **`bufio.Reader`, never `bufio.Scanner`.** The longest measured line is 514,199 bytes;
  Scanner's 64 KB default fails with `token too long`, and any cap is a knob to re-tune later.
- **Stream, never load.** The corpus is hundreds of megabytes and grows daily. Never hold it
  in memory.
- **Sniff shape, never trust a version field.** Tolerant structs with `json.RawMessage` for
  variable regions. The corpus spans dozens of CLI versions and 20+ top-level `type` values.
- **Fail loudly on the join, quietly on the unknown.** An unmatched `tool_use_id` is a
  reportable defect. An unknown event `type` is a counter, never fatal.
- **Every metric carries a `Derivation` field** (`measured` | `estimated`). A block whose rows
  all agree renders it once as a footer, not as a column on every row; `--json` tags every row.
  **The contract is enforceable in one direction only**: `Envelope.Validate` accepts `measured`
  unconditionally and rejects only an `estimated` row with no `Method`. An over-claim ships
  silently, so any figure not derived from the transcript needs its provenance checked against
  the source's own documentation and asserted by a test.
- **Two attribution mechanisms exist, and only one is portable.** Token and dollar attribution
  (`internal/report/attribute.go`) reads Claude Code's proprietary per-turn `attributionSkill` /
  `attributionPlugin` / `attributionAgent` / `attributionMcpServer` / `attributionMcpTool`
  fields; the unit is one assistant turn, and `(unattributed)` means the harness wrote no field
  on that turn — never that tare failed to attribute. Byte attribution (`tare tools`) parses
  `mcp__<server>__<tool>` out of the tool name via `mcpServer` and travels to any harness that
  records tool names and result sizes. Do not conflate them when planning an adapter.
  Mechanism 2 has two implementations; mechanism 1 has one. The MCP split did *not* travel:
  OpenCode joins server and tool with a single `_` and tool names contain `_`, so `mcpServer`
  cannot be reused and the server list must come from `~/.config/opencode/opencode.json`,
  matched longest-name-first.
- **Absent is not zero, and the adapter is where that rule earns its keep.** A metric the
  harness never recorded is omitted from the envelope and named in a warning; `formatValue`
  renders an omitted metric as a blank cell. Emitting `0` would be a `measured` claim that
  `Envelope.Validate` waves through — that is the whole failure mode. `statMetrics(dimension,
  stats, omit ...string)` leaves a row out; `dropUnknownErrors` leaves a per-key row out.
- **Two SQLite traps, both of which produce a wrong number rather than an error.**
  - **Bytes.** A byte figure read through `sqlite3` must use `length(CAST(x AS BLOB))`. Bare
    `length()` counts **characters** for a text value and the result gets labelled bytes —
    measured 0.26% low on the OpenCode corpus. Use `CAST`, not `octet_length()`, which is
    correct but arrived in SQLite 3.43.0.
  - **NULL.** `sum()` over a group whose rows are all NULL returns SQL **NULL**, and
    `encoding/json` decodes `null` into a non-pointer numeric as a silent **0**. This is
    general, not an OpenCode quirk: any SQL aggregate decoded into a non-pointer Go numeric can
    turn *absent* into a *measured zero*. Decode into a **pointer** and let `nil` mean absent.
    `coalesce(...,0)` is **not** the fix — it moves the lie into the database, where nothing
    downstream can tell it from a real zero.
- **WAL: `-wal` decides whether the OpenCode DB opens; `-shm` decides whether reading writes.**
  A read-only open fails with `unable to open database file (14)` without `-wal`, and `-shm`
  is neither necessary nor sufficient — SQLite builds `-shm` itself when missing, which is why
  a `-readonly` read needs a **writable directory** in that case. `.db` and `-wal` bytes are
  never touched. Snapshot all three files: with all three present nothing is created and
  read-only media works. `file:...?immutable=1` is **deliberately not used** — it ignores the
  WAL, trading a loud failure for a stale number. `openCodeFailureCause` in `opencode.go`
  encodes this; the full four-combination measurement is in the OpenCode adapter plan.
- **The JSON name is the contract.** `internal/report/scan.go` holds a `labels` map that is the
  human half of it — a rendering concern only. Metric names never move, keyed rows print their
  key verbatim (tool names, CLI versions, session ids are measured data), and an unmapped name
  falls through unchanged, so the map is never required to be complete.
- **No network.** No pricing API, no telemetry, no update check. Transcript content never
  leaves the machine.
- One file is not one session: subagent transcripts share the parent `sessionId` with their
  own `agentId` and `isSidechain: true`.
- Do not read `~/.claude/stats-cache.json` for cost — every `costUSD` in it is zero. Use
  `cost-state` events.

## Gates that must not break

- `TestReportReproducible` — 8 runs over one corpus must produce byte-identical JSON. It
  caught a real defect: allocation summed float shares while ranging over a map, and Go
  randomises map iteration, so two runs disagreed in the last bit of every dollar figure.
  **Never sum floats over a map range, and never build a rendered string from map order.**
  Range a declared slice, or sort with a total order including a tiebreak.
- `TestReportMarkdownVariesOnlyByTimestamp` — two renders of one report must differ on
  exactly one line (`| generated |`) and have equal line counts. Progress output, debug
  prints and clock reads must never reach the artifact writer.
- `TestValidateRejects` — the envelope contract. Derived facts belong in a warning or a
  rendered sentence, never as a second metric row saying what an existing row already says.

## Rendering rules learned the hard way

- **A trailing annotation line under a `tabwriter` table must carry no tabs.** A
  tab-terminated first cell joins the column block above it and stretches that column to
  the annotation's full width. This silently widened column 0 in six tables until it was
  found by diffing rendered output against an older binary. No test catches it.
- **Truncation is a terminal convenience.** `--top N` caps terminal tables (`0` = every row,
  which is what `--all` means); the Markdown `tare report` never truncates, because a file
  is not a terminal and `--all` is not spellable after the fact by whoever reads the file.
  A cut table names its own escape hatch: `showing top 15 of 39 attribution_skill rows — use --all`.
- **One TTY check exists**, `isTTY` in `main.go`, and it gates the `report` progress line and
  nothing else. It stats **stderr**, not stdout, because stderr is where the progress goes —
  gating on stdout made `tare report > out.md` silent at a real terminal, which is the exact
  hang the line exists to prevent. Note `/dev/null` is a character device.
- No colour, no ANSI, no `NO_COLOR` chain, no pager, no terminal-height sizing. All decided
  and rejected; `tare report | less` already works.


## Go skills installed for this project

Eight skills from `samber/cc-skills-golang` (MIT) live in `.claude/skills/`: `golang-cli`,
`golang-context`, `golang-dependency-injection`, `golang-design-patterns`, `golang-lint`,
`golang-naming`, `golang-safety`, `golang-stay-updated`.


Two declare network access in `allowed-tools`: `golang-stay-updated` (`WebFetch`, `WebSearch`)
and `golang-dependency-injection` (`WebFetch`, `mcp__context7__*`). The other six are limited
to filesystem plus `Bash(go:*)`, `Bash(golangci-lint:*)`, `Bash(git:*)`.

**`.claude/skills/` is not Claude-Code-only — no second copy is needed.** OpenCode reads
`.claude/skills/<name>/SKILL.md` as one of its six documented skill locations, and Cursor
reads it for compatibility too, so all eight are already available there.

Verify it by asking OpenCode, not by reading its docs and not by grepping its binary for
path strings — a compiled-in string proves nothing about whether discovery is active:

```bash
opencode debug skill    # JSON; each entry carries "name" and "location"
```

From this repository root on OpenCode 1.18.29 that lists all eight, each with a `location`
under `<repo>/.claude/skills/`, among 35 skills total. Parse the JSON to count — the output
embeds newlines inside `content` strings, so `rg -c` on it returns a wrong number.

Claude Code is the one harness that reads **only** `.claude/skills/` — it does not scan
`.agents/skills/`, and the request to add it was closed `not_planned`
(`anthropics/claude-code#66352`). So `.claude/skills/` is the path with the widest reach for
the two harnesses tare actually reads, and duplicating into `.agents/skills/` would buy Codex
at the cost of two copies to keep in sync. Note the `allowed-tools` values are Claude Code
syntax; other harnesses parse `name` and `description` and are not required to honour them.

`golang-project-layout` was installed and then **deliberately removed — do not re-add it.**
It mandates that all `main` packages live in `cmd/`, re-opening a settled decision, and its
init checklist writes an always-load directive into this file with "no user confirmation
needed". Three of the remaining eight still cross-reference it; those pointers dangle by design.

## Working with orchestrated sub-agents

A phase-running sub-agent owns the working tree for its files. It does **not** own the
repository history: no `commit`, `reset`, `revert`, `checkout`, `stash`, `add`, `push`.
Commits appearing mid-phase belong to the orchestrator and are expected — a sub-agent that
"cleans one up" destroys verified work. Report anything that looks wrong; never repair it.