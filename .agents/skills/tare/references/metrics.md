# Metric vocabulary and floor rules

Read this before quoting any metric, share, or size figure from a tare output.
The JSON metric names are the contract: they never move between versions, and
keyed rows render the envelope key verbatim. Codex combines recorded namespace
and tool name with an escaped dot separator; this preserves identity without
conflating tools whose unqualified names happen to match.

## Envelope shape

Every command emits one envelope: `command`, `version`, `metrics[]`,
`warnings[]`. The tables are a rendering of exactly these rows, so a table and
its `--json` can never disagree. `warnings[]` carries caveats — read them
before quoting: a missing source is named there, not silently dropped.

## Row shape

Each metric row carries: `name`, `dimension`, `key` (keyed rows only),
`value`, `unit`, and `derivation` (`measured` | `estimated`). An `estimated`
row also carries the `method` that produced it.

## Keyless dimensions (corpus-wide)

One value per corpus, no key:

- `scan` — file counts, byte volume, date range, per-event-type counts.
- `tools` — totals: `calls`, `context_bytes`, `image_bytes`,
  `produced_bytes`, `errors`, plus block counters (`tool_use_blocks`,
  `tool_result_blocks`, `unmatched_results`, `unanswered_uses`).
- `attribute` — `responses_naive`, `responses_distinct`,
  `responses_duplicate`, `fresh_tokens`, `rebilled_tokens`, `output_tokens`,
  `thinking_tokens`, `rebill_multiplier`, session and cost-state coverage
  counters, `attachment_events`, `attachment_bytes`.
- `corruption` — per-tool error, denial, failure, empty and truncation
  totals. `errors` counts every `is_error` result; `denied` and `failures`
  partition it, so `denied + failures == errors` exactly. A denial is a call
  the harness blocked before the tool ran, which is a policy finding rather
  than evidence the tool altered its output — read `failures` when the
  question is whether a tool is broken. `denial_kind` keys the denials by
  Claude Code's own `toolDenialKind` (`permission-rule`, `user-rejected`).
  Only `corruption` splits them: `tools` reports `errors` alone.
- `failures` — pattern counts: `retry_loops`, `repeated_failure_pairs`,
  `errored_results` (the denominator of the failure-attribution tables), and
  `unmatched_results`. The partition above holds wherever `failures` and
  `denied` are reported: `denied + failures == errors`. A denial still feeds
  *pattern* detection — it cost the agent a turn and returned nothing, which
  is what a loop looks like from the inside — and its share rides in rows of
  its own (`consecutive_denials`, `denied`), never in the failure counts.
- `doctor` — block denominators `skills_checked`, `plugins_checked`,
  `mcp_servers_checked`; join denominators `mcp_sessions` or
  `tool_sessions`. A zero on a `_checked` row is honest only when the block
  was readable; a missing or unreadable source omits the row and says so in
  a warning. `plugins_checked` is claude-only — under `--harness opencode`
  it is absent with a warning, and the absence is not a claim the harness is
  healthy. The keyed rows of both tables are below.

## Keyed dimensions

| Dimension | Command | Mechanism | Unit |
|---|---|---|---|
| `tool` | tools | byte attribution | bytes, calls |
| `mcp_server` | tools | byte attribution (parsed from the tool name) | bytes, calls |
| `attribution_skill` | attribute | token attribution (harness field) | tokens, dollars |
| `attribution_plugin` | attribute | token attribution (harness field) | tokens, dollars |
| `attribution_agent` | attribute | token attribution (harness field) | tokens, dollars |
| `attribution_mcp_server` | attribute | token attribution (harness field) | tokens, dollars |
| `attribution_mcp_tool` | attribute | token attribution (harness field) | tokens, dollars |
| `session` | attribute | token attribution per session id | tokens, dollars |
| `attachment_type` (+ sub-dims `hook_name`, `skill`, `mcp_tool`, `mcp_server`, `agent`) | attribute | bytes of attached context | bytes |
| `retry_loop` | failures | one tool + payload erroring 3+ consecutive times in one session; key `session/tool/payload-digest`, `#N` on repeats | calls |
| `repeated_failure` | failures | one tool + payload erroring across 2+ sessions; key `tool/payload-digest` | calls, sessions |
| `attribution_skill`, `attribution_plugin`, `attribution_mcp_server` | failures | errored calls rolled up by the calling turn's harness field — the same dimension names `attribute` emits, a different measure: counts, not tokens | calls |
| `skill`, `plugin`, `mcp_server` | doctor | static-config findings, keyed by absolute skill path, plugin directory, or server name; the findings kinds are in the warnings | findings |
| `mcp_server` | doctor | the cross-join misses: `never_observed` (value = the join denominator) and `unconfigured_observed` (value = the sessions that carried it) | sessions |

## Two mechanisms — never conflate them

1. **Byte mechanism** — the `tools` dimensions `tool` and `mcp_server`.
   Attribution uses the harness's tool identity: Claude Code's legacy
   `mcp__<server>__<tool>`, OpenCode's configured server prefixes, or Codex's
   explicit MCP namespace/legacy name. No token attribution is implied.
2. **Token mechanism** — the `attribute` dimensions `attribution_skill`,
   `attribution_plugin`, `attribution_agent`, `attribution_mcp_server`,
   `attribution_mcp_tool`. These read Claude Code's proprietary per-turn
   attribution fields; only the Claude Code corpus produces them.

`mcp_server` (bytes, from tool names) and `attribution_mcp_server` (tokens,
from harness fields) are different tables measured by different mechanisms.
Never cite one as the other.

## The floor rule — five `attribution_*` dimensions only

- **Read the `(unattributed)` row first.** It is the confidence bound on the
  whole table: every response no attribution field claimed lands there. Past a
  large `(unattributed)` share, the named rows own a minority of the tokens,
  and every dollar figure beside them is a floor, not a total.
- **`(unattributed)` is silence, not zero.** It means the harness wrote no
  field on that turn — never that tare failed to attribute. It is never
  redistributed across the named rows.
- **Never quote a named share without the `(unattributed)` share beside it**
  (read-the-share-first). A share quoted alone overstates what is known.
- **Byte dimensions have no `(unattributed)` bucket.** A result's tool name is
  always known; an unknown one is a reported defect (`unmatched_results`
  plus a warning), not a bucket. The same holds for the `session` dimension:
  its key is the raw session id, so it has no `(unattributed)` row.
- **No hardcoded shares.** Every recorded share has drifted between runs.
  Quote the measured share from the run in front of you; carry the rule, never
  the number.

## What the debugging rows can and cannot claim

- **`never_observed` is a floor, not a verdict.** The row says: no in-window
  session named this configured server. That is as consistent with an idle or
  newly added server as with a broken one — and when no session used MCP at
  all, the row is not emitted, because an absence measured against nothing
  claims nothing.
- **The join denominators differ by harness.** Claude Code counts sessions
  carrying any MCP activity (`mcp_sessions`); OpenCode cannot tell an MCP
  call from a built-in one, so it counts sessions carrying any tool call
  (`tool_sessions`) — the wider floor, and the warning says so.
- **`unconfigured_observed` keys are harness-dependent.** Claude Code keys
  server names the transcripts carried and no config named. OpenCode has no
  attribution fields, so its join is name-based over observed tool names:
  built-in tools, plugin-provided servers, and config scopes tare does not
  read all arrive there. Either way it is a join miss, never a claim the
  entity is misconfigured.
- **`command_not_found` measures the running tare process's PATH.** The
  harness may launch servers with a different environment, so the claim is
  "does not resolve here", not "cannot launch at all" — the warning names
  the caveat beside the finding.
- **Failure rows report patterns, not causes.** The payload digest in a key
  means semantically identical calls, not byte-identical ones; which tooling
  is at fault is the question the next agent answers, and the
  failure-attribution rows carry only what the harness's own fields named.

## Derivation semantics

`measured` means the figure is derived from transcript rows. `estimated` means
a documented derivation sits between the transcript and the figure, named in
the row's `method` — token estimates are bytes/4. The envelope contract is
enforceable in one direction only: an `estimated` row without a `method` is
rejected, but a `measured` claim is accepted unconditionally. An over-claim
ships silently, so treat any figure not obviously derived from the transcript
as needing provenance before you repeat it.

## Absent is not zero

A metric the harness never recorded is omitted from the envelope and named in
a warning; the table renders that cell blank. Emitting `0` would be a
`measured` claim — a blank is the honest form. When a table looks thinner than
expected, check `warnings[]` before assuming the tool undercounted.

## Codex measurement limits

`context_bytes` measures recorded outer function/custom-tool result text, not
current context occupancy or billed tokens. `image_bytes` measures inline
base64 payloads separately, without data-URL prefixes. `calls` counts matched
results; requested and unanswered calls have separate join counters.

An unknown output form omits affected tool/server/corpus totals with warnings.
Other fully measured tools retain their metrics, but an absent corpus total
cannot support percentages. Remote image bytes are unavailable even when the
image-result count is known. Errors, production/externalisation, rent, skill
use and plugin attribution are omitted in this adapter.

Nested tools inside `exec` do not inherit a share of its output bytes. The
outer tool owns those recorded bytes; direct MCP rows exclude nested activity.
Native/unknown response-item records are excluded with coverage warnings.
Never interpret an omitted error metric as a zero error rate.

## Output-size cliffs

Measured on the corpus this skill was written against; re-measure before
relying on the byte figures elsewhere.

| Output | Size | Derivation |
|---|---|---|
| `tare report --json` | 829,018 bytes ≈ 207k tokens | bytes: measured; tokens: estimate (bytes/4) |
| `tare report` default Markdown | 105,109 bytes | measured |
| `tare failures --json` | 44,907 bytes ≈ 11k tokens | bytes: measured; tokens: estimate (bytes/4) |
| `tare doctor --json` | 12,641 bytes ≈ 3.2k tokens | bytes: measured; tokens: estimate (bytes/4) |
| Default tables of the four read commands combined | ~8k tokens | estimate (bytes/4) |

Do not open `tare report --json` as a first read. It is a machine envelope,
not a human document.
