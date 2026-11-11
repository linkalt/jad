package sessionfs

import (
	"path/filepath"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// Template T0 (DECISIONS, unified agent runs): the settings stamp of a session load sees an edit
// of every file an agent's or another profile's configuration comes from, so a cached caller
// config is not used past such an edit: the toolset an agent names and the MCP server file that
// toolset lists, an agent folder's agent.json, and another profile of the session (a
// profile_message target, loaded per message on its own stamp, is still checked here at load).
// MCP server files are the exception, for the session's servers and an agent toolset's alike: the
// config keeps only their path, and the MCP manager reads the file again on every LoadTools (each
// prepared run), so an edit reaches the next run without a new load.
func TestSettingsStampSeesEveryTemplateInputButMCPServerFiles(t *testing.T) {
	cwd := t.TempDir()
	agents := filepath.Join(cwd, "agents")
	writeFileT(t, filepath.Join(agents, "helper", "agent.json"), `{"schema_version":1,"description":"one"}`)
	writeFileT(t, filepath.Join(agents, "helper", "prompt.md"), "be brief")
	fixture := layouttest.Write(t, layouttest.Spec{
		CWD: cwd,
		Profiles: map[string]map[string]any{
			"dev/coder":     {"models": []string{"openai/default"}, "toolset": "coding/default"},
			"review/target": {"models": []string{"openai/default"}},
		},
		MCP: map[string]map[string]any{"srv/own": {}},
		Native: map[string]any{"subagent": map[string]any{"agents_dir": []string{agents},
			"agents": map[string]any{"finder": map[string]any{"toolset": "extra/search"}}}},
	})
	toolset := filepath.Join(fixture.Root, "tools", "extra", "search.json")
	server := filepath.Join(fixture.Root, "mcp", "srv", "agent.json")
	writeFileT(t, server, `{"schema_version":1,"transport":{"type":"stdio","command":"x"}}`)
	writeFileT(t, toolset, `{"native":{"read_image":{}},"mcp":{"srv/agent":{}}}`)
	edits := map[string]struct {
		path, text string
		live       bool
	}{
		"agent toolset":    {toolset, `{"native":{"read_image":{},"skill":{}},"mcp":{"srv/agent":{}}}`, false},
		"own MCP server":   {filepath.Join(fixture.Root, "mcp", "srv", "own.json"), `{"schema_version":1,"transport":{"type":"stdio","command":"z"}}`, true},
		"agent MCP server": {server, `{"schema_version":1,"transport":{"type":"stdio","command":"y"}}`, true},
		"agent.json":       {filepath.Join(agents, "helper", "agent.json"), `{"schema_version":1,"description":"two"}`, false},
		"another profile":  {filepath.Join(fixture.Root, "profiles", "review", "target.json"), `{"models":["openai/default"],"instructions":"x"}`, false},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			backdateTree(t, fixture.Root, cwd)
			config, stamp := stampedLoad(t, fixture)
			if _, ok := config.AgentToolsets["extra/search"]; !ok {
				t.Fatalf("the agent toolset was not loaded: %v", config.AgentToolsets)
			}
			writeFileT(t, edit.path, edit.text)
			if stamp.Current() != edit.live {
				t.Fatalf("stamp current = %v after %s was edited, want %v", stamp.Current(), edit.path, edit.live)
			}
		})
	}
}
