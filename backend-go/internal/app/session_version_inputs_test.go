package app

import (
	"os"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestSessionVersionChangesWithEachStampedFile(t *testing.T) {
	directory := t.TempDir()
	config := sessionfs.SessionConfig{
		Models:     []string{"one"},
		ModelPaths: map[string]string{"one": directory + "/model-one.json"},
	}
	config.MessagesPath = directory + "/messages.jsonl"
	config.MessagesBackupPath = directory + "/backup.jsonl"
	config.UsageCachePath = directory + "/usage.json"
	config.StoragePaths.RunErrorCache = directory + "/run-error.json"
	config.SettingPath = directory + "/setting.json"
	config.ModelPath = directory + "/model.json"
	paths := map[string]string{
		"transcript": config.MessagesPath, "backup": config.MessagesBackupPath, "usage": config.UsageCachePath,
		"run-error": config.StoragePaths.RunErrorCache, "setting": config.SettingPath, "model": config.ModelPath,
		"listed model": config.ModelPaths["one"],
	}
	for name, path := range paths {
		if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := sessionVersion(config)
		if sessionVersion(config) != before {
			t.Fatalf("version changes with no edit (%s)", name)
		}
		if err := os.WriteFile(path, []byte("longer content"), 0o600); err != nil {
			t.Fatal(err)
		}
		if sessionVersion(config) == before {
			t.Errorf("version did not change when the %s file was edited", name)
		}
		before = sessionVersion(config)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if sessionVersion(config) == before {
			t.Errorf("version did not change when the %s file was removed", name)
		}
	}
}
