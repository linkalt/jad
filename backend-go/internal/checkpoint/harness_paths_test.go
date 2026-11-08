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

// The session file shares its folder with other sessions and its lock file, so the lock is
// excluded on its own; a restore that replaced it would break the stable-inode guarantee.
func TestHarnessPathsExcludeTheSessionLockFileAndStateDirectory(t *testing.T) {
	workspace := t.TempDir()
	sessionFile := filepath.Join(workspace, "sessions", "demo.json")
	state := filepath.Join(workspace, "state", "demo")
	config := sessionfs.SessionConfig{WorkingDir: workspace, SettingPath: sessionFile, StateDir: state}
	paths := HarnessPaths(config)
	if want := filepath.Join(workspace, "sessions", ".demo.json.jad-lock"); !containsPath(paths, want) {
		t.Fatalf("HarnessPaths = %v, want the lock file %s", paths, want)
	}
	if !containsPath(paths, state) {
		t.Fatalf("HarnessPaths = %v, want the state directory %s", paths, state)
	}
	if !containsPath(paths, sessionFile) {
		t.Fatalf("HarnessPaths = %v, want the session file %s", paths, sessionFile)
	}
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

// exec spills output under its own tools.native.exec.spill.storage_dir.
func TestHarnessPathsHoldTheExecSpillDirectory(t *testing.T) {
	workspace := t.TempDir()
	execSpill := filepath.Join(workspace, ".exec-out")
	config := sessionfs.SessionConfig{
		WorkingDir:  workspace,
		SettingPath: filepath.Join(workspace, "sessions", "demo", "setting.json"),
		NativeExec:  &sessionfs.NativeExecSettings{Child: sessionfs.NativeBashSettings{Spill: sessionfs.NativeBashSpillSettings{StorageDir: execSpill}}},
	}
	paths := HarnessPaths(config)
	if !containsPath(paths, execSpill) {
		t.Fatalf("HarnessPaths = %v, want %s", paths, execSpill)
	}

	config.NativeExec = nil
	if paths := HarnessPaths(config); containsPath(paths, execSpill) {
		t.Fatalf("HarnessPaths = %v, holds exec's directory although the tool is off", paths)
	}
}

// An explicit relative storage_dir resolves from the cwd, so every store can sit inside the
// workspace; each configured one is harness-owned and a tool that is off adds nothing.
func TestHarnessPathsHoldEveryConfiguredStoreDirectory(t *testing.T) {
	workspace := t.TempDir()
	stores := map[string]string{
		"task":         filepath.Join(workspace, "plans"),
		"records":      filepath.Join(workspace, "notes"),
		"code_context": filepath.Join(workspace, "ctx"),
		"tool_smith":   filepath.Join(workspace, "smith-out"),
	}
	config := sessionfs.SessionConfig{
		WorkingDir:        workspace,
		SettingPath:       filepath.Join(workspace, "sessions", "demo", "setting.json"),
		NativeTask:        &sessionfs.NativeTaskSettings{StorageDir: stores["task"]},
		NativeRecords:     &sessionfs.NativeRecordsSettings{StorageDir: stores["records"]},
		NativeCodeContext: &sessionfs.NativeCodeContextSettings{StorageDir: stores["code_context"]},
		NativeToolSmith:   &sessionfs.NativeToolSmithSettings{Child: sessionfs.NativeBashSettings{Spill: sessionfs.NativeBashSpillSettings{StorageDir: stores["tool_smith"]}}},
	}
	patterns := ExclusionPatterns(workspace, HarnessPaths(config))
	for name, dir := range stores {
		if paths := HarnessPaths(config); !containsPath(paths, dir) {
			t.Fatalf("HarnessPaths = %v, want the %s store %s", paths, name, dir)
		}
		rel, _ := filepath.Rel(workspace, dir)
		found := false
		for _, pattern := range patterns {
			found = found || pattern == "/"+rel
		}
		if !found {
			t.Fatalf("ExclusionPatterns = %v, want /%s for the %s store", patterns, rel, name)
		}
	}

	config.NativeTask, config.NativeRecords, config.NativeCodeContext, config.NativeToolSmith = nil, nil, nil, nil
	paths := HarnessPaths(config)
	for name, dir := range stores {
		if containsPath(paths, dir) {
			t.Fatalf("HarnessPaths = %v, holds the %s store although the tool is off", paths, name)
		}
	}
}

// A configured tool whose spill directory is empty must not turn into a pattern for the workspace itself.
func TestHarnessPathsSkipEmptyStoreDirectories(t *testing.T) {
	workspace := t.TempDir()
	config := sessionfs.SessionConfig{
		WorkingDir:        workspace,
		NativeCodeContext: &sessionfs.NativeCodeContextSettings{},
		NativeToolSmith:   &sessionfs.NativeToolSmithSettings{},
	}
	if paths := HarnessPaths(config); containsPath(paths, workspace) {
		t.Fatalf("HarnessPaths = %v, holds the workspace for an empty store directory", paths)
	}
}
