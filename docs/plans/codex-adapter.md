# Codex transcript adapter — implementation plan

Status: initial `tools --harness codex` adapter implemented and verified.
The instruction-rent, usage and additional-command investigations in phase 5
remain future work. Building a checkout binary is sufficient for local use;
global installation is optional.

Add `tare tools --harness codex` as the third reader, retaining the existing
`tools` envelope and renderer. Start with recorded tool-result volume and join
integrity. Extend to instruction rent and token accounting only after separate
measurement contracts are established.

## 1. Establish the development baseline

- Install or expose Go 1.27 or later, matching `go.mod`; do not lower the
  module requirement as a setup workaround.
- Run the repository gates: `go build ./...`, `go test ./...`, `go vet ./...`,
  `gofmt -l .` (empty), and `go list -m all` (only this module).
- Build the checkout with `go build -o tare .`. For agent use outside the
  checkout, use `go install .` and put the effective Go install directory on
  PATH (`GOBIN` when set, otherwise `$(go env GOPATH)/bin`).
- Codex integration uses the existing `.agents/skills/tare/SKILL.md` and local
  shell execution. No MCP wrapper, API key, account connection, or network
  service is required for this reader. SQLite remains an OpenCode requirement.
- Freeze the Codex corpus outside the repository before QA. Keep raw rollouts
  private; commit synthetic fixtures that reproduce shapes, never user logs.

## 2. Pin the input and measurement contract

OpenAI documents active rollouts under `$CODEX_HOME/sessions` and archived
rollouts under `$CODEX_HOME/archived_sessions`, with `~/.codex` as the default
home. Source: [official troubleshooting documentation](https://learn.chatgpt.com/docs/reference/troubleshooting).
These paths establish discovery, not a stable event-schema guarantee.

Implemented path behavior:

- Default to `$CODEX_HOME/sessions`, falling back to `~/.codex/sessions`.
- An explicit `--dir` wins and denotes a recursively scanned rollout directory.
- Active sessions are the default scope. Archives can be read explicitly with
  `--dir`; a snapshot can combine both trees. Document this scope clearly.
- Do not consult authentication files, the prompt history index, a state
  database, or current installed-tool configuration to infer historical use.
- Detect Codex record shapes. Missing, unreadable, empty, and wrong-harness
  corpora must receive distinct, useful diagnostics.

Read-only corpus inspection established these implementation requirements:

- Both `response_item.function_call` / `function_call_output` and
  `custom_tool_call` / `custom_tool_call_output` occur, joined by `call_id`.
- Tool identity can contain separate `namespace` and `name` fields. Preserve
  both; normalize deterministically without collisions and pin the convention
  in fixtures before shipping. MCP server identity must come from an explicit MCP namespace
  or legacy MCP name.
- Outputs occur as strings and arrays of `input_text` / `input_image` blocks.
  Inline images use data URLs. Some text strings themselves contain JSON;
  those strings are delivered text, not permission to discard their wrappers.
- `event_msg.item_completed` includes command, MCP, file-change and other
  execution metadata. Some item IDs match outer calls; others represent
  execution inside an orchestration tool. It is not a second additive stream
  of model-visible results.
- Multiple writer versions, repeated session metadata, subagent/fork metadata,
  and records larger than 8 MB occur. Tolerant structs and `bufio.Reader` are
  required. A file is not automatically a unique session.
- `world_state` contains full and incremental instruction-state records.
  Token data occurs both in `event_msg.token_count` and `token_usage_record`.
  `compacted` records can carry replacement history and usage snapshots.

These observations are evidence for fixtures, not a complete format specification.

| Metric or behavior | Initial contract |
|---|---|
| `calls`, `distinct_tools` | Preserve the existing tools command's completed-result accounting; requested calls have their own join counters. |
| `context_bytes` | Decoded UTF-8 bytes of recorded tool-result text. Exclude JSON serialization overhead, arguments, reasoning, event mirrors and images. Explain that this is recorded result volume, not current context occupancy or billed tokens. |
| `image_bytes`, `image_results` | Measure inline base64 payload bytes separately, matching the existing image-byte unit. Exclude the data-URL prefix. Remote or unknown image forms need explicit unavailability handling. |
| Join counters | Count recognized calls/results, unmatched results and unanswered calls. Scope IDs by proven session/branch identity. Retain enough metadata to resolve joins across time-window boundaries. |
| `mcp_server` | Roll up direct calls only when an explicit namespace/name establishes the server. Do not apply Claude plugin-name rules to Codex connectors. |
| `errors` | Omit with warnings initially unless a complete, tested status mapping is proven for the relevant rows. Missing status is not success; a completed custom call is not necessarily successful execution. |
| `produced_bytes`, `externalised_*` | Omit with warnings until an exact relationship between production and delivered results is established. |
| Skill/plugin rent and token/dollar attribution | Omit with warnings in the initial adapter. Their presence in instructions or metadata does not establish attributable billing. |
| Native search/discovery and unknown output shapes | Detect and warn about unsupported coverage. Never turn an unknown payload into a measured zero or count duplicate event views. |

The critical limit is orchestration: `functions.exec` can execute multiple
tools, transform their results, suppress them, or emit additional output.
Attribute its recorded result to the outer tool. Do not parse arbitrary
JavaScript into executed-call claims, divide bytes among nested tools, or add
MCP execution-event bytes to the outer result. Warn that direct MCP rows do
not cover all nested MCP activity. Any future nested-execution accounting must
use a separately defined dimension and establish its own provenance.

The implemented ownership rule follows `session_meta.id` switches. Exact
JSON payload copies (ignoring whitespace) at the same normalized timestamp
count once within a session. Conflicting copies and reused call IDs across
explicit parent/fork ancestry fail loudly. Replacement history and execution
mirrors are not recursively counted. External parent history is not expanded.

## 3. Implement the narrow adapter

Implemented files and boundaries:

- `internal/codex/`: Codex event shapes, streaming traversal, file/session
  metadata, result decoding and diagnostics. Keep Claude Code's
  `internal/transcript` behavior intact; avoid a speculative adapter registry.
- `internal/report/codex.go`: `CodexToolsEnvelope(dir, version, window)` with
  the same signature as the existing envelope constructors. Reuse metric
  builders, omission support, deterministic ordering and rendering.
- `main.go`: add the `codex` harness, dispatch and path resolution; update
  `usage()`, which is the real flag documentation.
- Tests beside each changed package; CLI tests in `main_test.go`.

Retain metadata and counters, not the corpus or tool output bodies. Bound
per-file/session state where ownership permits. Continue on unknown shapes
with diagnostics; surface read failures and join defects. Do not execute any
code found inside a transcript.

Support the existing `--since` and `--until` flags from the first release.
Join over the whole selected corpus before applying aggregation windows;
report untimestamped events and preserve empty-window behavior. Verify actual
Codex timestamp forms before reusing the current lexical comparisons.

Keep `scan`, `attribute`, `corruption`, and `report` Claude-Code-only for this
release. A new harness does not by itself justify exposing unsupported flags
on those commands. Avoid changing the JSON shape or existing units; if a
change proves necessary, apply the repository's schema-version rule.

## 4. Verify and document

Synthetic fixtures must cover string and block outputs, Unicode and escaping,
inline and remote images, namespaced and legacy names, ordinary and custom
calls, direct MCP calls, orchestration output, missing status, unknown records,
malformed and partial final lines, large records, unanswered and unmatched
calls, repeated metadata, colliding IDs across sessions, forked history,
compaction snapshots and event mirrors.

Pin these acceptance checks:

- Hand-counted expected bytes and calls match the envelope; absent metrics
  stay absent per tool, server and corpus, with explanatory warnings.
- `RenderTools`, JSON and `Envelope.Validate` agree; repeated reads of a
  frozen corpus produce byte-identical JSON.
- Window cuts do not manufacture unmatched joins; date and timestamp bounds
  and empty windows follow the established contract.
- Explicit `--dir`, custom `CODEX_HOME`, missing corpora and unsupported
  command/harness combinations behave as documented.
- Compare an independent local streaming tally with the adapter on the same
  snapshot. Verify bounded memory and unchanged source files.
- Run every repository gate from phase 1, including the existing
  reproducibility and Markdown timestamp tests. Preserve Claude Code and
  OpenCode fixture outputs.

Update `README.md`, `AGENTS.md`, `usage()`, the tare skill and its harness and
metrics references together. Record portable decisions only in tracked files.
The README and skill now also document the existing `--since` / `--until`
flags alongside the Codex addition. Do not edit the three-line `CLAUDE.md` bridge.

## 5. Extend only after the first adapter is trustworthy

Investigate instruction rent from `world_state`, base instructions and actual
delivered messages. Distinguish a state snapshot from an insertion into the
model context, handle full versus delta records, and avoid counting the same
instruction via multiple representations. Skill availability is not skill use.

Investigate usage records independently: distinguish per-response usage from
cumulative turn/thread totals, deduplicate response IDs and compaction copies,
and establish parent/subagent ownership. Observed token counts do not create
per-skill attribution or a dollar bill. Leave unrecorded quantities unavailable.

Consider Codex `scan`, compaction analysis, corruption and composed reports
only after defining their own metric availability. Claude Code's recorded
absence of compaction is specific to that adapter; it must not suppress Codex
events that are actually present.

## Verification record

The repository build, tests, vet, formatting and single-module checks pass.
Synthetic fixtures cover the byte contract, omissions, namespace identities,
large/partial records, session/fork ownership, duplicates and window precision.
Repeated fixture reads produce byte-identical JSON. An independent local tally
matched every emitted tool metric and corpus counter on a frozen rollout
corpus; repeated adapter reads matched byte-for-byte and left corpus files
unchanged. Raw corpus data and machine-specific paths are not stored here.
