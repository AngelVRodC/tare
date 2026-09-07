// Package report turns a transcript scan into metrics, a human table and the
// `--json` envelope every tare subcommand emits.
package report

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/AngelVRodC/tare/internal/transcript"
)

// Derivation values. Every metric carries one; nothing is emitted untagged.
const (
	Measured  = "measured"
	Estimated = "estimated"
)

// Envelope is the one `--json` shape, emitted by every subcommand so a
// consumer (and the Phase 5 reproducibility gate) has a single contract.
type Envelope struct {
	Tool     string   `json:"tool"`
	Version  string   `json:"version"`
	Command  string   `json:"command"`
	Corpus   Corpus   `json:"corpus"`
	Metrics  []Metric `json:"metrics"`
	Warnings []string `json:"warnings"`
}

// Corpus identifies the input a run measured.
type Corpus struct {
	Dir   string `json:"dir"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	From  string `json:"from"`
	To    string `json:"to"`
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
}

func newBuilder(dir, version, command string) *builder {
	return &builder{env: Envelope{
		Tool:     "tare",
		Version:  version,
		Command:  command,
		Corpus:   Corpus{Dir: dir},
		Metrics:  []Metric{},
		Warnings: []string{},
	}}
}

// seeTime widens the corpus date range to include one timestamp. RFC3339 sorts
// lexically, so no time parsing happens anywhere in this program.
//
// It takes the timestamp rather than the event so a harness that has no
// transcript.Event — one whose timestamps are integers it formats rather than
// lines it parses — widens the same range without a second implementation.
func (b *builder) seeTime(ts string) {
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

// rows appends already-built rows, keeping the order they were emitted in.
func (b *builder) rows(m ...Metric) {
	b.env.Metrics = append(b.env.Metrics, m...)
}

// warn records what the command could not compute. Nothing is ever omitted
// silently; an absent number is a warning, not a zero.
func (b *builder) warn(format string, args ...any) {
	b.env.Warnings = append(b.env.Warnings, fmt.Sprintf(format, args...))
}

// done seals the envelope with what the scan measured. Parse errors are
// appended here, which is why they are always the last warning.
func (b *builder) done(stats transcript.ScanStats) Envelope {
	b.env.Corpus.Files = stats.Files
	b.env.Corpus.Bytes = stats.Bytes
	b.env.Corpus.From = day(b.from)
	b.env.Corpus.To = day(b.to)
	if stats.ParseErrors > 0 {
		b.warn("%d lines failed to decode", stats.ParseErrors)
	}
	return b.env
}
