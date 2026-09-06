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
	b := newBuilder(dir, version, "scan")

	stats, err := transcript.Scan(dir, func(ev *transcript.Event) {
		b.see(ev)
		types[ev.Type]++
		if ev.Version != "" {
			versions[ev.Version]++
		}
	})
	if err != nil {
		return Envelope{}, err
	}

	topLevel := stats.Files - stats.SubagentFiles
	add := b.add
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
		b.warn("retention gap unavailable: %v", err)
	} else {
		add("stats_cache_sessions", sessions, "sessions")
		add("retention_gap", sessions-topLevel, "sessions")
	}

	for _, t := range slices.Sorted(maps.Keys(types)) {
		b.rows(MeasuredMetric("events", "type", t, types[t], "events"))
	}
	for _, v := range slices.Sorted(maps.Keys(versions)) {
		b.rows(MeasuredMetric("events", "cli_version", v, versions[v], "events"))
	}
	for _, t := range slices.Sorted(maps.Keys(stats.UnknownTypes)) {
		b.rows(MeasuredMetric("unknown_type_events", "unknown_type", t, stats.UnknownTypes[t], "events"))
		b.warn("unknown event type %q seen %d times — counted, not parsed", t, stats.UnknownTypes[t])
	}
	return b.done(stats), nil
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
	renderHeader(w, env)
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
	return renderTail(w, tw, env)
}

// renderHeader is the two identifying lines every table starts with.
func renderHeader(w io.Writer, env Envelope) {
	fmt.Fprintf(w, "tare %s — %s\n", env.Command, env.Corpus.Dir)
	fmt.Fprintf(w, "%s .. %s\n", env.Corpus.From, env.Corpus.To)
}

// renderTail flushes the table and prints the warnings under it. They stay out
// of the table on purpose: a warning is what the command could not measure.
func renderTail(w io.Writer, tw *tabwriter.Writer, env Envelope) error {
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

// writeCorpus prints the keyless corpus-wide rows as a three-column block.
func writeCorpus(tw io.Writer, env Envelope) {
	fmt.Fprint(tw, "\nCORPUS\t\t\n")
	for _, m := range env.Metrics {
		if m.Dimension == "corpus" {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", m.Name, formatValue(m.Value), m.Derivation)
		}
	}
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
