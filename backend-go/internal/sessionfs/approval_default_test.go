package sessionfs

import (
	"strings"
	"testing"
)

func TestApprovalDefaultAskFillsEveryToolWithoutItsOwnKeyAndAnExplicitValueWins(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{
		"approval": map[string]any{"default": "ask"},
		"tools": map[string]any{"native": map[string]any{
			"bash":    map[string]any{},
			"write":   map[string]any{"approval": "off"},
			"edit":    map[string]any{"approval": "ask"},
			"process": map[string]any{},
		}},
		"mcp_servers": []any{
			map[string]any{"id": "plain", "config_path": "a.json"},
			map[string]any{"id": "quiet", "config_path": "b.json", "approval": "off"},
		},
	})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	if config.Approval.Default != ApprovalAsk {
		t.Fatalf("approval.default = %q, want ask", config.Approval.Default)
	}
	for name, want := range map[string]string{
		"bash":    ApprovalAsk,
		"process": ApprovalAsk,
		"edit":    ApprovalAsk,
		"write":   ApprovalOff,
	} {
		var got string
		switch name {
		case "bash":
			got = config.NativeBash.Approval
		case "process":
			got = config.NativeProcess.Approval
		case "edit":
			got = config.NativeEdit.Approval
		case "write":
			got = config.NativeWrite.Approval
		}
		if got != want {
			t.Errorf("%s approval = %q, want %q", name, got, want)
		}
	}
	if config.MCPServers[0].Approval != ApprovalAsk {
		t.Errorf("a server without the key = %q, want ask", config.MCPServers[0].Approval)
	}
	if config.MCPServers[1].Approval != ApprovalOff {
		t.Errorf("a server with an explicit off = %q, want off", config.MCPServers[1].Approval)
	}
}

func TestApprovalDefaultOffOrAbsentLeavesAbsentKeysOff(t *testing.T) {
	for _, approval := range []any{map[string]any{"default": "off"}, map[string]any{}, nil} {
		extra := map[string]any{
			"tools":       map[string]any{"native": map[string]any{"bash": map[string]any{}}},
			"mcp_servers": []any{map[string]any{"id": "fx", "config_path": "fx.json"}},
		}
		if approval != nil {
			extra["approval"] = approval
		}
		config, err := loadApprovalSetting(t, extra)
		if err != nil {
			t.Fatalf("approval %#v: %v", approval, err)
		}
		if config.Approval.Default != ApprovalOff || config.NativeBash.Approval != ApprovalOff || config.MCPServers[0].Approval != ApprovalOff {
			t.Errorf("approval %#v: default=%q bash=%q mcp=%q, want all off", approval, config.Approval.Default, config.NativeBash.Approval, config.MCPServers[0].Approval)
		}
	}
}

func TestApprovalDefaultDoesNotCreateAToolTheSessionDoesNotConfigure(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{
		"approval": map[string]any{"default": "ask"},
		"tools":    map[string]any{"native": map[string]any{"bash": map[string]any{}}},
	})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	if config.NativeWrite != nil || config.NativeEdit != nil || config.NativeProcess != nil {
		t.Fatalf("a tool appeared: write=%v edit=%v process=%v", config.NativeWrite, config.NativeEdit, config.NativeProcess)
	}
}

func TestApprovalDefaultRefusesAnythingButOffAndAskNullIncluded(t *testing.T) {
	for _, value := range []any{nil, "on", "always", true, 1.0, "", []any{"ask"}} {
		_, err := loadApprovalSetting(t, map[string]any{"approval": map[string]any{"default": value}})
		if err == nil || !strings.Contains(err.Error(), `approval.default must be "off" or "ask"`) {
			t.Errorf("default %#v: error = %v", value, err)
		}
	}
}
