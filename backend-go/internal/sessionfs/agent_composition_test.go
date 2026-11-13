package sessionfs

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// agentCompositionFixture is a layout session for the template characterization (DECISIONS,
// unified agent runs, template T0): the caller profile dev/coder runs the toolset coding/default,
// which carries the session's tools (exec, async, the subagent section), a hook and an MCP
// server; the session has a state context and a sandbox; the profile denies skill; two models,
// one with efforts. The agent finder names its own toolset extra/search.
func agentCompositionFixture(t *testing.T) Location {
	t.Helper()
	fixture := layouttest.Write(t, layouttest.Spec{
		Models: map[string]map[string]any{
			"openai/default": {"provider": "openai", "model": "d"},
			"openai/cheap":   {"provider": "openai", "model": "c", "efforts": []string{"low", "high"}},
		},
		ProfileExtra: map[string]any{"tools": map[string]any{"deny": []string{"skill"}}},
		ToolsetExtra: map[string]any{"hooks": map[string]any{"pre_tool": []any{map[string]any{"name": "a", "argv": []any{"true"}}}}},
		MCP:          map[string]map[string]any{"srv/one": {}},
		RawSettings:  map[string]json.RawMessage{"statecontext": json.RawMessage(`{"enabled":true,"triggers":{"compaction":{"tools":["async"]}}}`)},
		Settings:     map[string]any{"sandbox": envSandbox(map[string]any{"write": map[string]any{"paths": []string{"{workspace}", "{tmp}"}}})},
		Native: map[string]any{"exec": map[string]any{}, "async": map[string]any{},
			"subagent": map[string]any{"agents": map[string]any{
				"plain":  map[string]any{},
				"finder": map[string]any{"toolset": "extra/search"},
				"own":    map[string]any{"model": "openai/default"},
			}}},
	})
	writeToolsetFile(t, fixture, "extra/search", `{"native":{"read_image":{},"skill":{}}}`)
	config, err := Load(LoadOptions{WorkingDir: fixture.Workspace, GlobalDir: fixture.GlobalDir, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := Resolve(config.Roots, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return location
}

// The config an agent's toolset is loaded as (AgentToolsets) is the caller's composition (root,
// session, caller profile settings) with the profile's toolset replaced by the agent's, and it is
// not the caller's config with a different tool surface: it also lacks what loadLayoutSessionSettings
// sets after the composition (the profile's identity and the selected model and effort), the
// subagent section (dropped on purpose) and the state context (deleted on purpose). Everything
// else is equal, whichever model the caller selected. Its ModelPath is not the selected model (nor a catalog model file). This is the evidence for the template
// decision: AgentToolsets cannot stand in for the run config of a toolset agent.
func TestAgentToolsetConfigDiffersFromTheCallersOnlyInToolsSelectionAndIdentity(t *testing.T) {
	location := agentCompositionFixture(t)
	tools := []string{"NativeExec", "NativeReadImage", "NativeAsync", "NativeSkill", "MCPServers", "Hooks", "StateContext", "NativeSubagent", "AgentToolsets"}
	// The workspace layout sets the profile's private folders, its self-edit policy, the sandbox rules
	// that hide the other profiles' folders, and the layout marker after the composition too.
	identity := []string{"Profile", "DefaultProfile", "Profiles", "ProfileNames", "ProfilePath", "RemotesDir", "SettingSources",
		"ProfileDataDir", "ProfileOwnDir", "ProfileSelfEdit", "SelfEditPaths", "Sandbox", "WorkspaceLayout"}
	for requested, selection := range map[string][]string{
		"":                  {"ModelPath", "ModelPaths"},
		"openai/cheap@high": {"ModelPath", "ModelPaths", "ReasoningEffort"},
	} {
		caller, err := LoadSessionSettings(location, "", requested)
		if err != nil {
			t.Fatal(err)
		}
		derived, ok := caller.AgentToolsets["extra/search"]
		if !ok {
			t.Fatalf("no derived config: %v", caller.AgentToolsets)
		}
		var differ []string
		a, b := reflect.ValueOf(caller), reflect.ValueOf(derived)
		for i := range a.NumField() {
			if !reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
				differ = append(differ, a.Type().Field(i).Name)
			}
		}
		want := slices.Concat(tools, identity, selection)
		slices.Sort(want)
		slices.Sort(differ)
		if !slices.Equal(differ, want) {
			t.Fatalf("model %q: fields that differ = %v, want %v", requested, differ, want)
		}
		// The derived model fields are whatever the plain settings load left (not a catalog
		// model file, and no effort); nothing reads them (buildAgentToolsets sets Model).
		if derived.NativeSubagent != nil || derived.StateContext.Enabled || derived.ModelPath == caller.ModelPath || derived.ReasoningEffort != "" {
			t.Fatalf("derived subagent %v, state context %v, model %q@%q", derived.NativeSubagent, derived.StateContext.Enabled, derived.ModelPath, derived.ReasoningEffort)
		}
		if caller.NativeSubagent == nil || !caller.StateContext.Enabled || caller.ModelPath == "" {
			t.Fatal("the caller lost its subagent section, state context or model")
		}
		// The caller profile's tools.deny is part of the profile layer both share.
		if !slices.Equal(derived.ProfileTools.Deny, []string{"skill"}) {
			t.Fatalf("derived deny = %v", derived.ProfileTools.Deny)
		}
	}
}
