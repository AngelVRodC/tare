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

// TestEnvelopeSchemaVersionOrderAndValue pins the schema_version contract:
// every `--json` envelope carries schema_version as its SECOND key — between
// tool and version, because struct order is serialisation order and position
// is consumer-visible — with value 1. Asserted on the wire, not the struct:
// that is what a consumer reads. The token-stream decode sees the keys in the
// order they were written, which json.Unmarshal into a map would discard.
func TestEnvelopeSchemaVersionOrderAndValue(t *testing.T) {
	// firstKeys decodes one envelope's JSON byte stream token by token and
	// returns the first three key names in wire order plus the schema_version
	// value the stream carried.
	firstKeys := func(t *testing.T, env Envelope) ([]string, float64) {
		t.Helper()
		var buf bytes.Buffer
		if err := WriteJSON(&buf, env); err != nil {
			t.Fatalf("WriteJSON: %v", err)
		}
		dec := json.NewDecoder(&buf)
		if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
			t.Fatalf("expected opening brace, got %v (%v)", tok, err)
		}
		var (
			keys  []string
			value = -1.0
		)
		for len(keys) < 3 {
			tok, err := dec.Token()
			if err != nil {
				t.Fatalf("reading key %d: %v", len(keys)+1, err)
			}
			name, ok := tok.(string)
			if !ok {
				t.Fatalf("expected a key, got %v", tok)
			}
			val, err := dec.Token()
			if err != nil {
				t.Fatalf("reading value of %s: %v", name, err)
			}
			keys = append(keys, name)
			if name == "schema_version" {
				num, ok := val.(float64)
				if !ok {
					t.Fatalf("schema_version = %v, want a number", val)
				}
				value = num
			}
		}
		return keys, value
	}

	// build points the test at every place an envelope is constructed: the
	// four command builders, the OpenCode rollup builder, and the report
	// merge. Each gets its own assertions so a fix at one site cannot mask a
	// miss at another.
	corpus := fixtureCorpus(t)
	envs := map[string]Envelope{}
	if env, err := ScanEnvelope(corpus, "test"); err != nil {
		t.Fatalf("ScanEnvelope: %v", err)
	} else {
		envs["ScanEnvelope"] = env
	}
	if env, err := ToolsEnvelope(corpus, "test"); err != nil {
		t.Fatalf("ToolsEnvelope: %v", err)
	} else {
		envs["ToolsEnvelope"] = env
	}
	if env, err := AttributeEnvelope(corpus, "test"); err != nil {
		t.Fatalf("AttributeEnvelope: %v", err)
	} else {
		envs["AttributeEnvelope"] = env
	}
	if env, err := CorruptionEnvelope(corpus, "test"); err != nil {
		t.Fatalf("CorruptionEnvelope: %v", err)
	} else {
		envs["CorruptionEnvelope"] = env
	}
	envs["openCodeEnvelope"] = openCodeEnvelope(corpus, "test", nil, openCodeSpan{}, nil, nil, 0)
	if r, err := BuildReport(corpus, "test", nil); err != nil {
		t.Fatalf("BuildReport: %v", err)
	} else {
		envs["merge (BuildReport)"] = r.Envelope
	}

	for name, env := range envs {
		t.Run(name, func(t *testing.T) {
			keys, value := firstKeys(t, env)
			want := []string{"tool", "schema_version", "version"}
			if len(keys) != len(want) {
				t.Fatalf("first keys = %v, want %v", keys, want)
			}
			for i := range want {
				if keys[i] != want[i] {
					t.Fatalf("key order = %v, want %v", keys, want)
				}
			}
			if value != 1 {
				t.Errorf("schema_version = %v, want 1", value)
			}
		})
	}
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
// TestNonTranscriptRowsRenderAndVanishWhenClean pins the reporting half of the
// journal exclusion: excluded records reach the envelope, the table and the
// warnings, and they leave no trace at all on a corpus without them.
//
// Both halves matter. Records dropped from the event count and absent from the
// output would be a silent loss; a row of zero on every clean corpus would be
// a column that says nothing.
func TestNonTranscriptRowsRenderAndVanishWhenClean(t *testing.T) {
	corpus := func(t *testing.T, files map[string]string) string {
		t.Helper()
		root := t.TempDir()
		for name, body := range files {
			path := filepath.Join(root, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	txLine := `{"type":"user","sessionId":"s1","timestamp":"2026-09-01T00:00:00.000Z"}` + "\n"

	t.Run("journal present", func(t *testing.T) {
		dir := corpus(t, map[string]string{
			"proj/sess.jsonl": txLine,
			"proj/sess/subagents/workflows/wf_x/journal.jsonl": `{"type":"started","key":"v2:a","agentId":"a1"}` + "\n" +
				`{"type":"result","key":"v2:a","agentId":"a1"}` + "\n",
		})
		env, err := ScanEnvelope(dir, "test")
		if err != nil {
			t.Fatalf("ScanEnvelope: %v", err)
		}
		if got := mustMetric(t, env, "files_non_transcript", "corpus", "").Value; got != 1 {
			t.Errorf("files_non_transcript = %v, want 1", got)
		}
		if got := mustMetric(t, env, "non_transcript_records", "corpus", "").Value; got != 2 {
			t.Errorf("non_transcript_records = %v, want 2", got)
		}
		var buf bytes.Buffer
		if err := RenderScan(&buf, env); err != nil {
			t.Fatalf("RenderScan: %v", err)
		}
		out := buf.String()
		for _, want := range []string{"Non-transcript files", "Non-transcript records", "Workflow-tool journal"} {
			if !strings.Contains(out, want) {
				t.Errorf("output is missing %q\n%s", want, out)
			}
		}
		// Those are journal record types, so the unknown-type warning about
		// unrecognised transcript events must not fire.
		if strings.Contains(out, "unknown event type") {
			t.Errorf("journal record types reported as unknown transcript events\n%s", out)
		}
	})

	t.Run("clean corpus omits the rows", func(t *testing.T) {
		env, err := ScanEnvelope(corpus(t, map[string]string{"proj/sess.jsonl": txLine}), "test")
		if err != nil {
			t.Fatalf("ScanEnvelope: %v", err)
		}
		for _, name := range []string{"files_non_transcript", "non_transcript_records"} {
			if m := findMetric(env, name, "corpus", ""); m != nil {
				t.Errorf("%s = %v, want the row omitted rather than a zero", name, m.Value)
			}
		}
		if strings.Contains(strings.Join(env.Warnings, "\n"), "Workflow-tool journal") {
			t.Errorf("clean corpus warned about journals: %v", env.Warnings)
		}
	})
}

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
