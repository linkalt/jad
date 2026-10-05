package sessionfs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSessionConfigNormalizesNativeGrepAndInheritsReadPolicy(t *testing.T) {
	rootDir := t.TempDir()
	sessionDir := filepath.Join(rootDir, "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setting := fmt.Sprintf(`{"schema_version":1,"cwd":%q,"tools":{"native":{"read":{"paths":{"allow":["allowed"],"deny":["private"]}},"grep":{"max_results":500}}}}`, rootDir)
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
