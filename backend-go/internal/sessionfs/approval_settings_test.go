package sessionfs

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// loadApprovalSetting loads a catalog-layout session built from the given object, so a
// test goes through the whole loader and its key allow-lists rather than one normalizer.
// "tools" becomes the toolset's native block, "mcp_servers" its mcp block (one server file
// per entry id), and every other key the session's settings.
func loadApprovalSetting(t *testing.T, extra map[string]any) (SessionConfig, error) {
	t.Helper()
	spec := layouttest.Spec{Settings: map[string]any{}}
	for key, value := range extra {
		switch key {
		case "tools":
			spec.Native = value.(map[string]any)["native"].(map[string]any)
		case "mcp_servers":
			spec.MCP = map[string]map[string]any{}
			for _, entry := range value.([]any) {
				server := entry.(map[string]any)
				use := map[string]any{}
				if approval, set := server["approval"]; set {
					use["approval"] = approval
				}
				spec.MCP["m/"+server["id"].(string)] = use
			}
		default:
			spec.Settings[key] = value
		}
	}
	return loadLayoutSpec(t, spec)
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
	want := ApprovalSettings{Default: ApprovalOff, TimeoutMS: 300_000, ExtendMS: 300_000, MaxTimeoutMS: 1_800_000, Headless: "deny", PreviewBytes: 4096}
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
		// A toolset's mcp use decodes approval as a string, so only a string reaches the
		// normalizer; null is read as absent and any other type fails the toolset decode.
		_, err := loadApprovalSetting(t, map[string]any{"mcp_servers": []any{map[string]any{"id": "fx", "config_path": "fx.json", "approval": value}}})
		want := `mcp_servers[0].approval must be "off" or "ask"`
		if _, isString := value.(string); !isString {
			want = "approval"
		}
		if value == nil {
			if err != nil {
				t.Errorf("mcp approval null: error = %v, want it read as absent", err)
			}
		} else if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("mcp approval %#v: error = %v, want %q", value, err, want)
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
	want := ApprovalSettings{Default: ApprovalOff, TimeoutMS: 1000, ExtendMS: 2000, MaxTimeoutMS: 5000, Headless: "allow", PreviewBytes: 128}
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
