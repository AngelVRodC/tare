package report

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/AngelVRodC/tare/internal/transcript"
)

// ScanEnvelope walks dir and returns the corpus inventory: files, bytes, date
// range, per-type and per-version event counts, the top-level vs `subagents/`
// split, unknown types and the retention gap. Every row is measured.
func ScanEnvelope(dir, version string) (Envelope, error) {
	types := map[string]int{}
	versions := map[string]int{}
	var from, to string

	stats, err := transcript.Scan(dir, func(ev *transcript.Event) {
		types[ev.Type]++
		if ev.Version != "" {
			versions[ev.Version]++
		}
		if ts := ev.Timestamp; ts != "" {
			if from == "" || ts < from {
				from = ts
			}
			if to == "" || ts > to {
				to = ts
			}
		}
	})
	if err != nil {
		return Envelope{}, err
	}

	topLevel := stats.Files - stats.SubagentFiles
	env := Envelope{
		Tool:    "tare",
		Version: version,
		Command: "scan",
		Corpus: Corpus{
			Dir:   dir,
			Files: stats.Files,
			Bytes: stats.Bytes,
			From:  day(from),
			To:    day(to),
		},
		Metrics:  []Metric{},
		Warnings: []string{},
	}

	add := func(name string, value any, unit string) {
		env.Metrics = append(env.Metrics, MeasuredMetric(name, "corpus", "", value, unit))
	}
	add("files", stats.Files, "files")
	add("bytes", stats.Bytes, "bytes")
	add("events", stats.Lines, "events")
	add("files_top_level", topLevel, "files")
	add("files_subagent", stats.SubagentFiles, "files")
	add("distinct_event_types", len(types), "types")
	add("known_event_types", transcript.KnownTypeCount(), "types")
	add("distinct_cli_versions", len(versions), "versions")
	add("parse_errors", stats.ParseErrors, "errors")

	// Retention gap: Claude Code's own session count against the transcripts
	// still on disk. stats-cache.json is a sibling of the projects directory.
	cachePath := filepath.Join(filepath.Dir(filepath.Clean(dir)), "stats-cache.json")
	if sessions, err := cachedSessions(cachePath); err != nil {
		env.Warnings = append(env.Warnings,
			fmt.Sprintf("retention gap unavailable: %v", err))
	} else {
		add("stats_cache_sessions", sessions, "sessions")
		add("retention_gap", sessions-topLevel, "sessions")
	}

	for _, t := range slices.Sorted(maps.Keys(types)) {
		env.Metrics = append(env.Metrics,
			MeasuredMetric("events", "type", t, types[t], "events"))
	}
	for _, v := range slices.Sorted(maps.Keys(versions)) {
		env.Metrics = append(env.Metrics,
			MeasuredMetric("events", "cli_version", v, versions[v], "events"))
	}
	for _, t := range slices.Sorted(maps.Keys(stats.UnknownTypes)) {
		env.Metrics = append(env.Metrics,
			MeasuredMetric("unknown_type_events", "unknown_type", t, stats.UnknownTypes[t], "events"))
		env.Warnings = append(env.Warnings,
			fmt.Sprintf("unknown event type %q seen %d times — counted, not parsed",
				t, stats.UnknownTypes[t]))
	}
	if stats.ParseErrors > 0 {
		env.Warnings = append(env.Warnings,
			fmt.Sprintf("%d lines failed to decode", stats.ParseErrors))
	}

	return env, nil
}

// cachedSessions reads totalSessions out of Claude Code's stats-cache.json.
func cachedSessions(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var cache struct {
		TotalSessions int `json:"totalSessions"`
	}
	if err := json.Unmarshal(b, &cache); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return cache.TotalSessions, nil
}

// RenderScan prints the envelope as a table. It reads only the envelope, so
// the table and `--json` can never disagree.
func RenderScan(w io.Writer, env Envelope) error {
	fmt.Fprintf(w, "tare %s — %s\n", env.Command, env.Corpus.Dir)
	fmt.Fprintf(w, "%s .. %s\n", env.Corpus.From, env.Corpus.To)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	section := ""
	for _, m := range env.Metrics {
		if m.Dimension != section {
			section = m.Dimension
			fmt.Fprintf(tw, "\n%s\t\t\n", strings.ToUpper(section))
		}
		label := m.Name
		if m.Key != "" {
			label = m.Key
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", label, formatValue(m.Value), m.Derivation)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, warn := range env.Warnings {
		fmt.Fprintf(w, "\nwarning: %s", warn)
	}
	if len(env.Warnings) > 0 {
		fmt.Fprintln(w)
	}
	return nil
}

// day trims an RFC3339 timestamp to its date. RFC3339 sorts lexically, so no
// time parsing is needed anywhere in this file.
func day(ts string) string {
	if len(ts) < 10 {
		return ts
	}
	return ts[:10]
}

func formatValue(v any) string {
	switch n := v.(type) {
	case int:
		return comma(int64(n))
	case int64:
		return comma(n)
	case float64:
		// 'g' keeps a 23.6x multiplier readable and a $0.0000149 allocation
		// honest, rather than rounding the second one away to zero.
		return strconv.FormatFloat(n, 'g', 6, 64)
	}
	return fmt.Sprint(v)
}

func comma(n int64) string {
	if n < 0 {
		return "-" + comma(-n)
	}
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
