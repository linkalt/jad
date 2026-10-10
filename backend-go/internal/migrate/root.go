package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// writeRootSetting writes <out>/setting.json: the server block and the libraries of the
// source, the session roots of the new tree, the catalog folders and the union of the
// tool definition paths.
func (r *runState) writeRootSetting() error {
	data, err := os.ReadFile(r.source.SettingPath)
	if err != nil {
		return err
	}
	source, err := rawObject(data)
	if err != nil {
		return err
	}
	base := filepath.Dir(r.source.SettingPath)
	setting := map[string]any{"schema_version": 1}
	if server, ok := source["server"]; ok {
		setting["server"] = r.serverBlock(server, base)
	}
	roots := make([]map[string]any, 0, len(r.source.Roots))
	for _, root := range r.source.Roots {
		roots = append(roots, map[string]any{"id": root.ID, "label": root.Label, "path": filepath.Base(r.sessionsDir(root))})
	}
	setting["session_roots"] = roots
	setting["catalogs"] = map[string]string{"profiles": "profiles", "models": "models", "tools": "tools", "mcp": "mcp", "state": "state"}
	libraries, _ := objectOf(source["libraries"])
	resolved := map[string]any{}
	for key, raw := range libraries {
		if key == "tool_definition_paths" {
			continue
		}
		var list []string
		if json.Unmarshal(raw, &list) == nil {
			for index := range list {
				list[index] = resolveFrom(base, list[index])
			}
			resolved[key] = list
			continue
		}
		resolved[key] = raw
	}
	if len(r.toolDef) > 0 {
		resolved["tool_definition_paths"] = r.toolDef
	}
	if len(resolved) > 0 {
		setting["libraries"] = resolved
	}
	for key := range source {
		switch key {
		case "schema_version", "server", "session_roots", "session_paths", "libraries":
		default:
			r.warn("root setting: %q is not carried over", key)
		}
	}
	return writeJSON(filepath.Join(r.out, "setting.json"), setting)
}

// serverBlock copies the server settings. A relative log file would resolve against the new
// setting.json, so it is made absolute against the old one.
func (r *runState) serverBlock(raw json.RawMessage, base string) any {
	fields, ok := objectOf(raw)
	if !ok {
		return raw
	}
	out := map[string]any{}
	for key, value := range fields {
		out[key] = value
	}
	var logFile string
	if json.Unmarshal(fields["log_file"], &logFile) == nil && logFile != "" {
		out["log_file"] = resolveFrom(base, logFile)
	}
	return out
}
