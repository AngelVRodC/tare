// Package report turns a transcript scan into metrics, a human table and the
// `--json` envelope every tare subcommand emits.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/AngelVRodC/tare/internal/transcript"
)

// Derivation values. Every metric carries one; nothing is emitted untagged.
const (
	Measured  = "measured"
	Estimated = "estimated"
)

// SchemaVersion is the version of the envelope shape on the wire. Absence of
// the field means the pre-0.3.0 shape; 1 means the 0.3.0 shape (which added
// the `microdollars` unit). Contract: any future change to the envelope shape
// or unit semantics bumps this in the same release — never ships under an
// unchanged value. It must land before, never after, the shape change.
const SchemaVersion = 1

// Envelope is the one `--json` shape, emitted by every subcommand so a
// consumer (and the Phase 5 reproducibility gate) has a single contract.
type Envelope struct {
	Tool string `json:"tool"`
	// SchemaVersion is the SECOND field on purpose: struct order is
	// serialisation order, so its position is consumer-visible.
	SchemaVersion int      `json:"schema_version"`
	Version       string   `json:"version"`
	Command       string   `json:"command"`
	Corpus        Corpus   `json:"corpus"`
	Metrics       []Metric `json:"metrics"`
	Warnings      []string `json:"warnings"`
}

// Corpus identifies the input a run measured.
type Corpus struct {
	Dir   string `json:"dir"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	From  string `json:"from"`
	To    string `json:"to"`
	// Since/Until are the --since/--until bounds a windowed run was measured
	// with, empty when unset. Additive: consumers that ignore them stay
	// correct, and merge propagates them via envs[0].
	Since string `json:"since"`
	Until string `json:"until"`
}

// Metric is one flat row. Dimension plus Key say what it is about; Derivation
// says how it was arrived at, and an estimated row also names its Method.
type Metric struct {
	Name       string  `json:"name"`
	Dimension  string  `json:"dimension"`
	Key        string  `json:"key"`
	Value      any     `json:"value"`
	Unit       string  `json:"unit"`
	Derivation string  `json:"derivation"`
	Method     *string `json:"method"`
}

// MeasuredMetric builds a row read straight off the corpus.
func MeasuredMetric(name, dimension, key string, value any, unit string) Metric {
	return Metric{
		Name:       name,
		Dimension:  dimension,
		Key:        key,
		Value:      value,
		Unit:       unit,
		Derivation: Measured,
	}
}

// EstimatedMetric builds a row that was derived rather than read, and names
// the method it was derived by. Validate rejects one with no method.
func EstimatedMetric(name, dimension, key string, value any, unit, method string) Metric {
	return Metric{
		Name:       name,
		Dimension:  dimension,
		Key:        key,
		Value:      value,
		Unit:       unit,
		Derivation: Estimated,
		Method:     &method,
	}
}

// Validate rejects an envelope that breaks the contract: an unknown or missing
// derivation, or an estimated row with no method named. Anything a command
// could not compute belongs in Warnings, never omitted silently.
func (e Envelope) Validate() error {
	for i, m := range e.Metrics {
		switch m.Derivation {
		case Measured:
		case Estimated:
			if m.Method == nil || *m.Method == "" {
				return fmt.Errorf("metric %d (%s/%s): estimated with no method named", i, m.Name, m.Key)
			}
		default:
			return fmt.Errorf("metric %d (%s/%s): derivation %q is neither %q nor %q",
				i, m.Name, m.Key, m.Derivation, Measured, Estimated)
		}
	}
	return nil
}

// WriteJSON validates the envelope and encodes it.
func WriteJSON(w io.Writer, env Envelope) error {
	if err := env.Validate(); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(env)
}

// builder accumulates one command's envelope.
//
// Every command builds the same header, widens the same corpus date range as
// it streams, and reports parse errors the same way. This is that, once — four
// copies of it drifted apart the moment one of them gained a field.
type builder struct {
	env      Envelope
	from, to string
	win      Window
	matched  bool
	// typeSeen counts every event per top-level type; typeTS counts those
	// whose timestamp was present. Their difference names the types a window
	// cannot place, which time-window-3 reports as unavailable, never zero.
	typeSeen, typeTS map[string]int64
}

func newBuilder(dir, version, command string, w Window) *builder {
	return &builder{
		env: Envelope{
			Tool:          "tare",
			SchemaVersion: SchemaVersion,
			Version:       version,
			Command:       command,
			Corpus:        Corpus{Dir: dir},
			Metrics:       []Metric{},
			Warnings:      []string{},
		},
		win:      w,
		typeSeen: map[string]int64{},
		typeTS:   map[string]int64{},
	}
}

// seeTime widens the corpus date range to include one timestamp. RFC3339 sorts
// lexically, so no time parsing happens anywhere in this program.
//
// It takes the timestamp rather than the event so a harness that has no
// transcript.Event — one whose timestamps are integers it formats rather than
// lines it parses — widens the same range without a second implementation.
//
// The type argument feeds the window, not the date range: a type seen only
// without timestamps cannot be placed in a window, and done() names it in the
// C1 warning rather than counting it as zero.
func (b *builder) seeTime(ts, evType string) {
	b.typeSeen[evType]++
	if ts != "" {
		b.typeTS[evType]++
		if b.win.Includes(ts) {
			b.matched = true
		}
	}
	if ts == "" {
		return
	}
	if b.from == "" || ts < b.from {
		b.from = ts
	}
	if b.to == "" || ts > b.to {
		b.to = ts
	}
}

// add appends one keyless corpus-wide measured row.
func (b *builder) add(name string, value any, unit string) {
	b.env.Metrics = append(b.env.Metrics, MeasuredMetric(name, "corpus", "", value, unit))
}

// addWin appends one keyless windowed measured row. Under a window that
// matched no events it is omitted, never zeroed (time-window-8): a zero row
// would be a `measured` claim that the window emptied, which Validate accepts
// unconditionally. Corpus rows — still true under any window — go through
// add, which appends unconditionally.
func (b *builder) addWin(name string, value any, unit string) {
	if b.win.Active() && !b.matched {
		return
	}
	b.add(name, value, unit)
}

// untimestampedTypes names the measured event types this run saw that never
// carried a timestamp. A window cannot place them, so windowed metrics omit
// them and the C1 warning names them as unavailable, not zero. The empty type
// is skipped: it is what a harness without an Event passes (OpenCode formats
// millisecond integers rather than parsing lines), not a measured type.
// Sorted, never map order — the warning string must be byte-identical run to
// run, which is what TestReportReproducible pins.
func (b *builder) untimestampedTypes() []string {
	var out []string
	for _, t := range slices.Sorted(maps.Keys(b.typeSeen)) {
		if t == "" || b.typeSeen[t] == 0 || b.typeTS[t] > 0 {
			continue
		}
		out = append(out, t)
	}
	return out
}

// rows appends already-built rows, keeping the order they were emitted in.
func (b *builder) rows(m ...Metric) {
	b.env.Metrics = append(b.env.Metrics, m...)
}

// warn records what the command could not compute. Nothing is ever omitted
// silently; an absent number is a warning, not a zero.
func (b *builder) warn(format string, args ...any) {
	b.env.Warnings = append(b.env.Warnings, fmt.Sprintf(format, args...))
}

// done seals the envelope with what the scan measured.
//
// The C1 warning (time-window-9) and the empty-window warning are appended
// here, which makes done() the single emission point for every command,
// OpenCode included. Parse errors stay last, as they have always been.
func (b *builder) done(stats transcript.ScanStats) Envelope {
	b.env.Corpus.Files = stats.Files
	b.env.Corpus.Bytes = stats.Bytes
	b.env.Corpus.From = day(b.from)
	b.env.Corpus.To = day(b.to)
	b.env.Corpus.Since = b.win.Since()
	b.env.Corpus.Until = b.win.Until()
	if b.win.Active() {
		b.warn("%s", windowC1Warning(b.win, b.untimestampedTypes()))
		if !b.matched {
			b.warn("the window matched no events — windowed counts are omitted, not zeroed; " +
				"corpus totals remain whole-corpus")
		}
	}
	if stats.ParseErrors > 0 {
		b.warn("%d lines failed to decode", stats.ParseErrors)
	}
	return b.env
}
