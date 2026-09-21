package report

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/AngelVRodC/tare/internal/codex"
	"github.com/AngelVRodC/tare/internal/transcript"
)

type codexKey struct{ session, id string }
type codexCall struct {
	name, server, timestamp string
	digest                  [32]byte
	kind                    string
}
type codexResult struct {
	content   codex.Content
	timestamp string
	digest    [32]byte
	kind      string
}

// CodexToolsEnvelope measures outer model-visible function/custom-tool results.
// Execution-event mirrors and compaction replacement histories are not new
// results. Retained state is IDs, hashes and byte counts, never output bodies.
func CodexToolsEnvelope(dir, version string, w Window) (Envelope, error) {
	includes, err := codexWindow(w)
	if err != nil {
		return Envelope{}, err
	}
	b := newBuilder(dir, version, "tools", w)
	b.windowNote = "this codex run measures events inside the requested inclusive UTC window; corpus files/bytes and unmatched_results remain whole-corpus, joins cross window boundaries, events without valid timestamps are excluded, and cost is unavailable"
	calls := map[codexKey]codexCall{}
	results := map[codexKey]codexResult{}
	parents := map[string]string{}
	unsupported := map[string]int{}
	untimestamped := map[string]int{}
	var duplicates, malformed int
	var orchestration bool
	stats, err := codex.Scan(dir, func(ev codex.Event) error {
		b.seeTimeIncluded(ev.Timestamp, ev.Type, includes(ev.Timestamp))
		if ev.Timestamp == "" {
			untimestamped[ev.Type]++
		}
		if ev.Type == "session_meta" {
			var meta struct {
				ForkedFrom string `json:"forked_from_id"`
				Parent     string `json:"parent_thread_id"`
			}
			if err := json.Unmarshal(ev.Payload, &meta); err != nil {
				return fmt.Errorf("codex: invalid session ancestry in %s", ev.File)
			}
			parent := meta.ForkedFrom
			if parent == "" {
				parent = meta.Parent
			}
			if parent != "" {
				if previous := parents[ev.Session]; previous != "" && previous != parent {
					return fmt.Errorf("codex: conflicting ancestry for session %s", ev.Session)
				}
				parents[ev.Session] = parent
			}
		}
		if ev.Type != "response_item" {
			return nil
		}
		var item struct {
			Type      string          `json:"type"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Namespace string          `json:"namespace"`
			Output    json.RawMessage `json:"output"`
		}
		if json.Unmarshal(ev.Payload, &item) != nil {
			malformed++
			return nil
		}
		switch item.Type {
		case "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output":
		case "message", "reasoning", "agent_message":
			return nil
		default:
			unsupported[item.Type]++
			return nil
		}
		if item.CallID == "" {
			malformed++
			return nil
		}
		key := codexKey{ev.Session, item.CallID}
		// Only exact payload duplicates (ignoring whitespace), at the same
		// timestamp and in the same session, can safely be counted once.
		var compact bytes.Buffer
		if err := json.Compact(&compact, ev.Payload); err != nil {
			return err
		}
		digest := sha256.Sum256(compact.Bytes())
		conflict := func() error {
			return fmt.Errorf("codex: conflicting %s for session %s call_id %s; cannot measure an ambiguous join", item.Type, ev.Session, item.CallID)
		}
		if item.Type == "function_call" || item.Type == "custom_tool_call" {
			if item.Name == "" {
				malformed++
				return nil
			}
			if prior, ok := calls[key]; ok {
				if prior.digest != digest || prior.timestamp != ev.Timestamp {
					return conflict()
				}
				duplicates++
				return nil
			}
			calls[key] = codexCall{codex.ToolName(item.Namespace, item.Name), codex.MCPServer(item.Namespace, item.Name), ev.Timestamp, digest, item.Type}
			if item.Name == "exec" && (item.Namespace == "" || item.Namespace == "functions") {
				orchestration = true
			}
		} else {
			if prior, ok := results[key]; ok {
				if prior.digest != digest || prior.timestamp != ev.Timestamp {
					return conflict()
				}
				duplicates++
				return nil
			}
			results[key] = codexResult{codex.Measure(item.Output), ev.Timestamp, digest, item.Type}
		}
		return nil
	})
	if err != nil {
		return Envelope{}, err
	}
	// Copied parent records preceding child metadata retain parent ownership
	// and deduplicate normally. A reused call ID under BOTH parent and child
	// metadata has ambiguous ownership; refuse to double-count inherited work.
	for _, key := range slices.SortedFunc(maps.Keys(calls), compareCodexKeys) {
		seen := map[string]bool{key.session: true}
		for parent := parents[key.session]; parent != ""; parent = parents[parent] {
			if seen[parent] {
				return Envelope{}, fmt.Errorf("codex: cyclic ancestry for session %s", key.session)
			}
			seen[parent] = true
			if _, exists := calls[codexKey{parent, key.id}]; exists {
				return Envelope{}, fmt.Errorf("codex: ambiguous inherited call_id %s in related sessions %s and %s", key.id, key.session, parent)
			}
		}
	}

	tools, servers := map[string]*toolStat{}, map[string]*toolStat{}
	// Unknown status/production is universal. Unknown payloads taint only
	// their own rows and containing totals, not unrelated tools or servers.
	unknownText, unknownImage := map[string]bool{}, map[string]bool{}
	serverText, serverImage := map[string]bool{}, map[string]bool{}
	textKnown, imageKnown, imagesKnown := true, true, true
	var totals toolStat
	var uses, returned, unmatched, unanswered, images int64
	for _, c := range calls {
		if includes(c.timestamp) {
			uses++
		}
	}
	for key, c := range calls {
		if _, ok := results[key]; !ok && includes(c.timestamp) {
			unanswered++
		}
	}
	// Sort diagnostics as well as metrics. Input directory order must not
	// decide the order of unmatched-join warnings.
	keys := slices.SortedFunc(maps.Keys(results), compareCodexKeys)
	for _, key := range keys {
		r := results[key]
		c, ok := calls[key]
		if !ok {
			unmatched++
			b.warn("codex session %s: result %s has no matching call", key.session, key.id)
			continue
		}
		if r.kind != c.kind+"_output" {
			return Envelope{}, fmt.Errorf("codex session %s: result %s has a different call type from its matching call", key.session, key.id)
		}
		if !includes(r.timestamp) {
			continue
		}
		returned++
		x := r.content
		bucket(tools, c.name).add(x.TextBytes, x.ImageBytes, 0, false)
		totals.add(x.TextBytes, x.ImageBytes, 0, false)
		if c.server != "" {
			bucket(servers, c.server).add(x.TextBytes, x.ImageBytes, 0, false)
			serverText[c.server] = serverText[c.server] || !x.TextKnown
			serverImage[c.server] = serverImage[c.server] || !x.ImageKnown
		}
		unknownText[c.name] = unknownText[c.name] || !x.TextKnown
		unknownImage[c.name] = unknownImage[c.name] || !x.ImageKnown
		textKnown, imageKnown, imagesKnown = textKnown && x.TextKnown, imageKnown && x.ImageKnown, imagesKnown && x.ImagesKnown
		if x.HasImage {
			images++
		}
	}
	b.addWin("tool_use_blocks", uses, "blocks")
	b.addWin("tool_result_blocks", returned, "blocks")
	b.add("unmatched_results", unmatched, "blocks")
	b.addWin("unanswered_uses", unanswered, "blocks")
	b.addWin("distinct_tools", len(tools), "tools")
	b.addWin("calls", totals.Calls, "calls")
	if textKnown {
		b.addWin("context_bytes", totals.ContextBytes, "bytes")
	}
	if imageKnown {
		b.addWin("image_bytes", totals.ImageBytes, "bytes")
	}
	if imagesKnown {
		b.addWin("image_results", images, "results")
	} else {
		b.warn("codex image_results is omitted from the corpus: an unsupported output shape prevents a complete count")
	}
	addRows := func(dim string, values map[string]*toolStat, textUnknown, imageUnknown map[string]bool) {
		rows := statMetrics(dim, values, "errors", "produced_bytes")
		rows = slices.DeleteFunc(rows, func(m Metric) bool {
			return (m.Name == "context_bytes" && textUnknown[m.Key]) || (m.Name == "image_bytes" && imageUnknown[m.Key])
		})
		b.rows(rows...)
		for _, field := range []struct {
			name string
			keys map[string]bool
		}{{"context_bytes", textUnknown}, {"image_bytes", imageUnknown}} {
			for _, key := range slices.Sorted(maps.Keys(field.keys)) {
				if field.keys[key] {
					b.warn("codex %s is omitted for %s %q and from the corpus total: at least one result has an unmeasured payload", field.name, dim, key)
				}
			}
		}
	}
	addRows("tool", tools, unknownText, unknownImage)
	addRows("mcp_server", servers, serverText, serverImage)
	b.warn("codex measures recorded outer function/custom-tool result text and inline base64 image payloads; this is result volume, not current context occupancy or billed tokens; execution-event mirrors and compaction replacement history are excluded")
	b.warn("errors, produced_bytes, externalised_results, externalised_produced_bytes and externalised_context_bytes are not reported for codex: complete status and production-to-delivery mappings are unmeasured, not zero")
	b.warn("rent_bytes, skill_calls and plugin attribution are not reported for codex: instruction state and tool namespaces do not establish installed rent, skill use or plugin ownership")
	if orchestration {
		b.warn("codex orchestration results are attributed to the outer exec tool; nested tool bytes cannot be apportioned, and mcp_server rows cover direct calls only")
	}
	for _, kind := range slices.Sorted(maps.Keys(unsupported)) {
		b.warn("codex: %d unsupported response_item %q records excluded; tool coverage is incomplete", unsupported[kind], kind)
	}
	for _, kind := range slices.Sorted(maps.Keys(stats.UnknownTypes)) {
		b.warn("codex: %d unknown %q events ignored", stats.UnknownTypes[kind], kind)
	}
	if duplicates > 0 {
		b.warn("codex: %d identical call/result copies counted once within their session", duplicates)
	}
	if len(parents) > 0 {
		b.warn("codex fork/subagent records are scoped by session_meta.id; external parent history is not expanded, and inherited call IDs with ambiguous ownership are rejected")
	}
	if malformed > 0 {
		b.warn("codex: %d malformed tool records excluded; calls and joins may be incomplete", malformed)
	}
	if stats.UnscopedRecords > 0 || stats.ExcludedFiles > 0 {
		b.warn("codex: %d records without session identity and %d files without Codex metadata excluded; corpus bytes includes all scanned JSONL bytes", stats.UnscopedRecords, stats.ExcludedFiles)
	}
	if stats.InvalidTimes > 0 {
		b.warn("codex: %d invalid timestamps treated as unavailable", stats.InvalidTimes)
	}
	if w.Active() {
		for _, kind := range slices.Sorted(maps.Keys(untimestamped)) {
			b.warn("codex: %d %q events have no valid timestamp and cannot be placed in the window", untimestamped[kind], kind)
		}
	}
	return b.done(transcript.ScanStats{Files: stats.Files, Bytes: stats.Bytes, ParseErrors: stats.ParseErrors}), nil
}

func compareCodexKeys(a, z codexKey) int {
	return cmp.Or(strings.Compare(a.session, z.session), strings.Compare(a.id, z.id))
}

// codexWindow keeps sub-millisecond timestamps and offsets correct without
// changing either the public bound syntax or the other adapters' comparisons.
func codexWindow(w Window) (func(string) bool, error) {
	bound := func(s string, end bool) (string, error) {
		if s == "" {
			return "", nil
		}
		if len(s) == 10 {
			if end {
				s += "T23:59:59.999999999Z"
			} else {
				s += "T00:00:00Z"
			}
		}
		return codex.Timestamp(s)
	}
	start, err := bound(w.Since(), false)
	if err != nil {
		return nil, fmt.Errorf("codex --since: %w", err)
	}
	end, err := bound(w.Until(), true)
	if err != nil {
		return nil, fmt.Errorf("codex --until: %w", err)
	}
	return func(ts string) bool {
		return !w.Active() || (ts != "" && (start == "" || ts >= start) && (end == "" || ts <= end))
	}, nil
}
