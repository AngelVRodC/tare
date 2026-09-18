# Per-harness gating

Route on the corpus, not on the harness you believe you are running in. The
corpus that exists decides what may be claimed; see SKILL.md Step 1 for the
probes.

## What each corpus feeds

| Corpus | Path | Commands | `attribution_*` |
|---|---|---|---|
| Claude Code | `~/.claude/projects/**/*.jsonl` | all five | yes |
| OpenCode | `~/.local/share/opencode/opencode.db` | `tools` only, via `--harness opencode` | no |
| Codex | `$CODEX_HOME/sessions/**/*.jsonl`, default `~/.codex/sessions` | `tools` only, via `--harness codex` | no |

An agent may run under any harness — Claude Code, OpenCode, Codex, Cursor —
and drive `tare` against whichever supported corpus exists on the machine.
Cursor remains unsupported as a transcript source. The `attribution_*` dimensions exist only when
the Claude Code corpus exists; the OpenCode database has no per-turn
attribution fields, so `tare tools --harness opencode` produces byte
mechanism tables and nothing else.

## OpenCode notes

- Requires the `sqlite3` binary on PATH. It is invoked as a local binary, not
  a dependency of tare.
- **WAL**: the `-wal` file decides whether the database opens at all; `-shm`
  decides whether reading it writes. When snapshotting a corpus, copy all
  three files — `.db`, `.db-wal`, `.db-shm` — together. With all three present
  nothing is created and read-only media works. Never read `immutable=1`:
  it ignores the WAL and reports a stale number as if it were current.
- Blank means unmeasured: OpenCode records fewer metrics than Claude Code.
  A blank cell in a `--harness opencode` table is an absent metric — named in
  `warnings[]` — never a zero.

## Codex notes

- Reads local JSONL directly; no API key, Codex process, SQLite or MCP wrapper.
- Default scope is active sessions. Use explicit `--dir` for archived sessions
  or a frozen copy; a directory without rollouts is not a usable corpus.
- Joins use `session_meta.id` and `call_id`. Parent and child metadata can
  occur in the same file. Exact duplicate calls/results count once; conflicting
  records and ambiguous inherited IDs fail rather than double-count.
- Recorded result text is decoded UTF-8 bytes. Inline base64 image payloads
  are separate. Unknown payload sizes are omitted at affected tool/server and
  corpus levels; remote image bytes are unmeasured.
- Tool keys use escaped `namespace.name` when a namespace is recorded. A
  literal dot/backslash in a component is escaped; legacy plain names remain.
- Orchestrated results belong to the outer `exec` tool. Nested execution-event
  output is not added again. Direct MCP rows do not measure all nested MCP use.
- Native search/discovery and unknown response-item types produce coverage
  warnings. Execution mirrors and compaction replacement history are excluded.
- Errors, produced/externalised bytes, rent, skill use, plugin attribution and
  token/dollar attribution remain unavailable. Never invent an error rate.
- `--since` / `--until` filter events while joins span the whole selected
  corpus; missing timestamps and empty windows are disclosed in warnings.

## Corpus absent

A harness being installed proves nothing about its corpus. When the probe for
a harness fails, emit exactly one line and claim nothing:

> <harness> is installed but no corpus was found at <expected path> —
> skipping it and claiming nothing about it.

Run nothing for that harness. Fabricate nothing. An absent corpus supports no
metric, no share, and no comparison.
