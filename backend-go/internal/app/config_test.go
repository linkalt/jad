package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

func TestLoadConfigAppliesRootSettingsAndEnvironmentOverrides(t *testing.T) {
	cwd := t.TempDir()
	setting := `{"session_roots":[{"id":"work","path":"sessions"}],"server":{"host":"127.0.0.1","port":8789,"max_body_bytes":8192,"run_state_retention_ms":0,"tool_stream_interval_ms":17,"sse_max_buffered_bytes":4096,"allowed_origins":["https://ui.example.test"]}}`
	if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir: cwd,
		Environment: map[string]string{
			"AGENT_API_TOKEN": integrationAPIToken,
			"HOST":            "localhost",
			"PORT":            "0",
		},
	})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.Host != "localhost" || config.Port != 0 {
		t.Errorf("listener settings = %q:%d", config.Host, config.Port)
	}
	if len(config.Roots) != 1 || config.Roots[0].ID != "work" {
		t.Errorf("roots = %+v", config.Roots)
	}
	if config.HTTP.APIToken != integrationAPIToken || config.HTTP.MaxBodyBytes != 8192 || config.HTTP.RunStateRetention != 0 || !config.HTTP.AllowZeroRunStateRetention {
		t.Errorf("HTTP settings = %+v", config.HTTP)
	}
	if config.HTTP.ToolStreamInterval != 17*time.Millisecond {
		t.Errorf("tool stream interval = %s, want 17ms", config.HTTP.ToolStreamInterval)
	}
	if _, hasToken := config.Environment[apiTokenEnvironmentName]; hasToken {
		t.Error("API authentication token was retained in the session environment")
	}
	if len(config.HTTP.AllowedOrigins) != 1 || config.HTTP.AllowedOrigins[0] != "https://ui.example.test" {
		t.Errorf("allowed origins = %#v", config.HTTP.AllowedOrigins)
	}
}

func TestLoadConfigRejectsInvalidPortAndServiceRequiresToken(t *testing.T) {
	cwd := t.TempDir()
	if _, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir:  cwd,
		Environment: map[string]string{"PORT": "invalid"},
	}); err == nil {
		t.Fatal("LoadConfig(PORT=invalid) succeeded")
	}
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: cwd, Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("LoadConfig(defaults) error = %v", err)
	}
	if _, err := NewService(config); err == nil {
		t.Fatal("NewService() without AGENT_API_TOKEN succeeded")
	}
}
