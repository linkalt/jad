package sessionfs

import (
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs/layouttest"
)

func TestLoadSessionConfigNormalizesNativeGrepAndDoesNotTakeReadPolicy(t *testing.T) {
	location, _ := layoutLocation(t, layouttest.Spec{Native: map[string]any{
		"read": map[string]any{"paths": map[string]any{"allow": []any{"allowed"}, "deny": []any{"private"}}},
		"grep": map[string]any{"max_results": 500},
	}})
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
	if len(config.NativeGrep.Policy.Paths) != 0 || config.NativeGrep.Policy.UnrestrictedPaths {
		t.Fatalf("grep took read's rules: %+v", config.NativeGrep.Policy)
	}
}
