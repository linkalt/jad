package sessionfs

import (
	"path/filepath"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs/layouttest"
)

func TestLoadSessionConfigNormalizesNativeGlobAndInheritsReadPolicy(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{Native: map[string]any{
		"read": map[string]any{"paths": map[string]any{"allow": []any{"allowed"}, "deny": []any{"private"}}},
		"glob": map[string]any{"max_results": 900},
	}})
	// relative read.paths resolve against the global root, which is not the session cwd
	rootDir := fixture.Root
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig() error = %v", err)
	}
	if config.NativeGlob == nil {
		t.Fatal("LoadSessionConfig() returned no native.glob settings")
	}
	if config.NativeGlob.MaxResults != 900 || config.NativeGlob.MaxBytes != 64*1024 || config.NativeGlob.Binary != "rg" || !config.NativeGlob.IncludeHidden {
		t.Fatalf("normalized glob settings = %+v", config.NativeGlob)
	}
	if len(config.NativeGlob.Policy.Paths) != 2 || config.NativeGlob.Policy.Paths[0].Path != filepath.Join(rootDir, "allowed") || config.NativeGlob.Policy.Paths[1].Path != filepath.Join(rootDir, "private") {
		t.Fatalf("inherited glob path policy = %+v", config.NativeGlob.Policy)
	}
}
