package sessionfs

import (
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestLoadSessionConfigNormalizesNativeGlobAndDoesNotTakeReadPolicy(t *testing.T) {
	location, _ := layoutLocation(t, layouttest.Spec{Native: map[string]any{
		"read": map[string]any{"paths": map[string]any{"allow": []any{"allowed"}, "deny": []any{"private"}}},
		"glob": map[string]any{"max_results": 900},
	}})
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
	if len(config.NativeGlob.Policy.Paths) != 0 || config.NativeGlob.Policy.UnrestrictedPaths {
		t.Fatalf("glob took read's rules: %+v", config.NativeGlob.Policy)
	}
}
