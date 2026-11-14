package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
)

// parseLibraryDefinitionPaths expands only root library entries, never the concrete
// paths composed into session settings. Expansion happens once when roots load.
func parseLibraryDefinitionPaths(raw json.RawMessage, where, base string) ([]string, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, configError("%s: libraries.tool_definition_paths must be an array of strings", where)
	}
	paths := make([]string, 0, len(values))
	for index, rawValue := range values {
		var value string
		if err := json.Unmarshal(rawValue, &value); err != nil || strings.TrimSpace(value) == "" {
			return nil, configError("%s: libraries.tool_definition_paths[%d] must be a non-empty string", where, index)
		}
		value = strings.TrimSpace(value)
		// Validate the whole input even if Glob would stop at a missing ancestor.
		if _, err := filepath.Match(value, ""); err != nil {
			return nil, configError("%s: libraries.tool_definition_paths[%d] invalid pattern %q: %v", where, index, value, err)
		}
		magic := "*?["
		if runtime.GOOS != "windows" {
			magic += `\`
		}
		if !strings.ContainsAny(value, magic) {
			paths = appendNewStrings(paths, resolveFrom(base, value))
			continue
		}
		pattern := value
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(escapeGlobBase(base), pattern)
		}
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, configError("%s: libraries.tool_definition_paths[%d] invalid pattern %q: %v", where, index, value, err)
		}
		if len(matches) == 0 {
			return nil, configError("%s: libraries.tool_definition_paths[%d] pattern %q matched no paths", where, index, value)
		}
		for _, match := range matches {
			paths = appendNewStrings(paths, filepath.Clean(match))
		}
	}
	return paths, nil
}

// The configured base is a literal directory, not part of the user's pattern.
func escapeGlobBase(base string) string {
	if runtime.GOOS == "windows" {
		return strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]").Replace(base)
	}
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`).Replace(base)
}
