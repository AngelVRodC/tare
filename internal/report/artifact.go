package report

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// Report is the composed artifact: the four command envelopes a reader can
// check section by section, plus the single merged envelope `--json` emits.
//
// The four sections are separate streaming passes over the corpus rather than
// one fused pass. Four passes over a quarter-gigabyte corpus cost about four
// seconds; fusing them would mean one function holding every accumulator of
// all four commands, and the day a section's arithmetic is questioned it could
// no longer be re-run on its own. Fuse it only if the wall time ever becomes
// the complaint.
type Report struct {
	Scan       Envelope
	Tools      Envelope
	Attribute  Envelope
	Corruption Envelope
	Envelope   Envelope
}

// BuildReport runs every command and merges the results.
//
// `--boost-deep` is deliberately not offered here: it shells out to sqlite3
// against a Boost history DB that only exists on the machine that ran it, so
// an artifact built with it could not be reproduced by the skeptic it is
// written for. `tare corruption --boost-deep` remains available on its own.
func BuildReport(dir, version string) (Report, error) {
	var (
		r   Report
		err error
	)
	if r.Scan, err = ScanEnvelope(dir, version); err != nil {
		return Report{}, err
	}
	if r.Tools, err = ToolsEnvelope(dir, version); err != nil {
		return Report{}, err
	}
	if r.Attribute, err = AttributeEnvelope(dir, version); err != nil {
		return Report{}, err
	}
	if r.Corruption, err = CorruptionEnvelope(dir, version, false); err != nil {
		return Report{}, err
	}
	r.Envelope = merge(dir, version, []Envelope{r.Scan, r.Tools, r.Attribute, r.Corruption})
	return r, nil
}

// metricID is what makes two rows the same row; seenMetric remembers which
// command emitted it first and what it said.
type metricID struct{ name, dimension, key string }

type seenMetric struct{ value, command string }

// merge unions the four envelopes into one.
//
// The commands overlap on purpose — `tools` and `corruption` both count calls
// per tool — so the union is deduplicated. A duplicate whose value disagrees
// is not dropped silently: it becomes a warning, because two commands
// measuring the same thing differently is a defect in this tool, and an
// artifact that hides it is worth nothing.
func merge(dir, version string, envs []Envelope) Envelope {
	out := Envelope{
		Tool:     "tare",
		Version:  version,
		Command:  "report",
		Corpus:   Corpus{Dir: dir},
		Metrics:  []Metric{},
		Warnings: []string{},
	}
	seen := map[metricID]seenMetric{}
	for _, env := range envs {
		for _, m := range env.Metrics {
			id := metricID{m.Name, m.Dimension, m.Key}
			// Not formatValue: that is the display formatter, and it is about
			// to become lossy per unit. This comparison decides whether a real
			// disagreement between two commands is reported, so it needs a
			// lossless key. %v also normalises int against int64 — tools.go
			// emits distinct_tools as int, corruption.go as int64, and a raw
			// `any` comparison would fire a false disagreement on that pair.
			value := fmt.Sprintf("%v", m.Value)
			if prev, dup := seen[id]; dup {
				if prev.value != value {
					out.Warnings = append(out.Warnings, fmt.Sprintf(
						"report: %s/%s/%s disagrees between %s (%s) and %s (%s) — the %s value is reported",
						m.Name, m.Dimension, m.Key,
						prev.command, prev.value, env.Command, value, prev.command))
				}
				continue
			}
			seen[id] = seenMetric{value, env.Command}
			out.Metrics = append(out.Metrics, m)
		}
		for _, w := range env.Warnings {
			out.Warnings = append(out.Warnings, env.Command+": "+w)
		}
	}

	// The corpus is live: sessions are appended while the four passes run. If
	// the passes disagree about what they read, the artifact says so rather
	// than quoting the first pass as if it were the whole truth.
	out.Corpus.Files = envs[0].Corpus.Files
	out.Corpus.Bytes = envs[0].Corpus.Bytes
	out.Corpus.From = envs[0].Corpus.From
	out.Corpus.To = envs[0].Corpus.To
	for _, env := range envs[1:] {
		if env.Corpus.Files != out.Corpus.Files || env.Corpus.Bytes != out.Corpus.Bytes {
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"report: corpus changed while the report ran — %s read %d files / %d bytes, %s read %d / %d",
				envs[0].Command, out.Corpus.Files, out.Corpus.Bytes,
				env.Command, env.Corpus.Files, env.Corpus.Bytes))
		}
	}
	return out
}

// RenderReport writes the Markdown half of the artifact. Everything a third
// party needs to re-run it is in the header; every section below is the
// verbatim output of the command that produced it, so no number in this
// document exists only here.
//
// generated is passed in rather than read from the clock so that two runs of
// this function over the same corpus differ in exactly one line.
func RenderReport(w io.Writer, r Report, generated time.Time) error {
	env := r.Envelope
	fmt.Fprintf(w, "# tare report\n\n")
	fmt.Fprintf(w, "| field | value |\n| --- | --- |\n")
	fmt.Fprintf(w, "| generated | %s |\n", generated.Format(time.RFC3339))
	fmt.Fprintf(w, "| tool | %s %s |\n", env.Tool, env.Version)
	fmt.Fprintf(w, "| corpus | `%s` |\n", env.Corpus.Dir)
	fmt.Fprintf(w, "| files | %s |\n", comma(int64(env.Corpus.Files)))
	fmt.Fprintf(w, "| bytes | %s |\n", comma(env.Corpus.Bytes))
	fmt.Fprintf(w, "| date range | %s .. %s |\n", env.Corpus.From, env.Corpus.To)
	fmt.Fprintf(w, "| claude code versions | %s |\n", strings.Join(cliVersions(r.Scan), ", "))
	fmt.Fprintf(w, "| reproduce | `tare report --dir %s` |\n", env.Corpus.Dir)
	fmt.Fprintf(w, "| machine-readable | `tare report --json --dir %s` |\n", env.Corpus.Dir)

	fmt.Fprintf(w, "\nEvery row below is tagged `measured` or `estimated`; an estimated row names\n"+
		"the method it was derived by. See Derivations. The tables are capped at %d rows\n"+
		"per dimension — `--json` carries all %s of them.\n",
		tableRows, comma(int64(len(env.Metrics))))

	for _, sec := range []struct {
		title  string
		env    Envelope
		render func(io.Writer, Envelope) error
	}{
		{"Corpus inventory", r.Scan, RenderScan},
		{"Per-tool byte attribution", r.Tools, RenderTools},
		{"Token attribution and context re-billing", r.Attribute, RenderAttribute},
		{"Corruption detection", r.Corruption, RenderCorruption},
	} {
		fmt.Fprintf(w, "\n## %s — `tare %s`\n\n```text\n", sec.title, sec.env.Command)
		if err := sec.render(w, sec.env); err != nil {
			return err
		}
		fmt.Fprint(w, "```\n")
	}

	fmt.Fprint(w, "\n## Derivations\n\n| metric | derivation | method | rows |\n| --- | --- | --- | --- |\n")
	for _, d := range derivations(env.Metrics) {
		method := d.key.method
		if method == "" {
			method = "—"
		}
		fmt.Fprintf(w, "| %s | %s | %s | %d |\n", d.key.name, d.key.derivation, method, d.rows)
	}

	fmt.Fprint(w, "\n## Warnings\n\n")
	if len(env.Warnings) == 0 {
		fmt.Fprint(w, "None.\n")
		return nil
	}
	for _, warn := range env.Warnings {
		fmt.Fprintf(w, "- %s\n", warn)
	}
	return nil
}

// cliVersions lists the Claude Code versions the corpus was written by.
func cliVersions(scan Envelope) []string {
	var out []string
	for _, m := range scan.Metrics {
		if m.Dimension == "cli_version" {
			out = append(out, m.Key)
		}
	}
	if len(out) == 0 {
		return []string{"none recorded"}
	}
	return out
}

// derivKey is one line of the legend; derivRow is that line with its count.
type derivKey struct{ name, derivation, method string }

type derivRow struct {
	key  derivKey
	rows int
}

// derivations rolls the flat metric list up into the legend: which metric was
// measured, which was estimated, and by what method. A name that appears with
// both derivations gets a line for each, because that would be the finding.
func derivations(metrics []Metric) []derivRow {
	counts := map[derivKey]int{}
	for _, m := range metrics {
		k := derivKey{name: m.Name, derivation: m.Derivation}
		if m.Method != nil {
			k.method = *m.Method
		}
		counts[k]++
	}
	keys := slices.SortedFunc(maps.Keys(counts), func(a, b derivKey) int {
		return strings.Compare(a.name+"\x00"+a.method, b.name+"\x00"+b.method)
	})
	out := make([]derivRow, 0, len(keys))
	for _, k := range keys {
		out = append(out, derivRow{k, counts[k]})
	}
	return out
}
