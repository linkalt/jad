package sessionfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// toolsetSpec is a session whose own toolset has only read and the subagent tool, with one
// agent that names the toolset "extra/search".
func toolsetSpec(agent map[string]any) layouttest.Spec {
	return layouttest.Spec{
		Native: map[string]any{
			"read":     map[string]any{},
			"subagent": map[string]any{"agents": map[string]any{"finder": agent}},
		},
	}
}

func writeToolsetFile(t *testing.T, fixture layouttest.Fixture, id, text string) {
	t.Helper()
	path := filepath.Join(fixture.Root, "tools", filepath.FromSlash(id)+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadToolsetSession(t *testing.T, spec layouttest.Spec, extra map[string]string) (SessionConfig, error) {
	t.Helper()
	fixture := layouttest.Write(t, spec)
	for id, text := range extra {
		writeToolsetFile(t, fixture, id, text)
	}
	config, err := Load(LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	location, err := Resolve(config.Roots, fixture.SessionID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return LoadSessionSettings(location, "", "")
}

// An agent's toolset gives the agent its own tool settings. The session's own settings do
// not gain them, and the toolset's settings never carry a subagent tool.
func TestSubagentToolsetIsLoadedApartFromTheSessionToolset(t *testing.T) {
	config, err := loadToolsetSession(t, toolsetSpec(map[string]any{"toolset": "extra/search"}), map[string]string{
		"extra/search": `{"native":{"grep":{},"glob":{},"subagent":{"agents":{"nested":{}}}}}`,
	})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	if config.NativeGrep != nil || config.NativeRead == nil {
		t.Errorf("session tools: grep %v read %v, want read only", config.NativeGrep != nil, config.NativeRead != nil)
	}
	agent, ok := config.NativeSubagent.Agent("finder")
	if !ok || agent.ToolsetKey() != "extra/search" {
		t.Fatalf("agent = %+v, want toolset extra/search", agent)
	}
	derived, ok := config.AgentToolsets["extra/search"]
	if !ok {
		t.Fatalf("AgentToolsets = %v, want extra/search", config.AgentToolsets)
	}
	if derived.NativeGrep == nil || derived.NativeGlob == nil || derived.NativeRead != nil {
		t.Errorf("toolset tools: grep %v glob %v read %v, want grep and glob only", derived.NativeGrep != nil, derived.NativeGlob != nil, derived.NativeRead != nil)
	}
	if derived.NativeSubagent != nil {
		t.Error("the toolset's own subagent entry was kept; a child cannot start a child")
	}
	if derived.StoragePaths.ToolStorage != config.StoragePaths.ToolStorage {
		t.Errorf("toolset storage %q differs from the session's %q", derived.StoragePaths.ToolStorage, config.StoragePaths.ToolStorage)
	}
}

// Two agents that name one toolset share one loaded copy; an agent without a toolset
// adds none.
func TestSubagentToolsetIsLoadedOncePerID(t *testing.T) {
	spec := toolsetSpec(map[string]any{"toolset": "extra/search"})
	spec.Native["subagent"] = map[string]any{"agents": map[string]any{
		"one":   map[string]any{"toolset": "extra/search"},
		"two":   map[string]any{"toolset": "extra/search"},
		"plain": map[string]any{},
	}}
	config, err := loadToolsetSession(t, spec, map[string]string{"extra/search": `{"native":{"grep":{}}}`})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	if len(config.AgentToolsets) != 1 {
		t.Errorf("AgentToolsets has %d entries, want 1", len(config.AgentToolsets))
	}
}

// A statecontext that lists the subagent provider is the session's own; the toolset copy,
// which has no subagent tool, must not fail on it.
func TestSubagentToolsetDoesNotReadTheSessionStateContext(t *testing.T) {
	spec := toolsetSpec(map[string]any{"toolset": "extra/search"})
	spec.Settings = map[string]any{"statecontext": map[string]any{"enabled": true, "triggers": map[string]any{"compaction": map[string]any{"tools": []string{"subagent"}}}}}
	config, err := loadToolsetSession(t, spec, map[string]string{"extra/search": `{"native":{"grep":{}}}`})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	if tools := config.StateContext.Triggers["compaction"].Tools; !config.StateContext.Enabled || len(tools) != 1 || tools[0] != "subagent" {
		t.Errorf("session state context = %+v, want subagent enabled", config.StateContext)
	}
}

func TestSubagentWithoutToolsetLoadsNone(t *testing.T) {
	config, err := loadToolsetSession(t, toolsetSpec(map[string]any{}), nil)
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	if len(config.AgentToolsets) != 0 {
		t.Errorf("AgentToolsets = %v, want none", config.AgentToolsets)
	}
}

func TestSubagentToolsetErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		agent map[string]any
		files map[string]string
		want  string
	}{
		"unknown toolset": {
			agent: map[string]any{"toolset": "extra/missing"},
			files: map[string]string{"extra/search": `{"native":{"grep":{}}}`},
			want:  `agent finder: toolset: toolset "extra/missing" is not available here (available: coding/default, extra/search)`,
		},
		"not a category/name": {
			agent: map[string]any{"toolset": "search"},
			want:  `toolset "search" must be written <category>/<name>`,
		},
		"blank": {
			agent: map[string]any{"toolset": " "},
			want:  "toolset must be a non-empty string",
		},
		"profile_message in the toolset": {
			agent: map[string]any{"toolset": "extra/search"},
			files: map[string]string{"extra/search": `{"native":{"grep":{},"profile_message":{"profiles":["review/reviewer"]}}}`},
			want:  "agent finder uses toolset extra/search: ",
		},
		"profile_message in the toolset names the session and the toolset": {
			agent: map[string]any{"toolset": "extra/search"},
			files: map[string]string{"extra/search": `{"native":{"grep":{},"profile_message":{"profiles":["review/reviewer"]}}}`},
			want:  ".json: tools.native.profile_message is not allowed in a toolset used by an agent (toolset extra/search); a sub-agent cannot message a profile",
		},
		"the toolset's own settings are checked": {
			agent: map[string]any{"toolset": "extra/search"},
			files: map[string]string{"extra/search": `{"native":{"grep":{"max_matchs":1}}}`},
			want:  "agent finder uses toolset extra/search: ",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadToolsetSession(t, toolsetSpec(tc.agent), tc.files)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
