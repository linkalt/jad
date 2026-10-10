package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// stateDir is where the layout keeps a profile's state: <state>/<session>/<category>/<name>/.
func (r *runState) stateDir(session, profileID string) string {
	return filepath.Join(r.out, "state", session, filepath.FromSlash(profileID))
}

// copyState copies the transcript, the caches, the staging folder, the HTTP debug folder
// and the tool storage of one profile to its folder in the new tree. It returns the number
// of files copied and the map from each legacy path to the new one.
func (r *runState) copyState(p *plan, profile *legacyProfile) (int, map[string]string, error) {
	storage := profile.config.StoragePaths
	target := r.stateDir(p.name, profile.id)
	staging := filepath.Join(target, "staging")
	moved := map[string]string{}
	total := 0
	copyOne := func(source, destination string) error {
		if source == "" {
			return nil
		}
		who := p.root.ID + ":" + p.name + " " + profile.name
		if owner, shared := r.stateOwner[source]; shared && owner != who {
			if _, err := os.Stat(source); err == nil {
				r.warn("%s: %s was also used by %s; each profile now has its own copy, so they no longer share it", who, source, owner)
			}
		} else {
			r.stateOwner[source] = who
		}
		count, err := copyPath(source, destination)
		if err != nil {
			return fmt.Errorf("%s: %w", source, err)
		}
		total += count
		moved[filepath.Clean(source)] = destination
		return nil
	}
	pairs := []struct{ source, destination string }{
		{storage.Messages, filepath.Join(target, "messages.jsonl")},
		{storage.MessagesBackup, filepath.Join(target, "messages.backup.jsonl")},
		{storage.Staging, staging},
		{storage.HTTPDebug, filepath.Join(target, "debug", "http")},
		{storage.ToolStorage, filepath.Join(target, "tools")},
	}
	for _, item := range pairs {
		if err := copyOne(item.source, item.destination); err != nil {
			return total, moved, err
		}
	}
	if storage.ToolStorage != "" {
		if _, err := rewriteSubagentRuns(filepath.Join(target, "tools"), storage.ToolStorage, profile.id); err != nil {
			return total, moved, fmt.Errorf("fix the sub-agent run manifests: %w", err)
		}
	}
	// A cache the legacy setting put outside the staging folder goes into the new one under
	// its default name, which is where the layout looks for it.
	caches := []struct{ source, name string }{
		{storage.CompactionCache, "compaction-cache.json"},
		{storage.HistoryCache, "history-cache.json"},
		{storage.UsageCache, "usage-cache.json"},
		{storage.CursorCache, "cursor-cache.json"},
		{storage.RunErrorCache, "run-error-cache.json"},
		{storage.StateContext, "statecontext.json"},
	}
	for _, item := range caches {
		if item.source == "" || insideDirectory(item.source, storage.Staging) {
			if relative, err := filepath.Rel(storage.Staging, item.source); err == nil && item.source != "" {
				moved[filepath.Clean(item.source)] = filepath.Join(staging, relative)
			}
			continue
		}
		if _, err := os.Stat(item.source); err != nil {
			continue
		}
		if err := copyOne(item.source, filepath.Join(staging, item.name)); err != nil {
			return total, moved, err
		}
	}
	return total, moved, nil
}

func insideDirectory(path, directory string) bool {
	if directory == "" {
		return false
	}
	relative, err := filepath.Rel(directory, path)
	return err == nil && relative != "." && !strings.HasPrefix(relative, "..")
}
