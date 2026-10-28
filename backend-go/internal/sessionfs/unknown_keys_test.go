package sessionfs

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionFilesAndMCPServersRefuseUnknownKeysAndNullBooleans(t *testing.T) {
	for _, test := range []struct {
		name    string
		raw     string
		mcp     bool
		wantErr string
	}{
		{name: "session_files typo", raw: `{"enable":false}`, wantErr: "session_files.enable is not supported; did you mean enabled?"},
		{name: "session_files null boolean", raw: `{"enabled":null}`, wantErr: "session_files.enabled must be a boolean"},
		{name: "session_files valid", raw: `{"enabled":false,"max_depth":3}`},
		{name: "mcp typo", mcp: true, raw: `[{"id":"a","config_path":"a.json","deny_tool":["x"]}]`, wantErr: "mcp_servers[0].deny_tool is not supported; did you mean deny_tools?"},
		{name: "mcp enable typo", mcp: true, raw: `[{"id":"a","config_path":"a.json","enable":false}]`, wantErr: "mcp_servers[0].enable is not supported; did you mean enabled?"},
		{name: "mcp null enabled", mcp: true, raw: `[{"id":"a","config_path":"a.json","enabled":null}]`, wantErr: "mcp_servers[0].enabled must be a boolean"},
		{name: "mcp valid", mcp: true, raw: `[{"id":"a","config_path":"a.json","enabled":true,"deny_tools":["x"]}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var err error
			if test.mcp {
				_, err = normalizeMCPServers(json.RawMessage(test.raw), "/w", "/s/setting.json")
			} else {
				_, err = normalizeSessionFileListing(json.RawMessage(test.raw), "/s/setting.json")
			}
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("error = %v, want none", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}
