package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSettings writes a settings.json fixture into a temp home and returns
// the path pluginNames should be called with.
func writeSettings(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPluginNames pins the name authority: what it reads, what it keeps, and
// what counts as unavailable. The authority provides key mapping only — the
// values behind each entry (including a disabled plugin's false) never filter.
func TestPluginNames(t *testing.T) {
	t.Run("file absent is unavailable", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "settings.json")
		names, err := pluginNames(missing)
		if err == nil {
			t.Fatalf("pluginNames(%q) = %v, want an error", missing, names)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("error %q must carry the settings path", err)
		}
	})

	t.Run("malformed JSON carries path and cause", func(t *testing.T) {
		path := writeSettings(t, "{not json")
		names, err := pluginNames(path)
		if err == nil {
			t.Fatalf("pluginNames(%q) = %v, want an error", path, names)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error %q must carry the settings path", err)
		}
		if !strings.Contains(err.Error(), "invalid character") {
			t.Errorf("error %q must carry the parse cause", err)
		}
	})

	t.Run("key absent is unavailable", func(t *testing.T) {
		path := writeSettings(t, `{"model":"opus"}`)
		names, err := pluginNames(path)
		if err == nil {
			t.Fatalf("pluginNames(%q) = %v, want an error", path, names)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error %q must carry the settings path", err)
		}
	})

	t.Run("marketplace suffix stripped, disabled entry kept", func(t *testing.T) {
		path := writeSettings(t, `{"enabledPlugins":{"my_plugin@acme":false,"sre@internal":true}}`)
		names, err := pluginNames(path)
		if err != nil {
			t.Fatalf("pluginNames: %v", err)
		}
		if len(names) != 2 || names[0] != "my_plugin" || names[1] != "sre" {
			t.Errorf("names = %v, want [my_plugin sre] — @marketplace stripped, disabled kept, longest first", names)
		}
	})

	t.Run("empty map is available and empty", func(t *testing.T) {
		path := writeSettings(t, `{"enabledPlugins":{}}`)
		names, err := pluginNames(path)
		if err != nil {
			t.Fatalf("pluginNames: %v", err)
		}
		if len(names) != 0 {
			t.Errorf("names = %v, want none", names)
		}
	})
}

// TestPluginServer pins the split: a segment `plugin_<plugin>_<server>` is
// matched against the sorted names longest-first, the remainder must be
// non-empty, and no match yields "" — the unresolved bucket's job.
func TestPluginServer(t *testing.T) {
	names := []string{"my_plugin", "my", "sre"} // as pluginNames sorts them
	cases := []struct{ segment, plugin string }{
		{"plugin_my_plugin_srv", "my_plugin"}, // longest-first beats `my`
		{"plugin_sre_jaeger-qa", "sre"},       // names containing `_` work
		{"plugin_my", ""},                     // empty remainder does not match
		{"plugin_ghost_srv", ""},              // no matching name
		{"context7", ""},                      // a plain MCP server is not a plugin segment
	}
	for _, tc := range cases {
		if got := pluginServer(tc.segment, names); got != tc.plugin {
			t.Errorf("pluginServer(%q) = %q, want %q", tc.segment, got, tc.plugin)
		}
	}
}

// TestPluginRollup pins the post-scan resolution: segment buckets are summed
// into plugin buckets by integer addition (order-independent, so
// TestReportReproducible-safe), plain MCP segments are skipped, and whatever
// no name claims lands in one `plugin (unresolved)` bucket whose distinct
// segment names come back sorted for the warning.
func TestPluginRollup(t *testing.T) {
	segments := map[string]*toolStat{
		"plugin_sre_grafana-prod": {Calls: 1, ContextBytes: 100, ImageBytes: 2, ProducedBytes: 150, Errors: 1},
		"plugin_sre_jaeger-qa":    {Calls: 2, ContextBytes: 50, ImageBytes: 3, ProducedBytes: 25, Errors: 0},
		"plugin_ghost_srv":        {Calls: 1, ContextBytes: 7, ImageBytes: 0, ProducedBytes: 7, Errors: 0},
		"context7":                {Calls: 5, ContextBytes: 999, ImageBytes: 0, ProducedBytes: 0, Errors: 0},
	}
	resolved, unresolved := pluginRollup(segments, []string{"sre"})

	sre := resolved["sre"]
	if sre == nil {
		t.Fatal("no sre bucket — matched segments must resolve under their plugin")
	}
	if sre.Calls != 3 || sre.ContextBytes != 150 || sre.ImageBytes != 5 || sre.ProducedBytes != 175 || sre.Errors != 1 {
		t.Errorf("sre bucket = %+v, want both segments integer-summed", sre)
	}
	// Bytes are never lost or redistributed: the plain MCP segment contributes
	// nothing to the plugin dimension, the ghost lands whole in unresolved.
	if _, ok := resolved["context7"]; ok {
		t.Error("a plain MCP server segment must not enter the plugin dimension")
	}
	ghost := resolved["plugin (unresolved)"]
	if ghost == nil || ghost.ContextBytes != 7 || ghost.Calls != 1 {
		t.Errorf("unresolved bucket = %+v, want the ghost segment's 7 bytes, unsplit", ghost)
	}
	if len(unresolved) != 1 || unresolved[0] != "plugin_ghost_srv" {
		t.Errorf("unresolved = %v, want the distinct sorted ghost name", unresolved)
	}
}

// pluginHome builds a fake HOME containing a settings.json authority. The
// authority is read via os.UserHomeDir, which on Unix is $HOME, so t.Setenv
// points it at the fixture and never at the measuring machine's real config.
func pluginHome(t *testing.T, settings string) {
	t.Helper()
	home := t.TempDir()
	if settings != "" {
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(settings), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
}

// pluginMetrics collects the envelope's plugin-dimension rows.
func pluginMetrics(env Envelope) []Metric {
	var out []Metric
	for _, m := range env.Metrics {
		if m.Dimension == "plugin" {
			out = append(out, m)
		}
	}
	return out
}

// TestToolsEnvelopePluginDimension pins the wiring end to end: the scan
// buckets the segments, the authority resolves them after the scan, and an
// unavailable authority omits — never zeroes — the whole dimension.
func TestToolsEnvelopePluginDimension(t *testing.T) {
	t.Run("resolved rows are keyed by name and measured", func(t *testing.T) {
		pluginHome(t, `{"enabledPlugins":{"sre@acme":true}}`)
		dir := toolCorpus(t,
			use("t1", "mcp__plugin_sre_grafana-prod__query"), result("t1", `"aaaa"`, false),
			use("t2", "mcp__context7__docs"), result("t2", `"bb"`, false),
		)
		env, err := ToolsEnvelope(dir, "test", Window{})
		if err != nil {
			t.Fatalf("ToolsEnvelope: %v", err)
		}
		if got := metricValue(t, env, "context_bytes", "plugin", "sre"); got != int64(4) {
			t.Errorf("sre context_bytes = %v, want 4", got)
		}
		for _, m := range pluginMetrics(env) {
			if m.Derivation != "measured" {
				t.Errorf("plugin metric %s/%s derivation = %q, want measured", m.Name, m.Key, m.Derivation)
			}
		}
	})

	t.Run("unresolved segments aggregate into one row plus one warning", func(t *testing.T) {
		pluginHome(t, `{"enabledPlugins":{"sre@acme":true}}`)
		dir := toolCorpus(t,
			use("t1", "mcp__plugin_ghost_srv__call"), result("t1", `"x"`, false),
		)
		env, err := ToolsEnvelope(dir, "test", Window{})
		if err != nil {
			t.Fatalf("ToolsEnvelope: %v", err)
		}
		if got := metricValue(t, env, "context_bytes", "plugin", "plugin (unresolved)"); got != int64(1) {
			t.Errorf("unresolved context_bytes = %v, want 1", got)
		}
		var count int
		for _, w := range env.Warnings {
			if strings.Contains(w, "unresolved segments") {
				count++
			}
		}
		if count != 1 {
			t.Errorf("want exactly one unresolved warning, got %d in %v", count, env.Warnings)
		}
		for _, w := range env.Warnings {
			if strings.Contains(w, "unresolved segments") && !strings.Contains(w, "plugin_ghost_srv") {
				t.Errorf("unresolved warning must name the segment: %q", w)
			}
		}
	})

	t.Run("unavailable authority omits, never zeroes", func(t *testing.T) {
		pluginHome(t, "") // no settings.json at all
		dir := toolCorpus(t,
			use("t1", "mcp__plugin_sre_grafana-prod__query"), result("t1", `"aaaa"`, false),
		)
		env, err := ToolsEnvelope(dir, "test", Window{})
		if err != nil {
			t.Fatalf("ToolsEnvelope: %v", err)
		}
		if rows := pluginMetrics(env); len(rows) != 0 {
			t.Errorf("plugin rows = %d, want none — absent authority is not zero", len(rows))
		}
		var count int
		for _, w := range env.Warnings {
			if strings.Contains(w, "plugin rollup unavailable") {
				count++
			}
		}
		if count != 1 {
			t.Errorf("want exactly one unavailable warning, got %d in %v", count, env.Warnings)
		}
	})
}
