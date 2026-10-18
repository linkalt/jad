package sessionfs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSubagentDenyAgentsDefaultsToEmptyAndNullIsTheDefault(t *testing.T) {
	for _, setting := range []string{`{"agents":{"a":{}}}`, `{"deny_agents":null,"agents":{"a":{}}}`, `{"deny_agents":[],"agents":{"a":{}}}`} {
		got, err := normalizeNativeSubagentSettings(subagentSettingsFor(setting), "/s/setting.json", "/w", "")
		if err != nil || len(got.DenyAgents) != 0 || len(got.Agents) != 1 {
			t.Errorf("%s: err = %v", setting, err)
		}
	}
}

func TestSubagentDenyAgentsRejectsBadValues(t *testing.T) {
	for value, want := range map[string]string{
		`"a"`:          "deny_agents must be an array of agent names",
		`{}`:           "deny_agents must be an array of agent names",
		`[1]`:          "deny_agents must be an array of agent names",
		`[""]`:         "deny_agents[0] must be a non-empty string",
		`["a"," "]`:    "deny_agents[1]: the name must be",
		`["a","a"]`:    "deny_agents lists a twice",
		`[" a "]`:      "deny_agents[0]: the name must be",
		`["bad name"]`: "deny_agents[0]: the name must be",
		`["-x"]`:       "deny_agents[0]: the name must be",
	} {
		_, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"deny_agents":`+value+`,"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("deny_agents %s: err = %v; want %q", value, err, want)
		}
	}
}

func TestSubagentDenyAgentsRemovesInlineAndFolderAgents(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"folderdenied", "folderkept"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "agent.json"), []byte(`{"schema_version":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setting := `{"deny_agents":["inlinedenied","folderdenied","nobody"],"agents":{"inlinedenied":{},"inlinekept":{}},"agents_dir":[` + strconv.Quote(dir) + `]}`
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(setting), "/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, agent := range got.Agents {
		names = append(names, agent.Name)
	}
	if strings.Join(names, ",") != "inlinekept,folderkept" {
		t.Fatalf("agents = %v; want inlinekept,folderkept", names)
	}
	if _, found := got.Agent("folderdenied"); found {
		t.Fatal("a denied folder agent is still found")
	}
}

func TestSubagentDenyAgentsRemovesDeniedChangeAgentBeforeAllowWriteCheck(t *testing.T) {
	setting := `{"deny_agents":["writer"],"agents":{"writer":{"tools":["bash"]},"reader":{}}}`
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(setting), "/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := got.Agent("writer"); found || len(got.Agents) != 1 {
		t.Fatalf("agents = %+v", got.Agents)
	}
}
