package report

import (
	"strings"
	"testing"
)

// TestNewWindowAcceptsValidBounds pins the only two shapes time-window-2
// accepts: a 10-char bare date and a 24-char UTC RFC3339 timestamp with
// milliseconds. Validation is a character-set check, never time.Parse.
func TestNewWindowAcceptsValidBounds(t *testing.T) {
	for _, tc := range []struct {
		name, since, until string
	}{
		{"bare dates", "2026-08-01", "2026-08-15"},
		{"full precision", "2026-08-01T00:00:00.000Z", "2026-08-15T23:59:59.999Z"},
		{"mixed shapes", "2026-08-01", "2026-08-15T12:00:00.500Z"},
		{"since only", "2026-08-01", ""},
		{"until only", "", "2026-08-15T12:00:00.500Z"},
		{"unwindowed", "", ""},
	} {
		w, err := NewWindow(tc.since, tc.until)
		if err != nil {
			t.Errorf("%s: NewWindow(%q, %q) returned %v", tc.name, tc.since, tc.until, err)
			continue
		}
		if w.Since() != tc.since || w.Until() != tc.until {
			t.Errorf("%s: accessors = %q/%q, want %q/%q", tc.name, w.Since(), w.Until(), tc.since, tc.until)
		}
		if want := tc.since != "" || tc.until != ""; w.Active() != want {
			t.Errorf("%s: Active() = %v, want %v", tc.name, w.Active(), want)
		}
		if want := tc.until != "" && len(tc.until) == 10; w.UntilBare() != want {
			t.Errorf("%s: UntilBare() = %v, want %v", tc.name, w.UntilBare(), want)
		}
	}

	// msPart extracts the milliseconds of a full-precision bound (used by the
	// OpenCode SQL composition in T5); a bare date has no fraction.
	if got := msPart("2026-08-15T12:00:00.500Z"); got != 500 {
		t.Errorf("msPart = %d, want 500", got)
	}
	if got := msPart("2026-08-15"); got != 0 {
		t.Errorf("msPart of a bare date = %d, want 0", got)
	}
}

// TestNewWindowRejectsInvalidBounds pins the rejected shapes: the 20-char
// RFC3339 without milliseconds, an offset instead of Z, an embedded quote
// (the SQL-injection shape), and plain garbage. The error must name BOTH
// accepted shapes and which flag was rejected.
func TestNewWindowRejectsInvalidBounds(t *testing.T) {
	cases := map[string]string{
		"20-char RFC3339 without millis": "2026-08-01T12:00:00Z",
		"offset instead of Z":            "2026-08-01T00:00:00+03:00",
		"embedded quote":                 "2026-08-0'",
		"garbage":                        "garbage",
		"short date":                     "2026-8-1",
		"empty-looking spaces":           "2026-08-15 ",
	}
	for name, bound := range cases {
		for _, flagName := range []string{"--since", "--until"} {
			since, until := bound, bound
			if flagName == "--since" {
				until = ""
			} else {
				since = ""
			}
			_, err := NewWindow(since, until)
			if err == nil {
				t.Errorf("%s as %s: NewWindow accepted %q", name, flagName, bound)
				continue
			}
			for _, want := range []string{flagName, "YYYY-MM-DD", "YYYY-MM-DDTHH:MM:SS.sssZ"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s as %s: error %q does not name %q", name, flagName, err, want)
				}
			}
		}
	}
}

// TestWindowIncludesSemantics pins time-window-4's lexical comparison rules:
// inactive includes everything; an active window excludes the empty
// timestamp; bare-date since uses plain ts >= since (the whole named day is
// in); bare-date until uses day(ts) <= until (the whole named day is in —
// the asymmetry is by design); full-precision bounds are inclusive both ends.
func TestWindowIncludesSemantics(t *testing.T) {
	inactive := Window{}
	if !inactive.Includes("2026-08-15T12:00:00.000Z") || !inactive.Includes("") {
		t.Error("an inactive window includes everything, including the empty timestamp")
	}

	sinceBare, _ := NewWindow("2026-08-01", "")
	for ts, want := range map[string]bool{
		"2026-08-01T00:00:00.000Z": true, // plain ts >= since: day start is in
		"2026-08-01T23:59:59.999Z": true,
		"2026-07-31T23:59:59.999Z": false,
	} {
		if got := sinceBare.Includes(ts); got != want {
			t.Errorf("bare since: Includes(%q) = %v, want %v", ts, got, want)
		}
	}

	untilBare, _ := NewWindow("", "2026-08-15")
	for ts, want := range map[string]bool{
		"2026-08-15T00:00:00.000Z": true, // day(ts) <= until: whole day is in
		"2026-08-15T23:59:59.999Z": true,
		"2026-08-16T00:00:00.000Z": false,
	} {
		if got := untilBare.Includes(ts); got != want {
			t.Errorf("bare until: Includes(%q) = %v, want %v", ts, got, want)
		}
	}

	untilFull, _ := NewWindow("", "2026-08-15T12:00:00.500Z")
	for ts, want := range map[string]bool{
		"2026-08-15T12:00:00.500Z": true, // full-precision until is inclusive
		"2026-08-15T12:00:00.501Z": false,
	} {
		if got := untilFull.Includes(ts); got != want {
			t.Errorf("full until: Includes(%q) = %v, want %v", ts, got, want)
		}
	}

	both, _ := NewWindow("2026-08-01T00:00:00.000Z", "2026-08-15T12:00:00.500Z")
	for ts, want := range map[string]bool{
		"2026-08-01T00:00:00.000Z": true, // since inclusive
		"2026-07-31T23:59:59.999Z": false,
		"2026-08-15T12:00:00.500Z": true, // until inclusive
		"2026-08-15T12:00:00.501Z": false,
	} {
		if got := both.Includes(ts); got != want {
			t.Errorf("both bounds: Includes(%q) = %v, want %v", ts, got, want)
		}
	}

	if !both.Includes("") == false {
		t.Error("an active window must exclude the empty timestamp")
	}
}
