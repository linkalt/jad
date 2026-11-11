package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// Template T0 (DECISIONS, unified agent runs): the run config of an agent is the caller's whole
// config (root, session, caller profile, with the model the caller selected for the run) with the
// agent's model, effort and prompt layered on top, for a toolset agent too. The model a caller
// selected reaches every agent without a model of its own; an agent's own model replaces it,
// with the agent's effort. The config the toolset was loaded as is not a usable base for the run:
// it has no subagent section (so no folder for the child's transcript) and not the selected model.
func TestAgentRunConfigLayersTheAgentOverTheCallersSelection(t *testing.T) {
	fixture := layouttest.Write(t, layouttest.Spec{
		Models: map[string]map[string]any{
			"openai/default": {"provider": "openai", "model": "d"},
			"openai/cheap":   {"provider": "openai", "model": "c", "efforts": []string{"low", "high"}},
		},
		Native: map[string]any{"exec": map[string]any{}, "subagent": map[string]any{"agents": map[string]any{
			"plain":  map[string]any{"instructions": "plain prompt"},
			"finder": map[string]any{"toolset": "extra/search"},
			"own":    map[string]any{"model": "openai/default"},
		}}},
	})
	toolset := filepath.Join(fixture.Root, "tools", "extra", "search.json")
	if err := os.MkdirAll(filepath.Dir(toolset), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(toolset, []byte(`{"native":{"read_image":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := sessionfs.Resolve(loaded.Roots, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	caller, err := sessionfs.LoadSessionSettings(location, "", "openai/cheap@high")
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string]sessionfs.NativeSubagentAgent{}
	for _, agent := range caller.NativeSubagent.Agents {
		agents[agent.Name] = agent
	}
	for name, want := range map[string]struct{ model, effort, prompt string }{
		"plain":  {"c", "high", "plain prompt"},
		"finder": {"c", "high", ""},
		"own":    {"d", "", ""},
	} {
		config, err := childConfig(RunTemplate{Base: caller, Agent: agents[name]})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if config.Model.Name != want.model || config.ReasoningEffort != want.effort || config.Model.PromptText != want.prompt {
			t.Fatalf("%s: model %q effort %q prompt %q, want %+v", name, config.Model.Name, config.ReasoningEffort, config.Model.PromptText, want)
		}
		if config.NativeSubagent == nil || config.NativeSubagent.StorageDir != caller.NativeSubagent.StorageDir {
			t.Fatalf("%s: the run does not keep the caller's subagent section", name)
		}
	}

	// The conflict: the toolset's derived config as the run's base.
	derived := caller.AgentToolsets["extra/search"]
	resolver := newEnvTestResolver(t)
	if _, _, err := resolver.openEphemeralStore(derived, RunSpec{CallID: "call_1"}); err == nil || !strings.Contains(err.Error(), "tools.native.subagent is not configured") {
		t.Fatalf("a run on the derived config opened its store: %v", err)
	}
	if derived.ModelPath == caller.ModelPath || derived.ReasoningEffort == caller.ReasoningEffort {
		t.Fatalf("the derived config carries the caller's selection %q@%q", derived.ModelPath, derived.ReasoningEffort)
	}
}
