package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// A single toolset given as a string must load the same tools and MCP servers as before
// toolsets could be a list. The expected text is fixed by hand; the MCP servers come in the
// order of the toolset file, which the fixture writes with sorted keys.
func TestSingleStringToolsetKeepsItsMergedOutput(t *testing.T) {
	config, err := loadToolsetSession(t, layouttest.Spec{
		Native: map[string]any{
			"grep": map[string]any{"max_results": 11},
			"glob": map[string]any{"max_results": 22},
		},
		MCP: map[string]map[string]any{
			"srv/zulu":  {"deny_tools": []string{"rm"}},
			"srv/alpha": {},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if config.Warnings != nil {
		t.Errorf("Warnings = %v, want nil", config.Warnings)
	}

	const wantGrep = `{"Policy":{"Paths":[],"UnrestrictedPaths":false,"Within":null},"Binary":"rg","MaxResults":11,"MaxMatchesPerFile":20,"MaxBytes":65536,"MaxLineChars":240,"MaxFileBytes":8388608,"TimeoutMs":10000,"KillGraceMs":250,"KillDeadlineMs":1000,"DrainAfterExitMs":500}`
	const wantGlob = `{"Policy":{"Paths":[],"UnrestrictedPaths":false,"Within":null},"Binary":"rg","IncludeHidden":true,"MaxResults":22,"MaxBytes":65536,"MaxLineChars":1000,"TimeoutMs":10000,"KillGraceMs":2000,"KillDeadlineMs":2000,"DrainAfterExitMs":500}`
	const wantServers = `[{"ID":"alpha","ConfigPath":"","Enabled":true,"DenyTools":[]},{"ID":"zulu","ConfigPath":"","Enabled":true,"DenyTools":["rm"]}]`

	assertJSON(t, "NativeGrep", config.NativeGrep, wantGrep)
	assertJSON(t, "NativeGlob", config.NativeGlob, wantGlob)

	servers := append([]MCPServerReference(nil), config.MCPServers...)
	for index := range servers {
		wantSuffix := filepath.FromSlash("/mcp/srv/" + servers[index].ID + ".json")
		if !strings.HasSuffix(servers[index].ConfigPath, wantSuffix) {
			t.Errorf("server %s ConfigPath = %q, want it to end with %q", servers[index].ID, servers[index].ConfigPath, wantSuffix)
		}
		servers[index].ConfigPath = ""
	}
	assertJSON(t, "MCPServers", servers, wantServers)
}

func assertJSON(t *testing.T, name string, value any, want string) {
	t.Helper()
	got, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s =\n%s\nwant\n%s", name, got, want)
	}
}
