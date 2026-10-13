package checkpoint

import (
	"path/filepath"
	"testing"

	"jad/backend-go/internal/sessionfs"
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

// exec spills output under its own tools.native.exec.spill.storage_dir, which is not bash's.
func TestHarnessPathsHoldTheExecSpillDirectoryApartFromBashs(t *testing.T) {
	workspace := t.TempDir()
	bashSpill := filepath.Join(workspace, ".bash-out")
	execSpill := filepath.Join(workspace, ".exec-out")
	config := sessionfs.SessionConfig{
		WorkingDir:  workspace,
		SettingPath: filepath.Join(workspace, "sessions", "demo", "setting.json"),
		NativeBash:  &sessionfs.NativeBashSettings{Spill: sessionfs.NativeBashSpillSettings{StorageDir: bashSpill}},
		NativeExec:  &sessionfs.NativeExecSettings{Child: sessionfs.NativeBashSettings{Spill: sessionfs.NativeBashSpillSettings{StorageDir: execSpill}}},
	}
	paths := HarnessPaths(config)
	if !containsPath(paths, bashSpill) || !containsPath(paths, execSpill) {
		t.Fatalf("HarnessPaths = %v, want both %s and %s", paths, bashSpill, execSpill)
	}

	config.NativeExec = nil
	if paths := HarnessPaths(config); containsPath(paths, execSpill) {
		t.Fatalf("HarnessPaths = %v, holds exec's directory although the tool is off", paths)
	}
}
