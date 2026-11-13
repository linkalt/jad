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
			"read_image": map[string]any{"max_image_bytes": 11},
			"skill":      map[string]any{"max_bytes": 22},
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

	const wantGrep = `{"Policy":{"Paths":[],"UnrestrictedPaths":false,"Within":null},"MaxImageBytes":11}`
	const wantGlob = `{"MaxBytes":22}`
	const wantServers = `[{"ID":"alpha","ConfigPath":"","Enabled":true,"DenyTools":[],"Resources":false,"Prompts":false,"TokenPath":""},{"ID":"zulu","ConfigPath":"","Enabled":true,"DenyTools":["rm"],"Resources":false,"Prompts":false,"TokenPath":""}]`

	// The workspace layout adds the folders the image tool must not read (the global auth folder and the
	// other profiles' private folders) to the policy; only the settings of the toolset are pinned.
	var readImage struct{ MaxImageBytes int }
	if raw, _ := json.Marshal(config.NativeReadImage); json.Unmarshal(raw, &readImage) != nil || readImage.MaxImageBytes != 11 || len(config.NativeReadImage.Policy.Paths) != 0 || config.NativeReadImage.Policy.UnrestrictedPaths {
		t.Errorf("NativeReadImage = %s, want max_image_bytes 11 and no path rules", raw)
	}
	_ = wantGrep
	assertJSON(t, "NativeSkill", config.NativeSkill, wantGlob)

	servers := append([]MCPServerReference(nil), config.MCPServers...)
	for index := range servers {
		wantSuffix := filepath.FromSlash("/mcp/srv/" + servers[index].ID + ".json")
		if !strings.HasSuffix(servers[index].ConfigPath, wantSuffix) {
			t.Errorf("server %s ConfigPath = %q, want it to end with %q", servers[index].ID, servers[index].ConfigPath, wantSuffix)
		}
		servers[index].ConfigPath = ""
		wantToken := filepath.FromSlash("/state/.mcp-oauth/srv/" + servers[index].ID + ".json")
		if !strings.HasSuffix(servers[index].TokenPath, wantToken) {
			t.Errorf("server %s TokenPath = %q, want it to end with %q", servers[index].ID, servers[index].TokenPath, wantToken)
		}
		servers[index].TokenPath = ""
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
