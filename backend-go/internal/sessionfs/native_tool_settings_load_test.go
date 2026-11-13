package sessionfs

import (
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestLoadSessionConfigNormalizesProfileToolStorageAndExecPaths(t *testing.T) {
	rootDir := t.TempDir()
	location, fixture := layoutLocation(t, layouttest.Spec{CWD: rootDir, Settings: map[string]any{"tool_storage_dir": "custom-tools"}, Native: map[string]any{
		"exec": map[string]any{"paths": map[string]any{"allow": []any{"allowed"}}},
	}})
	// relative paths.allow resolve against the global root, not the session cwd
	allowedRoot := fixture.GlobalDir // a relative path in a session or profile setting resolves from the global folder
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig() error = %v", err)
	}
	wantToolStorage := filepath.Join(rootDir, "custom-tools", "session-1", "dev", "coder", "tools")
	if config.StoragePaths.ToolStorage != wantToolStorage {
		t.Fatalf("tool storage = %q, want %q", config.StoragePaths.ToolStorage, wantToolStorage)
	}
	if config.NativeExec == nil {
		t.Fatal("native.exec is missing")
	}
	if want := filepath.Join(wantToolStorage, ".exec-output"); config.NativeExec.Child.Spill.StorageDir != want {
		t.Fatalf("exec spill = %q, want %q", config.NativeExec.Child.Spill.StorageDir, want)
	}
	if policy := config.NativeExec.Child.Policy; len(policy.Paths) != 1 || policy.Paths[0].Path != filepath.Join(allowedRoot, "allowed") {
		t.Fatalf("exec path policy = %+v", policy)
	}
}

func TestLoadSessionConfigRejectsTopLevelToolStorage(t *testing.T) {
	// "paths" is not a settings key of the layout; its tool_storage is refused with it.
	_, err := loadLayoutConfig(t, layouttest.Spec{Settings: map[string]any{"paths": map[string]any{"tool_storage": "tools"}}})
	if err == nil || !strings.Contains(err.Error(), "settings may not carry paths") {
		t.Fatalf("LoadSessionConfig() error = %v, want it to refuse paths in settings", err)
	}
}

func TestLoadSessionConfigSuggestsTheSettingKeyItMeant(t *testing.T) {
	// A misspelt key in settings is refused with the closest setting key, as tool keys are.
	_, err := loadLayoutConfig(t, layouttest.Spec{Settings: map[string]any{"max_tool_round": 3}})
	const want = "settings may not carry max_tool_round; it has its own file or field in the catalog layout; did you mean max_tool_rounds?"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("LoadSessionConfig() error = %v, want it to contain %q", err, want)
	}
}
