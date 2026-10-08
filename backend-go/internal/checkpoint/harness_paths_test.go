package checkpoint

import (
	"path/filepath"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if filepath.Clean(path) == filepath.Clean(want) {
			return true
		}
	}
	return false
}

// A sub-agent writes its transcripts under tools.native.subagent.storage_dir. When that
// directory sits inside the workspace, a checkpoint must not record it as a change the
// run made, or restoring would delete the transcripts of children that finished.
func TestHarnessPathsExcludeTheSubagentStorageDirectory(t *testing.T) {
	workspace := t.TempDir()
	storage := filepath.Join(workspace, ".subagents")
	config := sessionfs.SessionConfig{
		WorkingDir:     workspace,
		SettingPath:    filepath.Join(workspace, "sessions", "demo", "setting.json"),
		NativeSubagent: &sessionfs.NativeSubagentSettings{StorageDir: storage},
	}
	if paths := HarnessPaths(config); !containsPath(paths, storage) {
		t.Fatalf("HarnessPaths = %v, want it to hold the sub-agent storage directory %s", paths, storage)
	}

	config.NativeSubagent = nil
	if paths := HarnessPaths(config); containsPath(paths, storage) {
		t.Fatalf("HarnessPaths = %v, holds a sub-agent directory although the tool is off", paths)
	}
}
