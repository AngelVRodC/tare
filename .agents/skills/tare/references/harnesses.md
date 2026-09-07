# Per-harness gating

Route on the corpus, not on the harness you believe you are running in. The
corpus that exists decides what may be claimed; see SKILL.md Step 1 for the
probes.

## What each corpus feeds

| Corpus | Path | Commands | `attribution_*` |
|---|---|---|---|
| Claude Code | `~/.claude/projects/**/*.jsonl` | all five | yes |
| OpenCode | `~/.local/share/opencode/opencode.db` | `tools` only, via `--harness opencode` | no |

An agent may run under any harness — Claude Code, OpenCode, Codex, Cursor —
and drive `tare` against whichever of these two corpora exists on the machine.
Codex and Cursor keep no corpus tare reads today; their agents simply read
whichever corpus is present. The `attribution_*` dimensions exist only when
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

## Corpus absent

A harness being installed proves nothing about its corpus. When the probe for
a harness fails, emit exactly one line and claim nothing:

> OpenCode is installed but no corpus was found at
> ~/.local/share/opencode/opencode.db — skipping it and claiming nothing
> about it.

Run nothing for that harness. Fabricate nothing. An absent corpus supports no
metric, no share, and no comparison.
