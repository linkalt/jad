package app

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func TestCheckHookMatches(t *testing.T) {
	registered := []tools.Definition{
		{ID: "native.bash", Source: "native", Name: "bash"},
		{ID: "mcp:docs:lookup", Source: "mcp", Name: "lookup"},
	}
	config := func(match string) sessionfs.SessionConfig {
		return sessionfs.SessionConfig{
			SettingPath: "s.json",
			MCPServers: []sessionfs.MCPServerReference{
				{ID: "docs", Enabled: true},
				{ID: "off", Enabled: false},
			},
			Hooks: sessionfs.HooksSettings{PreTool: []sessionfs.ToolEventHook{{Name: "guard", Match: match, Blocking: true}}},
		}
	}
	for _, match := range []string{"*", "bash", "mcp:docs:lookup", "mcp:docs:*"} {
		if err := checkHookMatches(config(match), registered, nil); err != nil {
			t.Errorf("match %q: %v", match, err)
		}
	}
	err := checkHookMatches(config("bsh"), registered, nil)
	if err == nil || !strings.Contains(err.Error(), `hooks.pre_tool[0].match lists "bsh", which is not a registered tool (registered: bash, mcp:docs:lookup)`) {
		t.Fatalf("unregistered match: %v", err)
	}
	if err := checkHookMatches(config("mcp:ghost:*"), registered, nil); err == nil {
		t.Error("an unknown MCP server must be refused")
	}
	var warnings []string
	if err := checkHookMatches(config("mcp:off:lookup"), registered, func(m string) { warnings = append(warnings, m) }); err != nil {
		t.Fatalf("disabled server: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "disabled MCP server") {
		t.Errorf("warnings = %q", warnings)
	}
}
