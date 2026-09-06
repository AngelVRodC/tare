package report

import "testing"

// TestFormatValueByUnit pins the shape of every unit the envelope carries. The
// unit decides the rendering, so a regression here is a regression in every
// table at once.
func TestFormatValueByUnit(t *testing.T) {
	cases := []struct {
		unit  string
		value any
		want  string
	}{
		// bytes: SI, and int and int64 must agree.
		{"bytes", int64(254427880), "254.4 MB"},
		{"bytes", int64(0), "0 B"},
		{"bytes", int64(999), "999 B"},
		{"bytes", int64(1000), "1.0 kB"},
		{"bytes", 1500, "1.5 kB"},

		// percent: one decimal, never six significant figures.
		{"percent", 24.813621, "24.8%"},
		{"percent", 100.0, "100.0%"},
		{"percent", 0.088123, "0.1%"},
		{"percent", 0.0, "0.0%"},

		// usd: money to the cent, except below a cent, where rounding to
		// $0.00 would delete a real allocation.
		{"usd", 813.0334, "$813.03"},
		{"usd", 0.07878235507735767, "$0.08"},
		{"usd", 0.0000149, "$0.0000149"},
		{"usd", 0.0, "$0.00"},

		// ratio always carries the ×; only the precision scales. A
		// rebill_multiplier below 1 is real and must survive rounding.
		{"ratio", 0.5169169885527817, "0.52×"},
		{"ratio", 0.664, "0.66×"},
		{"ratio", 1.0, "1.0×"},
		{"ratio", 0.0, "0.00×"},
		{"ratio", 2.7, "2.7×"},
		{"ratio", 28.1045, "28×"},
		{"ratio", 17070.5, "17,071×"},

		// Every other unit keeps the pre-existing rendering.
		{"tokens", int64(14356), "14,356"},
		{"events", 60, "60"},
		{"sessions", int64(-3), "-3"},
		{"source", "json", "json"},
		{"", nil, "<nil>"},
	}
	for _, c := range cases {
		if got := formatValue(c.value, c.unit); got != c.want {
			t.Errorf("formatValue(%v, %q) = %q, want %q", c.value, c.unit, got, c.want)
		}
	}
}

// TestHumanizeBytesIsSI is the guard against the most common humanization bug
// there is: dividing by 1024 and labelling the result MB. tare measures bytes
// on disk, so it divides by 1000 and the two systems are never mixed.
func TestHumanizeBytesIsSI(t *testing.T) {
	if got := humanizeBytes(254427880); got != "254.4 MB" {
		t.Errorf("humanizeBytes(254427880) = %q, want %q (SI, not the IEC 242.6 MiB)", got, "254.4 MB")
	}
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},     // the B/kB boundary, below
		{1000, "1.0 kB"},   // and above
		{999999, "1.0 MB"}, // promotes on the rounded value, not the raw one
		{1000000, "1.0 MB"},
		{1500000000, "1.5 GB"},
		{2000000000000, "2.0 TB"},
		{-1500, "-1.5 kB"},
	}
	for _, c := range cases {
		if got := humanizeBytes(c.in); got != c.want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestHumanizeBytesPromotesOnRoundedValue guards the boundary the scale loop
// gets wrong if it tests the raw quotient: 999,999 B is 999.999 kB, which
// prints as "1000.0 kB" unless it is promoted first.
func TestHumanizeBytesPromotesOnRoundedValue(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{999949, "999.9 kB"}, // rounds to 999.9 — stays kB
		{999950, "1.0 MB"},   // rounds to 1000.0 — promotes
		{999999999, "1.0 GB"},
	} {
		if got := humanizeBytes(c.in); got != c.want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
