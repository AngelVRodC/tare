# Metric vocabulary and floor rules

Read this before quoting any metric, share, or size figure from a tare output.
The JSON metric names are the contract: they never move between versions, and
keyed rows print their key verbatim — tool names, session ids, and CLI
versions in the keys are measured data, not labels.

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

## Two mechanisms — never conflate them

1. **Byte mechanism** — the `tools` dimensions `tool` and `mcp_server`.
   Attribution comes from parsing `mcp__<server>__<tool>` out of the tool
   name, so it travels to any harness that records tool names and result
   sizes — OpenCode included.
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

## Output-size cliffs

Measured on the corpus this skill was written against; re-measure before
relying on the byte figures elsewhere.

| Output | Size | Derivation |
|---|---|---|
| `tare report --json` | 829,018 bytes ≈ 207k tokens | bytes: measured; tokens: estimate (bytes/4) |
| `tare report` default Markdown | 105,109 bytes | measured |
| Default tables of the four read commands combined | ~8k tokens | estimate (bytes/4) |

Do not open `tare report --json` as a first read. It is a machine envelope,
not a human document.
