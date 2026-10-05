package sessionfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSessionConfigMatchesToolNamespaceValidation(t *testing.T) {
	rootDir := t.TempDir()
	sessionDir := filepath.Join(rootDir, "session")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(sessionDir, "model.json")
	if err := os.WriteFile(modelPath, []byte(`{"model":"test-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	location := Location{Directory: sessionDir, Root: Root{GlobalJadRoot: rootDir}}

	for _, test := range []struct {
		name    string
		setting string
		wantErr string
	}{
		{
			name:    "native and mcp null default to empty objects",
			setting: `{"tools":{"native":null,"mcp":null}}`,
		},
		{
			name:    "unknown tool node",
			setting: `{"tools":{"mcp_servers":[]}}`,
			wantErr: "tools.mcp_servers is not supported",
		},
		{
			name:    "unknown native tool with suggestion",
			setting: `{"tools":{"native":{"memry":{}}}}`,
			wantErr: "tools.native.memry is not supported; did you mean memory?",
		},
		{
			name:    "mcp node must be an object",
			setting: `{"tools":{"mcp":[]}}`,
			wantErr: "tools.mcp must be an object",
		},
		{
			name:    "tools must be an object",
			setting: `{"tools":null}`,
			wantErr: "tools must be an object",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(test.setting), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadSessionConfig(location, "", "")
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
