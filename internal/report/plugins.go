package report

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// pluginSettingsPath is where Claude Code keeps user-scope plugin config.
// It is not under `--dir`: settings.json is user-scope **config**, not corpus
// data — the openCodeConfigPath precedent (config under home, not under the
// data dir) applies. A frozen corpus copy still resolves the measuring
// machine's installed plugins, which is correct: the authority describes what
// is installed here. A UserHomeDir failure is authority unavailable, the same
// path as a read failure.
func pluginSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// pluginSettings is the one field the authority needs. The values are decoded
// as raw JSON and ignored: enabledPlugins is a name authority, not a filter —
// a disabled plugin's tools still run and still cost bytes, so its name still
// resolves them.
type pluginSettings struct {
	EnabledPlugins map[string]json.RawMessage `json:"enabledPlugins"`
}

// pluginNames reads the authority and returns every plugin name, sorted
// longest-first with a strings.Compare tiebreak. Keys are
// `<plugin>@<marketplace>`: the part before the first `@` is the name. Both
// the read and the decode failure carry the settings path — a bare "cannot
// parse" would not tell the user which file to look at.
func pluginNames(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var cfg pluginSettings
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.EnabledPlugins == nil {
		return nil, fmt.Errorf("%s: no enabledPlugins key", path)
	}
	names := make([]string, 0, len(cfg.EnabledPlugins))
	for k := range cfg.EnabledPlugins {
		name, _, _ := strings.Cut(k, "@")
		names = append(names, name)
	}
	// openCodeServers' comparator: longest-first so a name that prefixes
	// another cannot claim its segments first, and a tiebreak so the slice is
	// byte-reproducible under Go's randomised map range.
	slices.SortFunc(names, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	return names, nil
}

// pluginServer names the plugin owning an MCP segment `plugin_<plugin>_<server>`,
// or "" when no configured name claims it. The remainder must be non-empty —
// `plugin_my` has no server part, so `my` does not claim it (openCodeServer's
// guard). Names arrive sorted longest-first from pluginNames, so the first hit
// is the split the comparator chose.
func pluginServer(segment string, names []string) string {
	for _, p := range names {
		if rest, ok := strings.CutPrefix(segment, "plugin_"+p+"_"); ok && rest != "" {
			return p
		}
	}
	return ""
}

// pluginRollup rewrites segment-keyed buckets (`plugin_<plugin>_<server>`,
// already bucketed in the scan loop) into plugin-keyed buckets. Segments
// without the `plugin_` prefix are plain MCP servers and are skipped — they
// already have their own dimension. Integer sums are order-independent, so
// ranging the map here cannot move a byte; the only map-derived text is the
// unresolved list, returned distinct and sorted for the warning.
// Non-matching segments land in one `plugin (unresolved)` bucket — the bytes
// are a subset of the tool rows either way, and losing them silently would
// make the plugin table lie about its own coverage.
func pluginRollup(segments map[string]*toolStat, names []string) (map[string]*toolStat, []string) {
	resolved := map[string]*toolStat{}
	var unresolvedKeys []string
	for seg, s := range segments {
		// Plain MCP server segments (no `plugin_` prefix) belong to the
		// mcp_server dimension and contribute nothing here.
		if !strings.HasPrefix(seg, "plugin_") {
			continue
		}
		plugin := pluginServer(seg, names)
		if plugin == "" {
			plugin = unresolvedRow
			unresolvedKeys = append(unresolvedKeys, seg)
		}
		d := bucket(resolved, plugin)
		d.Calls += s.Calls
		d.ContextBytes += s.ContextBytes
		d.ImageBytes += s.ImageBytes
		d.ProducedBytes += s.ProducedBytes
		d.Errors += s.Errors
	}
	slices.Sort(unresolvedKeys)
	return resolved, unresolvedKeys
}

// unresolvedRow is the key every unmatched segment aggregates into. It echoes
// attribute.go's `(unattributed)` style.
const unresolvedRow = "plugin (unresolved)"
