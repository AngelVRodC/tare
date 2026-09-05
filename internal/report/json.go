// Package report turns a transcript scan into metrics, a human table and the
// `--json` envelope every tare subcommand emits.
package report

import (
	"encoding/json"
	"fmt"
	"io"
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
