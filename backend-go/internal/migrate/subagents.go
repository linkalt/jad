package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// rewriteSubagentRuns fixes the run manifests copied under <tools>/.subagents/.runs. A
// manifest names its transcript folder by absolute path and its owner as
// "<session reference>\x00<profile>"; both change when the profile moves, and a manifest
// that still named the old place would have its transcript dropped and its run hidden from
// the profile it belongs to.
func rewriteSubagentRuns(newTools, oldTools, profileID string) (int, error) {
	runs := filepath.Join(newTools, ".subagents", ".runs")
	entries, err := os.ReadDir(runs)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(runs, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return changed, err
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields == nil {
			continue
		}
		touched := false
		var dir string
		if json.Unmarshal(fields["dir"], &dir) == nil && dir != "" {
			if relative, err := filepath.Rel(oldTools, dir); err == nil && !strings.HasPrefix(relative, "..") {
				fields["dir"], _ = json.Marshal(filepath.Join(newTools, relative))
				touched = true
			}
		}
		var owner string
		if json.Unmarshal(fields["owner"], &owner) == nil {
			if reference, _, found := strings.Cut(owner, "\x00"); found {
				fields["owner"], _ = json.Marshal(reference + "\x00" + profileID)
				touched = true
			}
		}
		if !touched {
			continue
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			return changed, err
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}
