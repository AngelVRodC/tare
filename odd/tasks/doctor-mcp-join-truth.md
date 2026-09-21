# ODD — doctor MCP join truth (canonical spellings + plugin-provided servers)

## Objective

Make `tare doctor`'s MCP join report the truth on a real machine: one row per
distinct server instead of one per spelling, and no `unconfigured_observed`
false positive for a server that a *installed plugin* declares.

## Problem / why

Two independent defects inflate `unconfigured_observed` from 6 genuine findings
to 24 rows on this machine.

**Bug 1 — one server, two spellings, two rows.** `doctorJoin`
(`internal/report/doctor.go:228`) feeds `see()` from two channels that spell the
same server differently:

- `ev.AttributionMcpServer` → `plugin:sre:k8s-qa`, `claude.ai Notion`
- `mcpServer(u.Name)` (`internal/report/tools.go:287`) → `plugin_sre_k8s-qa`,
  `claude_ai_Notion`

The `mcp__` tool name already encodes `:`, `.` and space as `_`. So the same
server splits its session count across a pair of rows and neither row is the
truth (`claude.ai Slack` 2 sessions + `claude_ai_Slack` 3 sessions).

**Bug 1 sibling surface (found while verifying, same root cause).** `tare tools`'
`MCP_SERVER` table splits the same way, because attachment rent keys keep the
harness's own spelling while call rows use the tool-name spelling. Measured on
this machine (2026-09-20, live corpus):

```
claude_ai_Notion        48 calls   257.1 kB context   RENT ABSENT
claude.ai Notion         0 calls     0 B   context   421.5 kB rent
claude.ai Claude Docs    0 calls     0 B   context   111.7 kB rent
plugin:github:github     0 calls     0 B   context    36.5 kB rent
```

`plugin:github:github` stays in colon form because `rentSegment` only rewrites
colon form when the plugin name is in `enabledPlugins`, and `github` is
installed but **not enabled**. Fixing only `doctorJoin` would leave this broken.

**Bug 2 — doctor never reads plugin-provided servers.** `doctorMCP`
(`internal/report/doctor.go:649`) reads four sources: `<claudeRoot>/settings.json`,
`<projectDir>/.mcp.json`, `~/.claude.json` top-level `mcpServers`, and
`~/.claude.json projects["<abs projectDir>"].mcpServers`. Plugin servers live in
`<installPath>/.mcp.json` and are named `plugin:<plugin>:<server>`, so every
`plugin:sre:*`, `plugin:context7:*`, `plugin:github:*` row is a false positive.
The warn text already apologises for this ("plugin-provided servers ... arrive
here too"). Prose is not a fix; the file is right there.

## Verified evidence (measured 2026-09-20, this machine)

`~/.claude/plugins/installed_plugins.json` — `version: 2`,
`plugins: { "<plugin>@<marketplace>": [ {scope, installPath, version, ...} ] }`,
34 plugin keys, 38 entries (3 plugins have 2–3 entries; `user` 26 / `project` 12).

- 7 installed plugins have `<installPath>/.mcp.json`; 6 parse and declare
  **12 servers** under `mcpServers`: `figma`(2), `context7`(1), `notion`(1),
  `data@crabi`(2: openmetadata, redshift), `sre@crabi`(6: grafana-qa/-prod,
  jaeger-qa/-prod, k8s-qa/-prod).
- `github@claude-plugins-official` `.mcp.json` uses a **top-level shape with no
  `mcpServers` wrapper**: `{"github": {"type":"http", ...}}`. This shape IS
  loaded by the harness — the corpus carries `plugin:github:github` rent
  (36.5 kB) and nothing else declares that name. **So the reader must sniff both
  shapes**; reading only `mcpServers` would leave `plugin:github:github` a false
  positive, which is the exact defect being fixed. 18 of 45 cached `.mcp.json`
  files use the top-level shape (all of them `github`, across stale versions).
- `financial-analysis@claude-for-financial-services` `.mcp.json` is **malformed
  JSON** — a missing comma before the `"box"` entry (line 47). It is an
  installed plugin, so under doctorMCP's standing posture ("a source that exists
  but cannot be parsed sets the broken floor and says so") it warns and makes the
  block partial. That is a real, actionable finding: the harness cannot load
  those ~11 servers either.
- **The cache walk is wrong and must not be used.** `~/.claude/plugins/cache/**`
  holds superseded version dirs. `engram@engram` is installed at **0.1.3**, whose
  installPath has **no** `.mcp.json` (only `hooks`, `scripts`, `skills`), while
  the stale **0.1.2** dir still has one declaring `engram`. A cache walk would
  join `plugin:engram:engram` and swallow a genuine finding. Only
  `installed_plugins.json` → `installPath` is correct.

### Not bugs — these rows must survive the fix

| Row | Why it persists |
| --- | --- |
| `plugin:engram:engram` | **Not a removal** (user correction 2026-09-20): engram is installed and useful. The installed 0.1.3 ships no `.mcp.json`; the corpus rows are from 0.1.2, when the plugin delivered the server. The same server is now user-configured and joins as bare `engram` (1149 attribution events vs 702 for the plugin spelling). Mechanism change, so the plugin spelling is history. |
| `boostgraph` | Genuine removal — absent from `installed_plugins.json` and from every config tare reads. |
| `plugin:playwright:playwright` | Genuine removal — `playwright` is not in `installed_plugins.json`. (`stack` is not installed either; the corpus carries no `plugin:stack:*` or `plugin:ui-ux-pro-max:*` row.) |
| `claude.ai Notion`, `claude.ai Slack`, `claude.ai Claude Docs` | Server-side connectors. No local config exists to join against, ever. |
| `twilio-docs` | Project-scoped in `/Users/darlene/Documents/crabi/twilio-plugins`; doctor reads only the current project's scope while the corpus spans every project. |

### Out of scope (explicitly not authorized)

- `doctorPlugins` (`doctor.go:544`) counts top-level entries of
  `~/.claude/plugins` — `cache`, `data`, `desplega`, `marketplaces`, `synced` —
  so `plugins_checked 5` is not a plugin count. User marked it "unrelated,
  noticed, untouched".
- Reading `projects[*].mcpServers` home-wide to kill `twilio-docs`.
- A separate classification for the unjoinable `claude.ai *` connectors.
- Modifying the user's harness, new dependencies, network access.

## Constraints (load-bearing, from AGENTS.md)

- Stdlib only; `go list -m all | wc -l` stays 1.
- Stream, never load; `bufio.Reader` not `Scanner`.
- **Absent is not zero**: an unreadable source warns and withholds the count;
  it never becomes `0`.
- Deterministic render: never order rows or build rendered text from map
  iteration. `TestReportReproducible` and
  `TestReportMarkdownVariesOnlyByTimestamp` must stay green.
- The JSON metric names are the contract: `mcp_sessions`, `never_observed`,
  `unconfigured_observed`, `findings`, `mcp_servers_checked` do not move.
- **Canonicalization is mcp_server-only.** Skill rent keys legitimately contain
  `:` (`desplega:feedback`, `frontend:a11y-audit`, `sre:investigate-alert`,
  `ponytail:ponytail-review`) and skill call keys come from the `Skill` tool
  input verbatim. Canonicalizing the skill or plugin dimension would merge
  distinct real entities and break the rent↔calls join. Never route
  `skillRent`/`skillCalls`/`pluginRent` through the canonicalizer.
- Do not canonicalize inside `mcpServer()` itself: `pluginServer`/`rentSegment`
  match `plugin_<name>_` prefixes against raw plugin names from the authority,
  and a plugin name containing `.` or `:` would stop matching.
- QA against a **frozen** corpus copy, never the live one (it grows during the
  session). `du -sh ~/.claude/projects` = 718 MB.

## TDD mode

- Resolved: **off**. Source: existing project configuration —
  `odd/tasks/harness-debugging.md` records "Resolved: off (no strict-TDD config
  or user request found)"; nothing since changed it and the user requested none.
- Runner for ordinary functional checks: `go test ./...`.

## Route

Delegated direct. Writer trigger fires: T1–T3 touch `internal/report/doctor.go`,
`internal/report/tools.go`, a new plugin-config reader, and their tests. One
bounded writer per task, exploration folded into the writer that needs it.
No SDD artifacts.

## Delivery

- Strategy: `ask-on-risk` (default). Forecast: **~380 authored changed lines**
  (canon helper ~15, doctorJoin restructure ~30, plugin MCP source ~90, tests
  ~200, docs ~45). Tests are the bulk.
- If the running count passes ~400 at any task boundary, stop and apply
  `ask-on-risk` (slice vs. `size:exception`) before the next commit.
- One work-unit commit per task on a feature branch, Conventional Commit
  message, tests and docs alongside the behaviour.

## Tasks

### T1 — canonicalize the doctor join (Bug 1) · [x] done

Add one exported-nothing helper next to `mcpServer` in `internal/report/tools.go`
(the canonicalizer belongs beside the function that produces the other spelling)
mapping `:`, `.` and space to `_`. Use it in `doctorJoin` on **both** observation
channels and on the configured side, so one server is one key with the union of
its sessions.

- Comparison key is canonical; `perServer` becomes canon-keyed, so
  `len(perServer[k])` is the true distinct-session union.
- Display: `never_observed` keeps the **config's own spelling** (the string the
  user greps in their config). `unconfigured_observed` displays the smallest raw
  spelling seen for that canon key — a total order (reproducibility) that happens
  to prefer the attribution punctuation, since `:` `.` and space all sort below
  `_`, matching the spellings users recognise (`plugin:engram:engram`,
  `claude.ai Notion`).
- Preserve the existing single merged ascending pass and the "keys are disjoint"
  total order; keep `mcp_sessions` semantics unchanged.
- `isConfigured` must be canon-keyed, and the miss loop must compare canon to
  canon — a raw config name containing `:` would otherwise look unconfigured.
- Tests: both spellings of one server collapse to one row carrying the union of
  sessions; a configured `plugin:sre:k8s-qa` joins an observed
  `plugin_sre_k8s-qa`; display spelling choice is pinned; skill/plugin keys are
  untouched by the canonicalizer.

Acceptance: `go test ./internal/report/` green, new tests pin the collapse and
the display rule.

### T2 — canonicalize mcp_server rent keys (Bug 1 sibling) · [x] done

Apply the same helper to the `mcp_server` rent keys in `ToolsEnvelope` where
`rentSegment`'s output is accumulated, so `claude.ai Notion` rent lands on the
`claude_ai_Notion` row and `plugin:github:github` rent lands on
`plugin_github_github`.

- **mcp_server dimension only.** `skillRent` and `pluginRent` are not touched.
- `rentSegment` keeps its authority-based `ok` decision for plugin-dim
  attribution; canon only changes the *key spelling*, which is orthogonal.
- A rent key that merges onto a called key must stop appearing in
  `rentOnlyKeys`, and a rent-only key with no calls stays a rent-only row with an
  honest `calls 0`.
- Tests: rent under `claude.ai Notion` merges onto a `claude_ai_Notion` call row;
  colon-form plugin rent with an unenabled plugin canonicalizes; a skill key
  containing `:` is unchanged.

Acceptance: `go test ./internal/report/` green; `tare tools` on a frozen corpus
shows no `claude.ai *` / `plugin:*` row beside an underscore row of the same
server.

### T3 — read plugin-provided MCP servers (Bug 2) · [x] done

Add a fifth source to `doctorMCP`: `~/.claude/plugins/installed_plugins.json` →
each entry's `installPath` → `<installPath>/.mcp.json`, with servers namespaced
as `plugin:<plugin>:<server>` (`<plugin>` = the part before the first `@`, the
rule `pluginNames` already uses).

- **Never walk `~/.claude/plugins/cache`.** Follow `installPath` only — the cache
  holds superseded versions whose `.mcp.json` would resurrect removed servers
  (`engram` 0.1.2 vs installed 0.1.3).
- Sniff **both** `.mcp.json` shapes: `mcpServers` wrapper, else top-level keys
  whose values are objects (the `github` case). Values decode as
  `json.RawMessage`, tolerant structs, no version field trusted.
- Namespaced keys cannot collide with plain config names, so no new precedence
  rule is needed. Merge every plugin server into **one** `mcpSrc` so a plugin
  with several install entries (3 do) cannot emit a bogus "project config wins"
  collision; later entry wins silently within that one source.
- Standing posture, unchanged: `installed_plugins.json` absent is silent; a file
  that exists but is unreadable or unparseable (the malformed
  `financial-analysis` case) warns naming the path and sets `broken`/`partial`;
  a plugin whose `.mcp.json` is absent is a silent skip.
- Plugin servers count toward `mcp_servers_checked` and go through
  `doctorCheckServer` like any other entry — a plugin stdio server whose command
  is not on PATH is a real finding.
- Use `doctorReader.read` so the file counts toward the static envelope's corpus
  header.
- Tests: fixture tree with `installed_plugins.json` + both `.mcp.json` shapes →
  namespaced names reach the join and an observed `plugin_sre_k8s-qa` no longer
  reports `unconfigured_observed`; a superseded cache dir is **not** read; a
  malformed plugin `.mcp.json` warns and makes the block partial; a missing
  `installed_plugins.json` is silent.

Acceptance: `go test ./internal/report/` green; on a frozen corpus
`tare doctor` reports the surviving rows listed under "Not bugs" and no
`plugin:sre:*` / `plugin:context7:*` / `plugin:github:*` row.

### T4 — documentation · [x] done

- `README.md`: the doctor MCP source list (currently four sources, ~line 234)
  gains the plugin source and its `plugin:<plugin>:<server>` naming; the
  `MCP_SERVER` rent prose (~line 195) says rent keys are canonicalized to the
  tool-name spelling so one server is one row; the known-limits table (~line 473)
  updates the "Plugin names come from user-scope `settings.json` only" row, which
  is now true for the *call-side rollup* but no longer for the *doctor join*.
- `AGENTS.md`: record the three traps under the existing design-constraint list —
  the two-spelling split and why the canonicalizer is mcp_server-only;
  `installed_plugins.json` vs. the cache walk (with the engram 0.1.2/0.1.3
  evidence); the two `.mcp.json` shapes with the corpus proof that the top-level
  one loads.
- No counts in either file (AGENTS.md rule: counts go stale within the session
  that writes them) except where the evidence is dated and load-bearing.
- `CLAUDE.md` stays exactly three lines; machine-specific notes stay in
  `CLAUDE.local.md`.

Acceptance: docs match the shipped behaviour; `usage()` unchanged (no flag
change in this feature).

### T5 — verification on a frozen corpus · [ ] pending

Freeze `~/.claude/projects` once to a temp dir outside the repo and point
`--dir` at the copy for every before/after comparison.

- `go build ./...`, `go build -o tare .`, `go test ./...`, `go vet ./...`
- `gofmt -l . | tee /dev/stderr | wc -l` → 0
- `go list -m all | wc -l` → 1
- `TestReportReproducible`, `TestReportMarkdownVariesOnlyByTimestamp`,
  `TestValidateRejects` green
- `tare doctor --dir <frozen>` before vs. after: `unconfigured_observed` row
  count and the surviving set; confirm `mcp_sessions` is unchanged by
  canonicalization (it counts sessions, not servers) and that
  `mcp_servers_checked` grew by the plugin servers
- `tare tools --dir <frozen>` before vs. after: no `claude.ai *` or `plugin:*`
  row left beside its underscore twin; total rent unchanged (merging rows must
  not lose or double a byte)
- `tare report` markdown still varies only by timestamp

Acceptance: every command above run, each result reported verbatim, any failure
named rather than smoothed over.

## Progress

- 2026-09-20 — feature document created after exploration. Bugs 1 and 2
  reproduced and measured on the live corpus; the sibling `tare tools` rent split
  and the two `.mcp.json` shapes were found during verification and folded into
  scope as the same root cause. `engram` reclassified from "removal" to
  "mechanism change" on user correction plus the 0.1.2/0.1.3 evidence.
  Route: delegated direct. TDD off. No source written yet.
- 2026-09-20 — T1 done. `mcpCanon` added beside `mcpServer` in
  `internal/report/tools.go`; `doctorJoin` restructured — `perServer` and the
  configured set are canon-keyed (`configName` replaces `isConfigured`, one
  map for membership plus the config display spelling), `see` canonicalizes
  both observation channels at the single choke point and records the
  smallest raw spelling per canon key for `unconfigured_observed` display.
  The merged ascending miss pass and `mcp_sessions` semantics are unchanged;
  it now sorts canon keys and displays each key's own spelling. Tests:
  `TestMCPCanon`, `TestSkillKeysNeverCanonicalized` (tools_test.go),
  `TestDoctorJoinCanonCollapsesSpellings`,
  `TestDoctorJoinConfiguredColonFormJoins`,
  `TestDoctorJoinNeverObservedKeepsConfigSpelling` (doctor_test.go). No
  frozen-corpus run (that is T5).
- 2026-09-20 — T2 done. `mcpCanon` now reaches the `tare tools` rent side in
  both branches of `ToolsEnvelope`'s post-scan resolution: the authority branch
  accumulates under `mcpCanon(seg)` (so `rentSegment` keeps its authority-based
  `ok` decision and its `plugin` return untouched — canonicalization changes the
  KEY SPELLING only), and the authority-unavailable branch builds a canon-keyed
  copy instead of aliasing `mcpRentRaw`, which is where the split would
  otherwise survive on any machine without `~/.claude/settings.json`.
  `pluginRent` keys stay plugin names, `skillRent`/`skillCalls` never see the
  canonicalizer, and the call-side `servers`/`pluginSegments` buckets are
  unchanged. A canon key with no calls stays a rent-only row with an honest
  `calls 0`. Tests: `TestToolsEnvelopeRentCanonMergesOntoCallRow`,
  `TestToolsEnvelopeRentCanonUnenabledPlugin`,
  `TestToolsEnvelopeRentCanonWithoutAuthority` (tools_test.go, plus the
  byte-conservation helpers `totalRent`/`countRows`);
  `TestToolsEnvelopeRentJoinsCalls` had two assertions that pinned the OLD
  verbatim spelling and were updated to the canonical one;
  `TestSkillKeysNeverCanonicalized` extended with a second colon-form skill key
  rather than duplicated. No frozen-corpus run (that is T5).
- 2026-09-20 — T3 done. `doctorMCP` reads a fifth source:
  `<claudeRoot>/plugins/installed_plugins.json` → each entry's `installPath` →
  `<installPath>/.mcp.json`, in `doctorPluginMCPSrc` + `doctorMCPServers`
  (doctor.go, beside their only caller). Servers are namespaced
  `plugin:<plugin>:<server>` (`<plugin>` = the part before the first `@`,
  `pluginNames`' rule) and merged into ONE `mcpSrc` with `scope: "plugin"`,
  appended last, so a multi-entry plugin cannot emit a bogus collision and a
  hand-written entry of the same spelling stays the winner. Both `.mcp.json`
  shapes are sniffed by the presence of the `mcpServers` key; in the bare shape
  only an object value counts as a server. Discovery follows `installPath` and
  never walks `plugins/cache` (the engram 0.1.2/0.1.3 trap, recorded in a
  comment at the read site). The standing posture is unchanged: an absent
  registry is silent, an unreadable or unparseable registry or plugin
  `.mcp.json` warns naming the path and sets the broken floor / `block.partial`,
  an absent plugin `.mcp.json` is a silent skip. Every file goes through
  `doctorReader.read`, and a repeated `installPath` is read once so the corpus
  header counts files rather than entries. Plugin servers count toward
  `mcp_servers_checked` and pass through `doctorCheckServer`.
  `doctor_opencode.go` untouched. Tests: `TestDoctorPluginMCPServersConfigure`,
  `TestDoctorPluginMCPBareShape`, `TestDoctorPluginMCPIgnoresCache`,
  `TestDoctorPluginMCPBrokenFile`, `TestDoctorPluginMCPAbsentRegistryIsSilent`,
  `TestDoctorPluginMCPMultipleEntriesOneSource` (doctor_test.go, plus the
   `doctorPluginInstall` registry fixture helper). No frozen-corpus run (that is
   T5).
- 2026-09-20 — T4 done, docs only. `README.md`: the `tare doctor` source passage
  now names five sources, with the plugin `.mcp.json` discovered via
  `installed_plugins.json → installPath` (never the cache) and namespaced
  `plugin:<plugin>:<server>`; the `tare tools` RENT prose says MCP-server rent
  keys are canonicalized to the tool-name spelling, one server one row, and
  states the MCP-server-only boundary (skill/plugin keys keep theirs); the
  known-limits row is scoped to the `tare tools` `PLUGIN` call-side rollup
  (authority still `enabledPlugins`) and notes the doctor join reads
  `installed_plugins.json` directly as a different mechanism. `AGENTS.md`: three
  bullets under the design constraints after the attribution-mechanisms one —
  the two-spelling split and why `mcpCanon` is mcp_server-only (and not inside
  `mcpServer()`); `installed_plugins.json` → `installPath`, never the cache,
  with the dated engram 0.1.2/0.1.3 evidence; both `.mcp.json` shapes with the
  `plugin:github:github` corpus proof. No counts committed; `usage()` untouched
  (verified `main.go` registers no new flag). No Go file edited;
  `.agents/skills/tare/references/harnesses.md` still lists four MCP sources for
  doctor — out of T4 scope, reported to the orchestrator.

## Verification evidence

T1 (2026-09-20):

- `go build ./...`: clean, no output
- `go test ./internal/report/`: ok github.com/AngelVRodC/tare/internal/report
- `go test ./...`: ok — tare, internal/report, internal/transcript all pass
- `go vet ./...`: clean, no output
- `gofmt -l . | tee /dev/stderr | wc -l`: 0
- `go list -m all | wc -l`: 1
- `go test -run 'TestReportReproducible|TestReportMarkdownVariesOnlyByTimestamp|TestValidateRejects' ./...`: ok github.com/AngelVRodC/tare/internal/report (root and transcript: no tests to run)
- `go test -count=1 -run 'TestDoctorJoinCanon|TestDoctorJoinConfiguredColonForm|TestDoctorJoinNeverObservedKeeps|TestMCPCanon|TestSkillKeysNeverCanonicalized' -v ./internal/report/`: 5/5 PASS (new T1 tests, uncached)

T2 and T3 (2026-09-20):

- `go build ./...`: clean, no output
- `go test -count=1 ./internal/report/`: ok github.com/AngelVRodC/tare/internal/report 0.333s
- `go test -count=1 ./...`: ok — tare 0.170s, internal/report 0.461s, internal/transcript 0.437s
- `go vet ./...`: clean, no output
- `gofmt -l . | tee /dev/stderr | wc -l`: 0
- `go list -m all | wc -l`: 1
- `go test -count=1 -run 'TestReportReproducible|TestReportMarkdownVariesOnlyByTimestamp|TestValidateRejects' ./...`: ok github.com/AngelVRodC/tare/internal/report 0.513s (root and transcript: no tests to run); verbose: 3/3 PASS
- `go test -count=1 -v -run 'TestToolsEnvelopeRentCanonMergesOntoCallRow|TestToolsEnvelopeRentCanonUnenabledPlugin|TestToolsEnvelopeRentCanonWithoutAuthority|TestSkillKeysNeverCanonicalized' ./internal/report/`: 4/4 PASS (uncached)
- `go test -count=1 -v -run 'TestDoctorPluginMCPServersConfigure|TestDoctorPluginMCPBareShape|TestDoctorPluginMCPIgnoresCache|TestDoctorPluginMCPBrokenFile|TestDoctorPluginMCPAbsentRegistryIsSilent|TestDoctorPluginMCPMultipleEntriesOneSource' ./internal/report/`: 6/6 PASS (uncached)
- `go test -count=1 ./internal/report/` after updating the two stale
  verbatim-spelling assertions in `TestToolsEnvelopeRentJoinsCalls`: ok — that
  test was the only pre-existing failure the change caused
- Real-tree read-only smoke run (NOT the frozen corpus, which is T5):
  `/tmp/tare-verify doctor --dir /tmp/tare-fixture` — a one-event fixture corpus
  beside the measuring machine's real `~/.claude`. The new source produced
  exactly the 13 namespaced servers predicted from the registry:
  `plugin:context7:context7`, `plugin:data:openmetadata`, `plugin:data:redshift`,
  `plugin:figma:figma`, `plugin:figma:figma-desktop`, `plugin:github:github`,
  `plugin:notion:notion`, `plugin:sre:grafana-prod`, `plugin:sre:grafana-qa`,
  `plugin:sre:jaeger-prod`, `plugin:sre:jaeger-qa`, `plugin:sre:k8s-prod`,
  `plugin:sre:k8s-qa` — including `plugin:github:github`, which only the BARE
  `.mcp.json` shape declares. `financial-analysis` warned as predicted:
  `mcp: /Users/darlene/.claude/plugins/cache/claude-for-financial-services/financial-analysis/0.1.1/.mcp.json
  exists but is not valid JSON (invalid character '"' after object key:value
  pair) — mcp_servers_checked is omitted, not zero`, and the join carried its
  `configured set is partial` caveat. No `plugin:engram:engram` (the installed
  0.1.3 ships no `.mcp.json` and the stale 0.1.2 cache dir was not read).

## Next step

T5 — verification on a frozen corpus: freeze `~/.claude/projects` once to a
temp dir outside the repo, run the full gate battery (`go build`, `go vet`,
`go test ./...`, `gofmt`, `go list`), then the before/after `tare doctor` and
`tare tools` comparisons listed under T5 in the Tasks section — surviving
`unconfigured_observed` set, unchanged `mcp_sessions`, rent byte conservation,
and the timestamp-only Markdown artifact check.
