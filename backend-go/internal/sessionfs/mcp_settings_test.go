package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeMCPServersDefaultsAndResolvesConfigPaths(t *testing.T) {
	cwd := t.TempDir()
	servers, err := normalizeMCPServers(json.RawMessage(`[
		{"id":"local-server","config_path":"mcp/server.json"},
		{"id":"remote-server","config_path":"/etc/mcp/remote.json","enabled":true,"deny_tools":["dangerous"]}
	]`), cwd, "/session/setting.json")
	if err != nil {
		t.Fatalf("normalizeMCPServers() error = %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("normalized server count = %d, want 2", len(servers))
	}
	if servers[0].ID != "local-server" || servers[0].ConfigPath != filepath.Join(cwd, "mcp", "server.json") || servers[0].Enabled {
		t.Errorf("default server settings = %+v", servers[0])
	}
	if servers[0].DenyTools == nil || len(servers[0].DenyTools) != 0 {
		t.Errorf("default deny_tools = %#v, want empty list", servers[0].DenyTools)
	}
	if servers[1].ID != "remote-server" || servers[1].ConfigPath != filepath.Clean("/etc/mcp/remote.json") || !servers[1].Enabled || len(servers[1].DenyTools) != 1 || servers[1].DenyTools[0] != "dangerous" {
		t.Errorf("explicit server settings = %+v", servers[1])
	}
}

func TestNormalizeMCPServersRejectsInvalidReferences(t *testing.T) {
	cwd := t.TempDir()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "null", raw: `null`, want: "mcp_servers must be an array"},
		{name: "entry is not object", raw: `[{"id":"one","config_path":"x"},null]`, want: "mcp_servers[1] must be an object"},
		{name: "invalid id", raw: `[{"id":"bad/id","config_path":"x"}]`, want: "id may contain only"},
		{name: "duplicate id", raw: `[{"id":"same","config_path":"x"},{"id":"same","config_path":"y"}]`, want: "id duplicates same"},
		{name: "enabled is not boolean", raw: `[{"id":"one","config_path":"x","enabled":"yes"}]`, want: "enabled must be a boolean"},
		{name: "deny tools is not array", raw: `[{"id":"one","config_path":"x","deny_tools":"dangerous"}]`, want: "deny_tools must be an array"},
		{name: "duplicate denied tool", raw: `[{"id":"one","config_path":"x","deny_tools":["x","x"]}]`, want: "deny_tools must not contain duplicates"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalizeMCPServers(json.RawMessage(test.raw), cwd, "/session/setting.json")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("normalizeMCPServers() error = %v, want substring %q", err, test.want)
			}
		})
	}
}
