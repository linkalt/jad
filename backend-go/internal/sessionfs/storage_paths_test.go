package sessionfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSessionConfigRejectsDuplicateStorageFilesWithinProfile(t *testing.T) {
	rootDir := t.TempDir()
	sessionDir := filepath.Join(rootDir, "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setting := `{"cwd":"` + rootDir + `","paths":{"messages":"shared.jsonl","messages_backup":"shared.jsonl"}}`
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSessionConfig(Location{Directory: sessionDir, Root: Root{GlobalJadRoot: rootDir}}, "", "")
	var configErr *ConfigError
	if !errors.As(err, &configErr) || !strings.Contains(configErr.Message, "must be different files") {
		t.Fatalf("duplicate storage path error = %T %v", err, err)
	}
}

func TestLoadSessionConfigRejectsStoragePathsSharedAcrossProfiles(t *testing.T) {
	rootDir := t.TempDir()
	sessionDir := filepath.Join(rootDir, "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setting := `{"cwd":"` + rootDir + `","paths":{"active_profile":"green","profiles":{"green":{"models":["model.json"]},"blue":{"models":["model.json"],"messages":"messages.jsonl"}}}}`
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSessionConfig(Location{Directory: sessionDir, Root: Root{GlobalJadRoot: rootDir}}, "green", "")
	var configErr *ConfigError
	if !errors.As(err, &configErr) || !strings.Contains(configErr.Message, "path profiles 'green' and 'blue' resolve to the same storage path") {
		t.Fatalf("shared profile storage path error = %T %v", err, err)
	}
}

func TestLoadSessionConfigRejectsInvalidPathSettingsLikeTypeScript(t *testing.T) {
	cases := []struct {
		name  string
		paths string
		want  string
	}{
		{"removed base model", `{"model":"m.json"}`, "paths.model was removed"},
		{"base models", `{"models":["m.json"]}`, "model_default and models must be configured within paths.profiles"},
		{"removed processes", `{"processes":"p"}`, "paths.processes and paths.tasks were removed"},
		{"profile removed model", `{"active_profile":"a","profiles":{"a":{"model":"m.json"}}}`, "paths.profiles.a.model was removed"},
		{"profile unknown field", `{"active_profile":"a","profiles":{"a":{"bogus":"x"}}}`, "paths.profiles.a.bogus is not a supported session path"},
		{"profile duplicate models", `{"active_profile":"a","profiles":{"a":{"models":["m.json","m.json"]}}}`, "models must not contain duplicate paths"},
		{"profile bad path type", `{"active_profile":"a","profiles":{"a":{"messages":3}}}`, "messages must be a non-empty string"},
		{"profile default not in models", `{"active_profile":"a","profiles":{"a":{"model_default":"x.json","models":["m.json"]}}}`, "model_default must be included in models"},
		{"profile prompt documents", `{"active_profile":"a","profiles":{"a":{"prompt_documents":[""]}}}`, "prompt_documents[0] must be a non-empty string"},
		{"profile removed tasks", `{"active_profile":"a","profiles":{"a":{"tasks":"t"}}}`, "paths.processes and paths.tasks were removed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rootDir := t.TempDir()
			sessionDir := filepath.Join(rootDir, "session-1")
			if err := os.MkdirAll(sessionDir, 0o755); err != nil {
				t.Fatal(err)
			}
			setting := `{"cwd":"` + rootDir + `","paths":` + tc.paths + `}`
			if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(setting), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadSessionConfig(Location{Directory: sessionDir, Root: Root{GlobalJadRoot: rootDir}}, "", "")
			var configErr *ConfigError
			if !errors.As(err, &configErr) || !strings.Contains(configErr.Message, tc.want) {
				t.Fatalf("error = %T %v, want %q", err, err, tc.want)
			}
		})
	}
}
