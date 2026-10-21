package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkLoadSessionConfig(b *testing.B) {
	root, cwd := b.TempDir(), b.TempDir()
	write := func(rel string, value any) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			b.Fatal(err)
		}
		data, err := json.Marshal(value)
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			b.Fatal(err)
		}
	}
	write("setting.json", map[string]any{
		"schema_version": 1,
		"session_roots":  []any{map[string]any{"id": "local", "path": "sessions"}},
		"catalogs":       map[string]any{"profiles": "profiles", "models": "models", "tools": "tools", "mcp": "mcp", "state": filepath.Join(cwd, "state")},
	})
	write("models/openai/small.json", map[string]any{"provider": "openai", "model": "m", "api_key_env": "FIXTURE_KEY", "base_url": "http://127.0.0.1:1/v1"})
	write("tools/coding/none.json", map[string]any{"native": map[string]any{}, "mcp": map[string]any{}})
	write("profiles/dev/main.json", map[string]any{"models": []string{"openai/small"}, "model_default": "openai/small", "toolset": "coding/none"})
	write("sessions/s1.json", map[string]any{"schema_version": 1, "cwd": cwd, "scope": map[string]any{"profiles": []string{"dev"}}})
	config, err := Load(LoadOptions{WorkingDir: root, RootSettingPath: filepath.Join(root, "setting.json"), Environment: map[string]string{"FIXTURE_KEY": "k"}})
	if err != nil {
		b.Fatal(err)
	}
	location, err := Resolve(config.Roots, "s1")
	if err != nil {
		b.Fatal(err)
	}
	b.Run("settings", func(b *testing.B) {
		b.ReportAllocs()
		for iteration := 0; iteration < b.N; iteration++ {
			if _, err := LoadSessionSettings(location, "", ""); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("settings-and-model", func(b *testing.B) {
		b.ReportAllocs()
		for iteration := 0; iteration < b.N; iteration++ {
			if _, err := LoadSessionConfig(location, "", ""); err != nil {
				b.Fatal(err)
			}
		}
	})
}
