package report

import (
	"cmp"
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

// BuildReport runs every command and merges the results. The window threads
// into every pass: a windowed report is four windowed envelopes, and the
// merged envelope carries the bounds via envs[0].
//
// progress names each pass as it starts; nil is silent. It is a separate
// writer from the one the report is rendered to on purpose — the caller sends
// it to stderr so a redirect still yields a file that is only the report.
func BuildReport(dir, version string, w Window, progress io.Writer) (Report, error) {
	var (
		r   Report
		err error
	)
	// Announced before the pass runs, not after: the whole point is that
	// something appears within 100 ms, and the first pass is the slow one.
	say := func(pass string) {
		if progress != nil {
			fmt.Fprintln(progress, pass)
		}
	}
	say("scanning…")
	if r.Scan, err = ScanEnvelope(dir, version, w); err != nil {
		return Report{}, err
	}
	say("tools…")
	if r.Tools, err = ToolsEnvelope(dir, version, w); err != nil {
		return Report{}, err
	}
	say("attribute…")
	if r.Attribute, err = AttributeEnvelope(dir, version, w); err != nil {
		return Report{}, err
	}
	say("corruption…")
	if r.Corruption, err = CorruptionEnvelope(dir, version, w); err != nil {
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
		Tool:          "tare",
		SchemaVersion: SchemaVersion,
		Version:       version,
		Command:       "report",
		Corpus:        Corpus{Dir: dir},
		Metrics:       []Metric{},
		Warnings:      []string{},
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
	out.Corpus.Since = envs[0].Corpus.Since
	out.Corpus.Until = envs[0].Corpus.Until
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

	// Ahead of the preamble, not after it: the preamble is a legend — how to
	// read the tables — and a reader who stops after the first screen should
	// leave with the finding rather than with the instructions for finding it.
	fmt.Fprint(w, "\n## Summary\n\n")
	lines := summary(env)
	if len(lines) == 0 {
		// Consistent with Warnings and with the rest of this codebase: an
		// absent number is said out loud, never printed as a zero.
		fmt.Fprint(w, "None available: the envelope carries no corpus-wide rows.\n")
	}
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}

	fmt.Fprintf(w, "\nEvery row below carries a `measured` or `estimated` derivation. A block whose\n"+
		"rows all agree says so once, in a footer under it; the column appears only\n"+
		"where a block genuinely mixes the two. `--json` tags every row. An estimated row names\n"+
		"the method it was derived by. See Derivations. Every table below prints every row —\n"+
		"a terminal caps them at a screenful, a file has no reason to — and `--json` carries\n"+
		"the same %s metric rows behind them.\n",
		comma(int64(len(env.Metrics))))

	for _, sec := range []struct {
		title  string
		env    Envelope
		render func(io.Writer, Envelope) error
	}{
		// Findings first, in the order the Summary states them. A cap of 0
		// truncates nothing: truncation is a terminal convenience and this
		// document is a file by definition. Only the two renderers that
		// truncate take the cap, and a closure carries it so the other two
		// keep a signature with no parameter they would ignore.
		{"Per-tool byte attribution", r.Tools, RenderTools},
		{"Token attribution and context re-billing", r.Attribute,
			func(w io.Writer, e Envelope) error { return RenderAttribute(w, e, 0) }},
		{"Corruption detection", r.Corruption,
			func(w io.Writer, e Envelope) error { return RenderCorruption(w, e, 0) }},
		// Demoted below the three findings: it answers "what was read", which
		// is provenance a reader checks after being told what was found.
		{"Corpus inventory", r.Scan, RenderScan},
	} {
		fmt.Fprintf(w, "\n## %s — `tare %s`\n\n```text\n", sec.title, sec.env.Command)
		if err := sec.render(w, sec.env); err != nil {
			return err
		}
		fmt.Fprint(w, "```\n")
	}

	fmt.Fprint(w, "\n## Warnings\n\n")
	if len(env.Warnings) == 0 {
		fmt.Fprint(w, "None.\n")
	}
	for _, warn := range env.Warnings {
		fmt.Fprintf(w, "- %s\n", warn)
	}

	// Last, because it is an appendix: ninety rows of per-metric bookkeeping
	// nobody reads top-down, kept because the derivation of every figure above
	// has to be checkable somewhere.
	fmt.Fprint(w, "\n## Derivations\n\n| metric | derivation | method | rows |\n| --- | --- | --- | --- |\n")
	for _, d := range derivations(env.Metrics) {
		method := d.key.method
		if method == "" {
			method = "—"
		}
		fmt.Fprintf(w, "| %s | %s | %s | %d |\n", d.key.name, d.key.derivation, method, d.rows)
	}
	return nil
}

// summary is the headline block: at most six lines, each one a figure a reader
// of `--json` can check for themselves.
//
// Two rules shape it. First, no number in this document may exist only here —
// so a line is either one row read straight off the envelope, or a ratio of
// named rows, and a derived line names the rows it divided so the arithmetic is
// reproducible from `--json`. A figure the envelope cannot corroborate is not
// printed at all.
//
// Second, it must be deterministic. TestReportMarkdownVariesOnlyByTimestamp
// allows exactly one differing line between two renders of one report, so the
// top-3 is sorted by a total order (context descending, then key) rather than
// trusted to arrive pre-sorted, and the three rows are summed as int64. Summing
// floats over a Go map range is the defect that test was written to catch.
//
// It reads the merged envelope rather than the four section envelopes on
// purpose: the merged one is exactly what `tare report --json` prints, so
// "corroborable from the JSON" is literal rather than approximate.
func summary(env Envelope) []string {
	out := make([]string, 0, 6)

	// Tool concentration. A sum of three rows, so the line names all three.
	if whole := corpusValue(env, "context_bytes"); whole > 0 {
		rows := groupRows(env.Metrics, "tool")
		slices.SortFunc(rows, func(a, b row) int {
			return cmp.Or(
				cmp.Compare(b.num("context_bytes"), a.num("context_bytes")),
				strings.Compare(a.key, b.key))
		})
		rows = rows[:min(3, len(rows))]
		var part int64
		keys := make([]string, 0, len(rows))
		for _, r := range rows {
			part += r.num("context_bytes")
			keys = append(keys, "`"+r.key+"`")
		}
		if len(keys) > 0 {
			out = append(out, fmt.Sprintf(
				"- **Tool concentration** — %s together return %s of every byte tools put into context "+
					"(their `context_bytes` rows at dimension `tool`, over corpus `context_bytes`).",
				strings.Join(keys, ", "), formatValue(percent(part, whole), "percent")))
		}
	}

	// The four single-row lines. Each is one envelope row, printed by the same
	// formatter the tables use, so the Summary and the section cannot disagree.
	if m, ok := corpusMetric(env, "rebill_multiplier"); ok {
		out = append(out, fmt.Sprintf(
			"- **Context re-billing** — the corpus re-billed %s as many tokens as it sent fresh "+
				"(corpus `rebill_multiplier`).", formatValue(m.Value, m.Unit)))
	}
	if m, ok := corpusMetric(env, "duplicate_rate_percent"); ok {
		out = append(out, fmt.Sprintf(
			"- **Response duplication** — %s of assistant responses repeat a message already counted "+
				"(corpus `duplicate_rate_percent`).", formatValue(m.Value, m.Unit)))
	}
	if m, ok := corpusMetric(env, "attachment_share_percent"); ok {
		out = append(out, fmt.Sprintf(
			"- **Attachment share** — attachments are %s of the corpus on disk "+
				"(corpus `attachment_share_percent`).", formatValue(m.Value, m.Unit)))
	}
	// Both halves or neither: the allocated figure alone reads as the total
	// spend, which is the one thing it is not.
	alloc, haveAlloc := corpusMetric(env, "allocated_cost_usd")
	unalloc, haveUnalloc := corpusMetric(env, "unallocated_cost_usd")
	if haveAlloc && haveUnalloc {
		out = append(out, fmt.Sprintf(
			"- **Measured spend** — %s allocated to a dimension, %s billed with no events to allocate it across "+
				"(corpus `allocated_cost_usd`, `unallocated_cost_usd`).",
			formatValue(alloc.Value, alloc.Unit), formatValue(unalloc.Value, unalloc.Unit)))
	}

	// Attribution coverage, reported for the thinnest dimension because the
	// most-unclaimed one is the bound on every dollar figure below it. Phase 2
	// deliberately added no metric row for this share, so it is derived here
	// from the two rows the line names.
	if whole := corpusValue(env, "rebilled_tokens"); whole > 0 {
		if dim, part, ok := thinnestAttribution(env); ok {
			out = append(out, fmt.Sprintf(
				"- **Attribution coverage** — `%s` holds %s of `%s` re-billed tokens, the thinnest of the five "+
					"dimensions; the dollars beside its named rows are floors "+
					"(`rebilled_tokens` at `%s`/`%s`, over corpus `rebilled_tokens`).",
				unattributedKey, formatValue(percent(part, whole), "percent"), dim, dim, unattributedKey))
		}
	}
	return out
}

// corpusMetric reads one keyless corpus-wide row whole, and says whether it is
// there at all. corpusValue coerces to int64, which would silently render a
// ratio, a percentage or a dollar figure as 0 — and every Summary line but two
// is a float.
func corpusMetric(env Envelope, name string) (Metric, bool) {
	for _, m := range env.Metrics {
		if m.Dimension == "corpus" && m.Name == name {
			return m, true
		}
	}
	return Metric{}, false
}

// thinnestAttribution names the attribution dimension whose `(unattributed)`
// row holds the most re-billed tokens.
//
// attributionDims is ranged in declaration order — the order the tables
// themselves print in — and the comparison is strict, so a tie resolves to the
// first-declared dimension and the Summary line is byte-identical run to run.
// sessionDim is not among them by construction: its key is the raw session id
// and never the sentinel.
func thinnestAttribution(env Envelope) (dimension string, rebilled int64, ok bool) {
	unclaimed := map[string]int64{}
	for _, m := range env.Metrics {
		if m.Name == "rebilled_tokens" && m.Key == unattributedKey {
			unclaimed[m.Dimension] = asInt64(m.Value)
		}
	}
	for _, d := range attributionDims {
		if v := unclaimed[d.name]; v > rebilled {
			dimension, rebilled = d.name, v
		}
	}
	return dimension, rebilled, dimension != ""
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
