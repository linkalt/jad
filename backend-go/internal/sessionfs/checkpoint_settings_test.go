package sessionfs

import (
	"strings"
	"testing"
)

func TestCheckpointDefaultsAreOffAndMatchTypeScript(t *testing.T) {
	config, err := loadSetting(t, map[string]any{})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	want := CheckpointSettings{
		Enabled: false, GitBinary: "git", MaxCheckpoints: 20, MaxFiles: 20_000, MaxTotalBytes: 512 * 1024 * 1024,
		TimeoutMS: 120_000, IncludeMCPTools: true, SnapshotOn: "first_mutation", SafeRestore: true,
	}
	if config.Checkpoint != want {
		t.Fatalf("default checkpoint settings = %+v, want %+v", config.Checkpoint, want)
	}
}

func TestCheckpointAcceptsEveryKey(t *testing.T) {
	config, err := loadSetting(t, map[string]any{"checkpoint": map[string]any{
		"enabled": true, "git_binary": " /usr/bin/git ", "max_checkpoints": 3, "max_files": 50, "max_total_bytes": 1024,
		"timeout_ms": 2000, "include_mcp_tools": false, "snapshot_on": "run_start", "safe_restore": false,
	}})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	want := CheckpointSettings{
		Enabled: true, GitBinary: "/usr/bin/git", MaxCheckpoints: 3, MaxFiles: 50, MaxTotalBytes: 1024,
		TimeoutMS: 2000, IncludeMCPTools: false, SnapshotOn: "run_start", SafeRestore: false,
	}
	if config.Checkpoint != want {
		t.Fatalf("checkpoint settings = %+v, want %+v", config.Checkpoint, want)
	}
}

// Every case here is also a TypeScript test: both backends must refuse the same
// values with the same message.
func TestCheckpointRejectsBadValuesAndUnknownKeys(t *testing.T) {
	cases := []struct {
		name       string
		checkpoint any
		message    string
	}{
		{"enabled string", map[string]any{"enabled": "yes"}, "checkpoint.enabled must be a boolean"},
		{"enabled null", map[string]any{"enabled": nil}, "checkpoint.enabled must be a boolean"},
		{"empty binary", map[string]any{"git_binary": ""}, "checkpoint.git_binary must be a non-empty string"},
		{"binary number", map[string]any{"git_binary": 7}, "checkpoint.git_binary must be a non-empty string"},
		{"checkpoints zero", map[string]any{"max_checkpoints": 0}, "checkpoint.max_checkpoints must be a safe integer between 1 and 1000"},
		{"files fraction", map[string]any{"max_files": 1.5}, "checkpoint.max_files must be a safe integer"},
		{"files null", map[string]any{"max_files": nil}, "checkpoint.max_files must be a safe integer"},
		{"bytes zero", map[string]any{"max_total_bytes": 0}, "checkpoint.max_total_bytes must be a safe integer"},
		{"timeout low", map[string]any{"timeout_ms": 999}, "checkpoint.timeout_ms must be a safe integer between 1000"},
		{"mcp number", map[string]any{"include_mcp_tools": 1}, "checkpoint.include_mcp_tools must be a boolean"},
		{"snapshot_on word", map[string]any{"snapshot_on": "always"}, `checkpoint.snapshot_on must be "first_mutation" or "run_start"`},
		{"safe string", map[string]any{"safe_restore": "true"}, "checkpoint.safe_restore must be a boolean"},
		{"misspelt key", map[string]any{"enable": true}, "checkpoint.enable is not supported; did you mean enabled?"},
		{"not an object", "on", "checkpoint must be an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadSetting(t, map[string]any{"checkpoint": tc.checkpoint})
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v, want one containing %q", err, tc.message)
			}
		})
	}
}
