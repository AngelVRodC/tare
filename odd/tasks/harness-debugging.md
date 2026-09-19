# ODD — tare harness-debugging transformation

## Objective

Transform tare from a pure cost meter into a harness diagnostic tool: detect
failure patterns, attribute them to the installed tooling that caused them, and
validate harness configuration — while keeping every tare invariant.

## Problem / why

tare already counts per-tool errors, denials and truncations (`corruption`) and
attributes turns to skills/plugins/MCP (`attribute`), but nothing joins them or
detects *patterns*: retry loops, the same failing call repeated across sessions,
failures concentrated in one skill, silent plugin failures invisible in
transcripts. Research (Argus rule analyzer, AgentDebugX arXiv:2607.18754
Detect→Attribute→Recover loop, MCP troubleshooting guides) says the value is in
pattern detection + attribution evidence, with root-cause judgment left to the
LLM driving the CLI — consistent with tare's "names no third-party tool" rule.

## Authorized scope (user chose: all three slices, failures first)

1. `tare failures` — transcript pattern detection + attribution (slices FD-1, FD-2).
2. `tare doctor` — read-only harness config health checks (DR-1, DR-2).
3. Skill upgrade — Detect→Attribute→Propose loop in the tare skill (SK-1).

Out of scope unless re-authorized: modifying existing commands' behavior, new
dependencies, network access, fixing the user's harness itself.

## Constraints (load-bearing, from AGENTS.md)

- Stdlib only; `go list -m all | wc -l` stays 1.
- `bufio.Reader` streaming; never load the corpus; bounded memory per finding.
- Absent ≠ zero: unrecorded metrics are omitted + warned, never `0`.
- Every metric carries a `Derivation`; estimated rows need a `Method`.
- Deterministic render: never sum floats or order rows by map iteration;
  `TestReportReproducible`-style byte-identical `--json` applies to new envelopes.
- JSON name is the contract; `usage()` is the only flag docs users see.
- Envelope must pass `Envelope.Validate`.

## TDD mode

- Resolved: **off** (source: no strict-TDD config or user request found;
  `mem_search` for testing config returned nothing).
- Runner for ordinary checks: `go test ./...`.

## Route

Delegated direct (writer trigger: each slice touches 2+ non-trivial files).
No SDD artifacts. One bounded writer per task.

## Delivery

- Forecast: >400 authored lines total across the feature → strategy
  **ask-on-risk** (default); each task sized to its own work-unit commit on
  `feat/harness-debugging`. Chain strategy asked once at PR planning, not now.
- Native review candidate = each work-unit commit; `gentle-ai review mode status`
  decides whether review runs (off = ordinary policy, no ceremony).

## Tasks

- [x] FD-1 — `tare failures` core: envelope builder `FailuresEnvelope` in
      `internal/report/failures.go` + fixture tests. Patterns (each a keyed
      metric block): (a) retry loop — identical tool + identical call payload
      failing ≥3 consecutive times within one session; (b) repeated failure —
      same tool+payload erroring across ≥2 distinct sessions; (c)
      attribution — error counts rolled up by attributionSkill /
      attributionPlugin / attributionMcpServer (denied calls excluded, reusing
      the corruption split). Sort orders total (count desc, key asc).
      Checks: `go build ./... && go vet ./... && go test ./... && gofmt -l .` → 0.
      Route: delegated writer. Commit: `feat: detect failure patterns in a new failures envelope`
- [ ] FD-2 — `tare failures` CLI: `main.go` dispatch, `usage()` line,
      `RenderFailures` table (`--top/--all` like corruption), `--json`, wire
      into nothing else (no `report` merge — deferred). Docs: README + AGENTS.md
      command list. Checks: build/vet/test/gofmt + manual run vs frozen corpus copy.
      Route: delegated writer. Commit: `feat: wire the failures command into the CLI`
- [ ] DR-1 — `tare doctor` core: `internal/report/doctor.go` — validate
      plugin.json / skill SKILL.md frontmatter (name/kebab, parseable
      allowed-tools), MCP server configs from settings/.mcp.json, flag
      servers configured but never seen in the corpus (silent connection
      failure) by cross-joining the transcript server list. Read-only;
      stdlib-only minimal YAML (key: value lines) is sanctioned. Checks as FD-1.
      Route: delegated writer. Commit: `feat: validate harness configuration in a new doctor envelope`
- [ ] DR-2 — `tare doctor` CLI wiring + renderer + docs. Checks as FD-2.
      Route: delegated writer. Commit: `feat: wire the doctor command into the CLI`
- [ ] SK-1 — `.agents/skills/tare/SKILL.md`: add the failure-diagnosis loop
      (run `failures` → attribute → `doctor` → propose harness fix; judgment
      stays with the agent). Checks: frontmatter valid, `opencode debug skill`
      still lists it. Route: direct inline (1 mechanical file). Commit:
      `docs: teach the tare skill the detect-attribute-propose loop`

## Progress / evidence

- 2026-09-19: scope authorized (all three, failures first). Branch created.
- FD-1 done — commit `f70cd3b` (1052 lines, 3 files). Checks: build/vet/gofmt/
  `go test ./...` green (writer + parent spot-check), `go list -m all` = 1.
  Review: assessed `medium` (slice_budget_reached) → consent granted →
  `review-reliability` capture refused twice by the OpenCode review transport
  (`opencode_review_transport_binding_invalid`, session root == repo root).
  Reported as one occurrence comment on gentle-ai#4030 (canonical open tracker,
  reproductions through 3.4.0 stable); comment `#issuecomment-5744957636`.
  Resolved via the exact candidate-scoped decline (`declined_this_candidate`).
  **FD-1 review outcome: unavailable (provider defect) — no PASS claimed.**
  Review boundary for the next commit: `main` (this candidate burned
  unreviewed-by-defect, not reviewed).

## Next step

Confirm chain strategy (running count 1052 > 400), then delegate FD-2.
