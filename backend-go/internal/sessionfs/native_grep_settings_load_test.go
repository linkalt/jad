package sessionfs

import (
	"path/filepath"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs/layouttest"
)

func TestLoadSessionConfigNormalizesNativeGrepAndInheritsReadPolicy(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{Native: map[string]any{
		"read": map[string]any{"paths": map[string]any{"allow": []any{"allowed"}, "deny": []any{"private"}}},
		"grep": map[string]any{"max_results": 500},
	}})
	// relative read.paths resolve against the global root, which is not the session cwd
	rootDir := fixture.Root
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig() error = %v", err)
	}
	if config.NativeGrep == nil {
		t.Fatal("LoadSessionConfig() returned no native.grep settings")
	}
	if config.NativeGrep.MaxResults != 500 || config.NativeGrep.MaxMatchesPerFile != 20 || config.NativeGrep.Binary != "rg" {
		t.Fatalf("normalized grep settings = %+v", config.NativeGrep)
	}
	if len(config.NativeGrep.Policy.Paths) != 2 || config.NativeGrep.Policy.Paths[0].Path != filepath.Join(rootDir, "allowed") || config.NativeGrep.Policy.Paths[1].Path != filepath.Join(rootDir, "private") {
		t.Fatalf("inherited grep path policy = %+v", config.NativeGrep.Policy)
	}
}
