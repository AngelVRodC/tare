package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureCorpus builds a ~/.claude-shaped tree: a projects dir with two
// transcripts plus the sibling stats-cache.json the retention gap needs.
func fixtureCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "projects")
	if err := os.MkdirAll(filepath.Join(dir, "p", "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("p", "a.jsonl"), strings.Join([]string{
		`{"type":"user","sessionId":"s1","version":"2.1.260","timestamp":"2026-08-06T10:00:00.000Z"}`,
		`{"type":"assistant","sessionId":"s1","version":"2.1.261","timestamp":"2026-09-05T10:00:00.000Z"}`,
		`{"type":"quantum-flux","sessionId":"s1"}`,
	}, "\n")+"\n")
	write(filepath.Join("p", "subagents", "b.jsonl"),
		`{"type":"assistant","sessionId":"s1","agentId":"a1","isSidechain":true,"version":"2.1.261","timestamp":"2026-08-20T10:00:00.000Z"}`+"\n")
	if err := os.WriteFile(filepath.Join(root, "stats-cache.json"),
		[]byte(`{"totalSessions":5}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestEnvelopeDerivation is the phase gate: every row of every `--json`
// envelope carries a derivation, and an estimated row names its method.
func TestEnvelopeDerivation(t *testing.T) {
	env, err := ScanEnvelope(fixtureCorpus(t), "test")
	if err != nil {
		t.Fatalf("ScanEnvelope: %v", err)
	}
	if len(env.Metrics) == 0 {
		t.Fatal("no metrics emitted")
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// Assert on the encoded bytes, not the struct: that is what a consumer and
	// the Phase 5 jq gate actually see.
	var buf bytes.Buffer
	if err := WriteJSON(&buf, env); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var decoded struct {
		Metrics []map[string]any `json:"metrics"`
	}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	if len(decoded.Metrics) != len(env.Metrics) {
		t.Fatalf("encoded %d metrics, want %d", len(decoded.Metrics), len(env.Metrics))
	}
	for i, m := range decoded.Metrics {
		d, ok := m["derivation"].(string)
		if !ok || d == "" {
			t.Errorf("metric %d (%v) has no derivation", i, m["name"])
			continue
		}
		if d == Estimated && m["method"] == nil {
			t.Errorf("metric %d (%v) is estimated with method null", i, m["name"])
		}
	}
}

// TestValidateRejects proves the gate above can actually fail.
func TestValidateRejects(t *testing.T) {
	method := "some-method"
	cases := map[string]Metric{
		"missing derivation":  {Name: "x"},
		"bogus derivation":    {Name: "x", Derivation: "guessed"},
		"estimated no method": {Name: "x", Derivation: Estimated},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if err := (Envelope{Metrics: []Metric{m}}).Validate(); err == nil {
				t.Errorf("Validate accepted %+v", m)
			}
		})
	}
	ok := Envelope{Metrics: []Metric{
		MeasuredMetric("a", "corpus", "", 1, "files"),
		{Name: "b", Derivation: Estimated, Method: &method},
	}}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate rejected a valid envelope: %v", err)
	}
}

// TestScanEnvelopeCounts checks the inventory arithmetic against a fixture
// whose every number is known by hand.
func TestScanEnvelopeCounts(t *testing.T) {
	env, err := ScanEnvelope(fixtureCorpus(t), "test")
	if err != nil {
		t.Fatalf("ScanEnvelope: %v", err)
	}
	if env.Corpus.Files != 2 {
		t.Errorf("files = %d, want 2", env.Corpus.Files)
	}
	if env.Corpus.From != "2026-08-06" || env.Corpus.To != "2026-09-05" {
		t.Errorf("range = %s..%s, want 2026-08-06..2026-09-05", env.Corpus.From, env.Corpus.To)
	}

	want := map[string]any{
		"events":                4,
		"files_top_level":       1,
		"files_subagent":        1,
		"distinct_cli_versions": 2,
		"parse_errors":          0,
		"stats_cache_sessions":  5,
		"retention_gap":         4, // 5 cached sessions, 1 top-level transcript left
	}
	for _, m := range env.Metrics {
		if m.Dimension != "corpus" {
			continue
		}
		if exp, ok := want[m.Name]; ok {
			if m.Value != exp {
				t.Errorf("%s = %v, want %v", m.Name, m.Value, exp)
			}
			delete(want, m.Name)
		}
	}
	for name := range want {
		t.Errorf("metric %q was never emitted", name)
	}

	if len(env.Warnings) != 1 || !strings.Contains(env.Warnings[0], "quantum-flux") {
		t.Errorf("warnings = %v, want one naming the unknown type", env.Warnings)
	}
}

// TestRenderScanReadsEnvelope pins the table to the envelope, so the two can
// never report different numbers.
func TestRenderScanReadsEnvelope(t *testing.T) {
	env, err := ScanEnvelope(fixtureCorpus(t), "test")
	if err != nil {
		t.Fatalf("ScanEnvelope: %v", err)
	}
	var buf bytes.Buffer
	if err := RenderScan(&buf, env); err != nil {
		t.Fatalf("RenderScan: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"CORPUS", "TYPE", "quantum-flux", "measured", "warning:"} {
		if !strings.Contains(out, want) {
			t.Errorf("table is missing %q\n%s", want, out)
		}
	}
}

// TestDerivationColumnOnlyWhenMixed pins the rule that replaced a column
// printing "measured" on all 60 rows: the per-row tag renders only where a
// block genuinely mixes derivations, and a uniform block says it once.
func TestDerivationColumnOnlyWhenMixed(t *testing.T) {
	render := func(t *testing.T, metrics []Metric) string {
		t.Helper()
		var buf bytes.Buffer
		env := Envelope{Tool: "tare", Command: "scan", Metrics: metrics, Warnings: []string{}}
		if err := RenderScan(&buf, env); err != nil {
			t.Fatalf("RenderScan: %v", err)
		}
		return buf.String()
	}

	uniform := render(t, []Metric{
		MeasuredMetric("files", "corpus", "", int64(3), "files"),
		MeasuredMetric("bytes", "corpus", "", int64(9000), "bytes"),
	})
	if !strings.Contains(uniform, "all rows measured") {
		t.Errorf("uniform block should carry the footer\n%s", uniform)
	}
	// Two rows plus the footer: the word appears once, not once per row.
	if got := strings.Count(uniform, "measured"); got != 1 {
		t.Errorf("want 'measured' exactly once (the footer), got %d\n%s", got, uniform)
	}

	mixed := render(t, []Metric{
		MeasuredMetric("files", "corpus", "", int64(3), "files"),
		EstimatedMetric("cost_usd", "corpus", "", 1.5, "usd", "priced from cost-state"),
	})
	if strings.Contains(mixed, "all rows") {
		t.Errorf("mixed block must not collapse to a footer\n%s", mixed)
	}
	if !strings.Contains(mixed, "measured") || !strings.Contains(mixed, "estimated") {
		t.Errorf("mixed block must keep the per-row column\n%s", mixed)
	}
}
