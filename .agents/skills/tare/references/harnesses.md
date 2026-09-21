# Per-harness gating

Route on the corpus, not on the harness you believe you are running in. The
corpus that exists decides what may be claimed; see SKILL.md Step 1 for the
probes.

## What each corpus feeds

| Corpus | Path | Commands | `attribution_*` |
|---|---|---|---|
| Claude Code | `~/.claude/projects/**/*.jsonl` | every corpus command, incl. `doctor`'s join | yes |
| OpenCode | `~/.local/share/opencode/opencode.db` | `tools` and `doctor`, via `--harness opencode` | no |

An agent may run under any harness — Claude Code, OpenCode, Codex, Cursor —
and drive `tare` against whichever of these two corpora exists on the machine.
Codex and Cursor keep no corpus tare reads today; their agents simply read
whichever corpus is present. The `attribution_*` dimensions exist only when
the Claude Code corpus exists; the OpenCode database has no per-turn
attribution fields, so `tare tools --harness opencode` produces byte
mechanism tables and nothing else.

## What each harness's `doctor` covers

- **claude-code** (the default): the full static pass — SKILL.md frontmatter
  across the user and project skill roots, plugin manifests (Claude Code is
  the only harness with a manifest layout tare can validate), and MCP
  entries from `settings.json`, `~/.claude.json` (user and project scopes),
  `.mcp.json`, and each installed plugin's own `.mcp.json` — the last read
  via `~/.claude/plugins/installed_plugins.json → installPath` (never the
  plugin cache, which holds superseded versions) and namespaced
  `plugin:<plugin>:<server>`, so a plugin-provided server joins the corpus
  instead of surfacing as unconfigured — plus the cross-join against the
  transcript corpus, which is fatal without one, like every corpus command.
  The join canonicalizes MCP server spellings (`:` `.` space → `_`) so one
  server observed as both `plugin:sre:k8s-qa` and `plugin_sre_k8s-qa` is one
  row. Project scope wins a name collision, and the warning names both files,
  because the shadowed entry is configuration that will never load.
- **opencode**: the same frontmatter rules over the skill roots OpenCode
  itself reads, and MCP entries from the user and project `opencode.json` —
  no plugin block, which is omitted with a warning, and absence is not
  health. The join is name-based over tool names from the database, with
  `tool_sessions` as its denominator (see references/metrics.md); servers
  configured `enabled: false` are excluded from `never_observed` with a
  warning — their silence is the configuration working. A missing database
  is not fatal: it costs only the join, and the config blocks stand alone.

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

Run nothing that reads a corpus for that harness. Fabricate nothing. An absent
corpus supports no metric, no share, and no comparison.
