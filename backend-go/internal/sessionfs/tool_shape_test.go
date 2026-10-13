package sessionfs

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// The catalog layout has no "tools" node: the toolset file holds `native` and `mcp`
// directly, and its other keys are refused when the file is read.
func TestLoadSessionConfigMatchesToolNamespaceValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		toolset string
		wantErr string
	}{
		{name: "native null defaults to an empty object", toolset: `{"native":null}`},
		{name: "mcp null is refused, unlike the legacy tools.mcp", toolset: `{"mcp":null}`, wantErr: "mcp must be an object"},
		{name: "unknown toolset key", toolset: `{"mcp_servers":[]}`, wantErr: `unknown field "mcp_servers"`},
		{name: "unknown native tool with suggestion", toolset: `{"native":{"memry":{}}}`, wantErr: "tools.native.memry is not supported; did you mean memory?"},
		{name: "mcp node must be an object", toolset: `{"mcp":[]}`, wantErr: "mcp"},
		{name: "native node must be an object", toolset: `{"native":[]}`, wantErr: "native"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadLayoutConfig(t, layouttest.Spec{ToolsetRaw: test.toolset})
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadSessionConfig() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("LoadSessionConfig() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestNormalizeMCPServersNullDenyToolsUsesDefault(t *testing.T) {
	servers, err := normalizeMCPServers([]byte(`[{"id":"fixture","config_path":"server.json","enabled":null,"deny_tools":null}]`), "/workspace", "/session/setting.json")
	if err != nil {
		t.Fatalf("normalizeMCPServers() error = %v", err)
	}
	if len(servers) != 1 || servers[0].Enabled || servers[0].DenyTools == nil || len(servers[0].DenyTools) != 0 {
		t.Fatalf("normalized MCP server = %#v, want disabled server with empty deny list", servers)
	}
}
