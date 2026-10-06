package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadApprovalSetting loads a session whose setting.json is the given object plus
// the fields every session needs, so a test goes through the whole loader and its
// key allow-lists rather than one normalizer.
func loadApprovalSetting(t *testing.T, extra map[string]any) (SessionConfig, error) {
	t.Helper()
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setting := map[string]any{"schema_version": 1, "cwd": root}
	for key, value := range extra {
		setting[key] = value
	}
	encoded, err := json.Marshal(setting)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadSessionSettings(Location{SessionID: "session-1", Directory: sessionDir, Root: Root{GlobalJadRoot: root}}, "", "")
}

func nativeTools(tools map[string]any) map[string]any {
	return map[string]any{"tools": map[string]any{"native": tools}}
}

func TestApprovalDefaultsAreOffAndTheSharedBlockHasTypeScriptDefaults(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{"tools": map[string]any{"native": map[string]any{
		"write": map[string]any{}, "edit": map[string]any{}, "bash": map[string]any{}, "process": map[string]any{},
	}}, "mcp_servers": []any{map[string]any{"id": "fx", "config_path": "fx.json"}}})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	for name, got := range map[string]string{
		"write": config.NativeWrite.Approval, "edit": config.NativeEdit.Approval,
		"bash": config.NativeBash.Approval, "process": config.NativeProcess.Approval, "mcp": config.MCPServers[0].Approval,
	} {
		if got != ApprovalOff {
			t.Errorf("%s approval = %q, want %q", name, got, ApprovalOff)
		}
	}
	want := ApprovalSettings{TimeoutMS: 300_000, ExtendMS: 300_000, MaxTimeoutMS: 1_800_000, Headless: "deny", PreviewBytes: 4096}
	if config.Approval != want {
		t.Fatalf("default approval settings = %+v, want %+v", config.Approval, want)
	}
}

func TestApprovalAskIsReadForEveryGatedToolAndNeverInherited(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{
		"tools": map[string]any{"native": map[string]any{
			"write":   map[string]any{"approval": "ask"},
			"edit":    map[string]any{},
			"bash":    map[string]any{"approval": "ask"},
			"process": map[string]any{"approval": "ask"},
		}},
		"mcp_servers": []any{map[string]any{"id": "fx", "config_path": "fx.json", "approval": "ask"}, map[string]any{"id": "gx", "config_path": "gx.json"}},
	})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	if config.NativeWrite.Approval != ApprovalAsk || config.NativeBash.Approval != ApprovalAsk || config.NativeProcess.Approval != ApprovalAsk || config.MCPServers[0].Approval != ApprovalAsk {
		t.Fatalf("ask was not read: write=%q bash=%q process=%q mcp=%q", config.NativeWrite.Approval, config.NativeBash.Approval, config.NativeProcess.Approval, config.MCPServers[0].Approval)
	}
	// edit inherits write's backup and path settings but must not inherit its gate.
	if config.NativeEdit.Approval != ApprovalOff {
		t.Fatalf("edit inherited write's approval: %q", config.NativeEdit.Approval)
	}
	if config.MCPServers[1].Approval != ApprovalOff {
		t.Fatalf("a server without the key asks: %q", config.MCPServers[1].Approval)
	}
}

func TestApprovalRejectsAnythingButOffOrAsk(t *testing.T) {
	for _, value := range []any{nil, "on", "Ask", "", true, 1.0, []any{"ask"}} {
		for _, tool := range []string{"write", "edit", "bash", "process"} {
			_, err := loadApprovalSetting(t, nativeTools(map[string]any{tool: map[string]any{"approval": value}}))
			want := "tools.native." + tool + `.approval must be "off" or "ask"`
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s approval %#v: error = %v, want %q", tool, value, err, want)
			}
		}
		_, err := loadApprovalSetting(t, map[string]any{"mcp_servers": []any{map[string]any{"id": "fx", "config_path": "fx.json", "approval": value}}})
		if err == nil || !strings.Contains(err.Error(), `mcp_servers[0].approval must be "off" or "ask"`) {
			t.Errorf("mcp approval %#v: error = %v", value, err)
		}
	}
}

func TestApprovalKeyIsRefusedOnToolsThatCannotAsk(t *testing.T) {
	for _, tool := range []string{"read", "grep", "glob", "task", "memory", "fetch", "request_draft", "skill"} {
		_, err := loadApprovalSetting(t, nativeTools(map[string]any{tool: map[string]any{"approval": "ask"}}))
		if err == nil || !strings.Contains(err.Error(), "tools.native."+tool+".approval is not supported") {
			t.Errorf("%s accepted an approval key: %v", tool, err)
		}
	}
}

func TestApprovalSharedBlockValuesAndBounds(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{"approval": map[string]any{
		"timeout_ms": 1000, "extend_ms": 2000, "max_timeout_ms": 5000, "headless": "allow", "preview_bytes": 128,
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := ApprovalSettings{TimeoutMS: 1000, ExtendMS: 2000, MaxTimeoutMS: 5000, Headless: "allow", PreviewBytes: 128}
	if config.Approval != want {
		t.Fatalf("approval = %+v, want %+v", config.Approval, want)
	}
	for _, tc := range []struct {
		field string
		value any
	}{
		{"timeout_ms", 0}, {"timeout_ms", 1_800_001}, {"extend_ms", 999}, {"max_timeout_ms", 1.5},
		{"preview_bytes", 63}, {"preview_bytes", 65_537}, {"preview_bytes", "4096"},
	} {
		_, err := loadApprovalSetting(t, map[string]any{"approval": map[string]any{tc.field: tc.value}})
		if err == nil || !strings.Contains(err.Error(), "approval."+tc.field+" must be a safe integer") {
			t.Errorf("%s=%v: error = %v", tc.field, tc.value, err)
		}
	}
	if _, err := loadApprovalSetting(t, map[string]any{"approval": map[string]any{"timeout_ms": 9000, "max_timeout_ms": 8000}}); err == nil || !strings.Contains(err.Error(), "approval.timeout_ms must not exceed max_timeout_ms") {
		t.Errorf("timeout above max: %v", err)
	}
	if _, err := loadApprovalSetting(t, map[string]any{"approval": map[string]any{"headless": "maybe"}}); err == nil || !strings.Contains(err.Error(), `approval.headless must be "deny" or "allow"`) {
		t.Errorf("bad headless: %v", err)
	}
	if _, err := loadApprovalSetting(t, map[string]any{"approval": map[string]any{"timeout": 1000}}); err == nil || !strings.Contains(err.Error(), "approval.timeout is not supported") {
		t.Errorf("misspelt key: %v", err)
	}
	if _, err := loadApprovalSetting(t, map[string]any{"approval": "ask"}); err == nil || !strings.Contains(err.Error(), "approval must be an object") {
		t.Errorf("approval not an object: %v", err)
	}
}
