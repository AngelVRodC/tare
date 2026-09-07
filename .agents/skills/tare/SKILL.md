---
name: tare
description: >-
  Measure what installed tooling actually costs context. Use when asked what a
  tool, skill, plugin, agent, or MCP server is costing the context window;
  which of them is worth keeping; what is eating tokens or bytes; whether a
  skill or MCP server pays for itself; or where transcript bloat comes from.
  Runs the local `tare` CLI over local agent transcripts. No network access;
  nothing leaves the machine.
license: MIT
compatibility: >-
  Requires the `tare` CLI installed and on PATH. The corpus probes and the
  commands below are plain shell and file reads, so any harness that can run
  shell commands and read files can drive this skill. Metric availability is
  harness-gated: read references/harnesses.md before quoting any metric for a
  specific harness.
metadata:
  author: AngelVRodC
  version: "1.0.0"
allowed-tools: Read, Grep, Glob, Bash(tare:*)
---

# tare — what your tooling costs

tare reads local agent transcripts and reports which installed tooling actually
costs context: per-tool byte volume, per-skill/plugin/agent/MCP token and
dollar attribution, and corruption detection. Two attribution mechanisms exist,
and the corpus you find decides which are available. Every number is measured
from the machine's own transcripts — no network, no pricing API, no telemetry.

## Step 1 — probe the corpus

Route on corpus presence, never on harness self-identification. An agent cannot
reliably introspect which harness it is running in, and the gate that matters
is which corpus exists:

```bash
ls -A ~/.claude/projects 2>/dev/null | grep -q .   # Claude Code corpus present?
test -f ~/.local/share/opencode/opencode.db        # OpenCode corpus present?
```

- Claude Code corpus present → all five commands are available, including every
  `attribution_*` dimension.
- OpenCode corpus present → `tare tools --harness opencode` is available.
  OpenCode cannot produce `attribution_*`; see references/harnesses.md.
- Harness installed, corpus absent → emit exactly one line and claim nothing:

  > OpenCode is installed but no corpus was found at
  > ~/.local/share/opencode/opencode.db — skipping it and claiming nothing
  > about it.

  Run nothing for that harness. Fabricate nothing. No metrics for a corpus
  that does not exist.

`attribution_*` instructions are gated on the Claude Code corpus existing —
the gate is the corpus, not the process. An agent running under OpenCode that
finds a Claude Code corpus may legitimately use them.

## Step 2 — choose the command

Flags go after the command, always: `tare <command> [flags]`.

| Question | Command | Output |
|---|---|---|
| How big is the corpus: files, bytes, date range, event types? | `tare scan` | one small table |
| Which tool dominates context bytes? Which tools error? | `tare tools` | one small table |
| Same, over the OpenCode database | `tare tools --harness opencode` | one small table |
| How many tokens and dollars per skill, plugin, agent, MCP server? | `tare attribute` | several tables |
| Is a tool corrupting what it returns: errors, empties, truncation? | `tare corruption` | compact tables |
| Everything composed into one artifact for later reading? | `tare report > report.md` | file, not terminal |

Flag scope (a flag exists only where it means something):

- `--dir <root>` and `--json` are accepted by every command.
- `--harness claude-code|opencode` is accepted by `tools` only; `claude-code`
  is the default.
- `--top N` and `--all` are accepted by `attribute` and `corruption` only.
  `tare scan --all` is an error by design, not a silently ignored flag.

Never open `tare report --json` as a first read: it is the largest envelope the
program produces. Measured sizes and their token estimates are in
references/metrics.md.

## Step 3 — read the output

- Default tables first. The default tables of the four read commands together
  are a few screenfuls (~8k estimated tokens at bytes/4 — the estimate label
  matters; see references/metrics.md).
- `--json` is for machine joins only — scripts, diffs, another tool reading
  the envelope. The JSON envelope carries every row; it ignores `--top`.
- A truncated table names its own escape hatch, in the shape
  `showing top N of M <dimension> rows — use --all`. Quote a share only after
  widening with `--all` (or `--top 0`) when the denominator matters.
- Every metric row is labelled `measured` or `estimated`. A blank cell means
  the harness never recorded that metric — absent is not zero. The full
  vocabulary and the floor rule live in references/metrics.md.

## Step 4 — turn a finding into an actionable

A cost figure alone justifies nothing. An actionable has three parts: the
measured share, the failure rate, and the question only the user can answer.

Refusal shape:

> `<tool>` is N% of your context bytes and errors on M% of its calls.

Then the value question:

> Whether what it returns is worth that is not in this data — read its
> outputs before deciding.

Compute N and M from the envelope in front of you, never from memory: every
recorded share has drifted between runs, so carry the rule, never the number.
On the token-attribution tables, read the `(unattributed)` row first — the
rule and its reason are in references/metrics.md.

## Worked example A — byte attribution (`tare tools`)

Any corpus that records tool names and result sizes.

1. Run `tare tools`.
2. Find the row with the largest `context_bytes` in the `tool` table.
3. N = that row's `context_bytes` divided by the corpus total `context_bytes`
   (the envelope carries the total as a corpus-wide metric). M = that row's
   `errors` divided by that row's `calls`.
4. Deliver the actionable in the refusal shape, then the value question.
5. If the row is an MCP tool, check the `mcp_server` table too — bytes roll up
   per server there. This is the byte mechanism, parsed from the
   `mcp__<server>__<tool>` tool name; it is not the token mechanism.

What not to conclude: a large share is not a verdict. The refusal shape ends
in a question because usefulness is not in the transcript.

## Worked example B — attribution floor (`tare attribute`)

Claude Code corpus only — gate on the Step 1 probe before running.

1. Run `tare attribute`.
2. Read the `(unattributed)` row of the `attribution_skill` table FIRST. It is
   the confidence bound on the whole table: every response no field claimed
   lands there. Never quote a named share without it.
3. Only then read the named rows. Deliver the actionable:

   > `<skill>` is at most X% of attributed turns — the harness tags narrowly,
   > and `(unattributed)` is silence, not zero.

4. Close with the same value question as example A. The floor rule applies to
   the five `attribution_*` dimensions only; the byte tables have no
   `(unattributed)` bucket.

## Links

- [references/metrics.md](references/metrics.md) — metric vocabulary, the two
  attribution mechanisms, the `(unattributed)` floor rule, and the measured
  output-size cliffs. Read before quoting any metric or share.
- [references/harnesses.md](references/harnesses.md) — per-harness corpus
  paths and gating, the OpenCode WAL note, and blank-means-unmeasured. Read
  before claiming anything about a specific harness.
