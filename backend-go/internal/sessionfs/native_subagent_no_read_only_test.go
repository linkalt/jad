package sessionfs

import (
	"strings"
	"testing"
)

// read_only was removed from subagent agents. A config that still sets it is refused by name,
// whatever its value and wherever the agent is written: ignoring it would turn an agent that
// was protected into one that writes the workspace.
func TestSubagentAgentReadOnlyKeyIsRefusedByName(t *testing.T) {
	const want = "read_only was removed; protection now comes only from isolation; remove the key, and set isolation to \"worktree\" if this child must not write the workspace"
	for name, section := range map[string]string{
		"true":          `{"agents":{"a":{"tools":["exec"],"read_only":true}}}`,
		"false":         `{"agents":{"a":{"read_only":false}}}`,
		"null":          `{"agents":{"a":{"read_only":null}}}`,
		"with isolated": `{"agents":{"a":{"read_only":true,"isolation":"worktree"}}}`,
	} {
		_, err := normalizeNativeSubagentSettings(subagentSettingsFor(section), "/s/setting.json", "/w", "")
		if err == nil || !strings.Contains(err.Error(), "tools.native.subagent.agents.a.read_only was removed") || !strings.Contains(err.Error(), want) {
			t.Errorf("inline %s: error %v", name, err)
		}
	}
	root := subagentTree(t, map[string]string{"agents/finder/agent.json": `{"schema_version":1,"read_only":true}`})
	_, err := loadWithDirs(t, root, `{"agents_dir":["agents"]}`)
	if err == nil || !strings.Contains(err.Error(), "agents/finder/agent.json") || !strings.Contains(err.Error(), want) {
		t.Errorf("agent.json: error %v", err)
	}
}

// With read_only gone, whether an agent can change files and whether it can change the session's
// own files depend on its tool set and its isolation, and on nothing else.
func TestSubagentChangesFilesFollowToolSetAndIsolation(t *testing.T) {
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{
		"bare": {},
		"bare_isolated": {"isolation": "worktree"},
		"exec": {"tools": ["exec"]},
		"exec_isolated": {"tools": ["exec"], "isolation": "worktree"},
		"readers": {"tools": ["read_image"]},
		"smith": {"tools": ["read_image", "tool_smith"]}}}`), "/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]struct{ files, workspace bool }{
		"bare":          {true, true},
		"bare_isolated": {true, false},
		"exec":          {true, true},
		"exec_isolated": {true, false},
		"readers":       {false, false},
		"smith":         {true, true},
	} {
		agent, _ := got.Agent(name)
		if agent.ChangesFiles() != want.files || agent.ChangesWorkspace() != want.workspace {
			t.Errorf("%s: ChangesFiles %v ChangesWorkspace %v, want %v %v", name, agent.ChangesFiles(), agent.ChangesWorkspace(), want.files, want.workspace)
		}
	}
	// An agent's catalog with no change tool (NoChangeTools, set by the subagent tool) is the one other input.
	bare, _ := got.Agent("bare")
	bare.NoChangeTools = true
	if bare.ChangesFiles() || bare.ChangesWorkspace() {
		t.Errorf("a catalog with no change tool still changes files: %+v", bare)
	}
}
