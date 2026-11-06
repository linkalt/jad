package sessionfs

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeSubagentReadOnlyAgentIsNotAWriter(t *testing.T) {
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{
		"finder": {"tools": ["exec", "read_image"], "read_only": true},
		"fixer": {"tools": ["exec"]},
		"plain": {"tools": ["exec"], "read_only": false},
		"unset": {"tools": ["exec"], "read_only": null}}}`), "/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	finder, _ := got.Agent("finder")
	if !finder.ReadOnly || finder.ChangesFiles() || finder.ChangesWorkspace() || len(finder.ChangeTools()) != 0 {
		t.Fatalf("finder: %+v, change tools %v", finder, finder.ChangeTools())
	}
	for _, name := range []string{"fixer", "plain", "unset"} {
		agent, _ := got.Agent(name)
		if agent.ReadOnly || !agent.ChangesFiles() {
			t.Fatalf("%s: %+v", name, agent)
		}
	}
	// The fingerprint of an agent without read_only is what it was before the key existed.
	fixer, _ := got.Agent("fixer")
	encoded, _ := json.Marshal(fixer)
	if strings.Contains(string(encoded), "ReadOnly") {
		t.Fatalf("fixer encodes ReadOnly: %s", encoded)
	}
}

func TestNativeSubagentReadOnlyRefusals(t *testing.T) {
	for name, test := range map[string]struct{ section, want string }{
		"not a boolean":   {`{"agents":{"a":{"tools":["exec"],"read_only":"yes"}}}`, "read_only"},
		"without exec":    {`{"agents":{"a":{"tools":["read_image"],"read_only":true}}}`, "read_only only makes exec calls read-only, and tools does not list exec"},
		"without tools":   {`{"agents":{"a":{"read_only":true}}}`, "tools does not list exec"},
		"with tool_smith": {`{"agents":{"a":{"tools":["exec","tool_smith"],"read_only":true}}}`, "read_only does not cover tool_smith"},
		"with subagent":   {`{"agents":{"a":{"tools":["exec","subagent"],"read_only":true}}}`, "read_only does not cover subagent"},
		"with worktree":   {`{"agents":{"a":{"tools":["exec"],"read_only":true,"isolation":"worktree"}}}`, "isolation worktree needs a tool that changes files"},
	} {
		_, err := normalizeNativeSubagentSettings(subagentSettingsFor(test.section), "/s/setting.json", "/w", "")
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
}
