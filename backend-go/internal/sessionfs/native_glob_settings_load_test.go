package sessionfs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSessionConfigNormalizesNativeGlobAndInheritsReadPolicy(t *testing.T) {
	rootDir := t.TempDir()
	sessionDir := filepath.Join(rootDir, "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setting := fmt.Sprintf(`{"schema_version":1,"cwd":%q,"tools":{"native":{"read":{"paths":{"allow":["allowed"],"deny":["private"]}},"glob":{"max_results":900}}}}`, rootDir)
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), []byte(`{"model":"gpt-test","api_key_env":"TEST_KEY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	location := Location{SessionID: "session-1", Directory: sessionDir, Root: Root{GlobalJadRoot: rootDir}}
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
