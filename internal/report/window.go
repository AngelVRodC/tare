package report

import (
	"fmt"
	"strconv"
	"strings"
)

// Window is the --since/--until time window a run measures within. The zero
// value is the unwindowed run: every event counts, every check passes.
//
// Bounds are lexical, never parsed. A bound is either a bare date
// YYYY-MM-DD (10 chars) or a full UTC RFC3339 timestamp
// YYYY-MM-DDTHH:MM:SS.sssZ (24 chars), and every comparison is a plain
// string compare — RFC3339 sorts lexically, so no time parsing happens
// anywhere in this program (the same invariant scan.go's day() relies on).
type Window struct {
	since     string
	until     string
	untilBare bool
}

// NewWindow validates both bounds lexically and returns the window. An empty
// bound means unset. A malformed bound is a loud error naming the two
// accepted shapes, because main.go returns it before any corpus is read.
func NewWindow(since, until string) (Window, error) {
	if since != "" {
		if err := checkBound("--since", since); err != nil {
			return Window{}, err
		}
	}
	if until != "" {
		if err := checkBound("--until", until); err != nil {
			return Window{}, err
		}
	}
	return Window{since: since, until: until, untilBare: until != "" && len(until) == 10}, nil
}

// checkBound accepts exactly two shapes and nothing else: 10 chars, digits
// with '-' at 4 and 7; or 24 chars, digits with fixed '-', 'T', ':', ':',
// '.', 'Z' at 4, 7, 10, 13, 16, 19, 23. The character-set check is all the
// validation there is — no time.Parse, and no quote or NULL-producing byte
// can reach the SQL composition the OpenCode adapter builds from a bound.
func checkBound(flag, v string) error {
	shaped := func(fixed map[int]byte) bool {
		for i := 0; i < len(v); i++ {
			if f, ok := fixed[i]; ok {
				if v[i] != f {
					return false
				}
				continue
			}
			if v[i] < '0' || v[i] > '9' {
				return false
			}
		}
		return true
	}
	switch len(v) {
	case 10:
		if shaped(map[int]byte{4: '-', 7: '-'}) {
			return nil
		}
	case 24:
		if shaped(map[int]byte{4: '-', 7: '-', 10: 'T', 13: ':', 16: ':', 19: '.', 23: 'Z'}) {
			return nil
		}
	}
	return fmt.Errorf("%s accepts a bare date YYYY-MM-DD or full UTC RFC3339 YYYY-MM-DDTHH:MM:SS.sssZ, got %q", flag, v)
}

// Active reports whether any bound is set.
func (w Window) Active() bool { return w.since != "" || w.until != "" }

// Includes reports whether a timestamp falls inside the window, by plain
// string comparison. Semantics per time-window-4: an inactive window includes
// everything; an active one excludes the empty timestamp (an untimestamped
// event has no position in a window, and the caller reports such types as
// unavailable rather than zero); a bare-date --since compares plain
// ts >= since, so the whole named day is included; a bare-date --until
// compares day(ts) <= until, so the whole named day is included too — the
// since/until asymmetry is by design; full-precision bounds are inclusive
// at both ends.
func (w Window) Includes(ts string) bool {
	if !w.Active() {
		return true
	}
	if ts == "" {
		return false
	}
	if w.since != "" && ts < w.since {
		return false
	}
	if w.until != "" {
		if w.untilBare {
			if day(ts) > w.until {
				return false
			}
		} else if ts > w.until {
			return false
		}
	}
	return true
}

// Since returns the --since bound, empty when unset.
func (w Window) Since() string { return w.since }

// Until returns the --until bound, empty when unset.
func (w Window) Until() string { return w.until }

// UntilBare reports whether --until was a bare date, which windows by whole
// days rather than by instant.
func (w Window) UntilBare() bool { return w.untilBare }

// windowC1Warning is the one observational warning every windowed envelope
// carries (time-window-9). It names the four things a reader needs before
// trusting a windowed figure: what the window means (a), what stayed
// corpus-wide (b), which measured event types carry no timestamp and are
// therefore excluded rather than zeroed (c), and that cost joins the window
// at session granularity (d). Enforcement is render tests, not
// Envelope.Validate — no window field is added to Validate.
//
// untimestamped arrives from untimestampedTypes, already sorted, so the
// string is byte-identical run to run and TestReportReproducible holds.
func windowC1Warning(w Window, untimestamped []string) string {
	bound := "timestamp <= " + w.Until()
	if w.Since() != "" {
		bound = "timestamp >= " + w.Since()
		if w.Until() != "" {
			bound += " <= " + w.Until()
		}
	}
	untimestampedNote := "every measured event type carries a timestamp"
	if len(untimestamped) > 0 {
		untimestampedNote = "event types carrying no timestamp are excluded from windowed counts " +
			"and reported unavailable, not zero: " + strings.Join(untimestamped, ", ")
	}
	return "this run measures only events with " + bound + " — a lexical subset of the corpus; " +
		"corpus totals (files, bytes, parse errors) and join-integrity counters (unmatched_results) " +
		"remain whole-corpus; " + untimestampedNote +
		"; cost is session-granular — a session whose startTime falls inside the window is billed wholly to it"
}

// msPart extracts the milliseconds digits of a full-precision bound. The
// OpenCode adapter composes `time_created <= strftime(...)*1000 + <ms>` from
// it, because strftime truncates the fraction — and the lexical compare does
// not. A bare date has no fraction, so 0. Callers pass validated bounds; the
// Atoi cannot fail on digits checkBound already accepted.
func msPart(v string) int {
	if len(v) != 24 {
		return 0
	}
	n, err := strconv.Atoi(v[20:23])
	if err != nil {
		return 0
	}
	return n
}
