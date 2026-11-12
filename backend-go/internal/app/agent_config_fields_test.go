package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
	"jad/backend-go/internal/tools/native"
)

// agentConfigField is where an agent's run and its tools take one SessionConfig field from
// (DECISIONS, unified agent runs, R3a). A toolset agent has two configs today: its run config
// (childConfig: the caller's config with the agent's model, effort and prompt) and its tools
// config (AgentToolsets[key]: the caller's composition with the agent's toolset in place of the
// profile's, loaded by loadAgentToolsets and finished for the owner run by native.NewAgentToolConfigs).
type agentConfigField int

const (
	// fieldLayer: root, session and caller profile settings, equal in both configs.
	fieldLayer agentConfigField = iota + 1
	// fieldCallerOnly: set on the caller's config after the composition (profile identity,
	// the model selection, warnings). The run takes the caller's; the tools do not read it.
	fieldCallerOnly
	// fieldToolSurface: the tool settings the toolset layer replaces. The agent's tools are
	// built from the tools config at the owner's prepare; the run carries the caller's
	// value and does not read it.
	fieldToolSurface
	// fieldConflict: read by the run and by the tools, with different values. No single
	// SessionConfig can carry both, so an overlay of caller facts on the tools config (or of
	// the toolset on the run config) changes one side.
	fieldConflict
)

// agentConfigFields classifies every SessionConfig field; a field added to SessionConfig fails
// TestAgentConfigFieldsAreClassifiedForTheRunAndTheTools until it is placed here.
var agentConfigFields = map[string]agentConfigField{
	// Profile identity and the caller's selection.
	"Profile": fieldCallerOnly, "DefaultProfile": fieldCallerOnly, "Profiles": fieldCallerOnly,
	"ProfileNames": fieldCallerOnly, "ProfilePath": fieldCallerOnly, "Category": fieldCallerOnly,
	"ProfileOwnDir": fieldCallerOnly, "ProfileDataDir": fieldCallerOnly, "SettingSources": fieldCallerOnly, "ProfileSelfEdit": fieldCallerOnly,
	"RemotesDir": fieldCallerOnly, "Warnings": fieldCallerOnly,
	"ModelPath": fieldCallerOnly, "ModelPaths": fieldCallerOnly, "ModelFault": fieldCallerOnly, "ReasoningEffort": fieldCallerOnly,

	// Tool surface.
	"NativeExec": fieldToolSurface, "NativeReadImage": fieldToolSurface, "NativeLSP": fieldToolSurface,
	"ExtensionWebSearch": fieldToolSurface, "NativeAsync": fieldToolSurface, "NativeTask": fieldToolSurface,
	"NativeCodeContext": fieldToolSurface, "NativeRecords": fieldToolSurface, "NativeRequestDraft": fieldToolSurface,
	"NativeSkill": fieldToolSurface, "ExtensionDecideSystemone": fieldToolSurface,
	// A toolset may not configure profile_message; a toolset child is given the owner's tool.
	"NativeProfileMessage": fieldToolSurface,
	"NativeToolSmith":      fieldToolSurface, "NativeTranscript": fieldToolSurface, "NativeTranscripts": fieldToolSurface,
	"NativeProfile": fieldToolSurface, "NativeLoadTool": fieldToolSurface, "MCPServers": fieldToolSurface,
	"AgentToolsets": fieldToolSurface,

	// Read by both sides with different values.
	// Run: the caller's subagent_stop (subagentStopHook). Tools: the toolset's pre_tool and
	// post_tool hooks (buildNativeTools and NewMCPCopies over the toolset config).
	"Hooks": fieldConflict,
	// Run: the caller's section, for the child's transcript folder (openEphemeralStore).
	// Tools: none, so a toolset gives no subagent tool of its own (loadAgentToolsets drops it).
	"NativeSubagent": fieldConflict,
	// Run: the caller's state context (stateContextFor). Tools: none, because the providers
	// read the session's own tools (compose deletes statecontext for a toolset).
	"StateContext": fieldConflict,
	// Run: the agent's own model, else the caller's selected one (childConfig). Tools: the
	// caller's model, whose key env their shells hide (NewAgentToolConfigs sets it).
	"Model": fieldConflict,

	// Layers shared by both.
	"SessionName": fieldLayer, "ModelDefault": fieldLayer, "Models": fieldLayer, "SettingPath": fieldLayer,
	"StateDir": fieldLayer, "MessagesPath": fieldLayer, "MessagesBackupPath": fieldLayer, "UsageCachePath": fieldLayer,
	"StagingDir": fieldLayer, "StoragePaths": fieldLayer, "WorkingDir": fieldLayer, "SessionFiles": fieldLayer,
	"FilePolicy": fieldLayer, "GlobalJadRoot": fieldLayer, "Libraries": fieldLayer, "HTTPDebug": fieldLayer, "ToolDefinitionPaths": fieldLayer,
	"MaxToolRounds": fieldLayer, "MaxParallelToolCalls": fieldLayer, "ToolCallScheduling": fieldLayer,
	"ProviderRetry": fieldLayer, "ProviderStream": fieldLayer, "ProviderNetwork": fieldLayer,
	"MaxRunTokens": fieldLayer, "ModelFallback": fieldLayer, "Sandbox": fieldLayer, "Isolation": fieldLayer,
	"Budget": fieldLayer, "ProjectInstructions": fieldLayer, "MaxRunCostUSD": fieldLayer, "MaxRunDurationMS": fieldLayer,
	"Checkpoint": fieldLayer, "PlanMode": fieldLayer, "ProfileTools": fieldLayer, "ImageResize": fieldLayer,
	"ImageStore": fieldLayer, "BackupTurns": fieldLayer, "Compaction": fieldLayer, "Persistence": fieldLayer,
	"FrontCompaction": fieldLayer, "FrontCompactionConfig": fieldLayer, "PromptDocuments": fieldLayer,
	"ProjectInstructionFiles": fieldLayer,
}

// The R3a evidence: every SessionConfig field is classified, the layer fields are equal in the
// caller's, the run's and the tools' config, the run keeps every caller field but the model, and
// each conflict field has a different value for the run and for the tools of a toolset agent
// with its own model, a caller that selected a model and a profile toolset with hooks and a
// state context. A shared config for both would change one side's behaviour.
func TestAgentConfigFieldsAreClassifiedForTheRunAndTheTools(t *testing.T) {
	typ := reflect.TypeFor[sessionfs.SessionConfig]()
	var names []string
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		names = append(names, name)
		if agentConfigFields[name] == 0 {
			t.Errorf("SessionConfig.%s is not classified for an agent's run and tools", name)
		}
	}
	for name := range agentConfigFields {
		if !slices.Contains(names, name) {
			t.Errorf("classified field %s is not in SessionConfig", name)
		}
	}

	fixture := layouttest.Write(t, layouttest.Spec{
		Models: map[string]map[string]any{
			"openai/default": {"provider": "openai", "model": "d", "api_key_env": "AGENT_KEY"},
			"openai/cheap":   {"provider": "openai", "model": "c", "efforts": []string{"low", "high"}, "api_key_env": "CALLER_KEY"},
		},
		ToolsetExtra: map[string]any{"hooks": map[string]any{"pre_tool": []any{map[string]any{"name": "a", "argv": []any{"true"}}}}},
		RawSettings:  map[string]json.RawMessage{"statecontext": json.RawMessage(`{"enabled":true,"triggers":{"compaction":{"tools":["async"]}}}`)},
		Native: map[string]any{"exec": map[string]any{}, "async": map[string]any{}, "subagent": map[string]any{"agents": map[string]any{
			"scout": map[string]any{"toolset": "extra/search", "model": "openai/default"},
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
	if caller, err = caller.WithModel(); err != nil {
		t.Fatal(err)
	}
	run, err := childConfig(RunTemplate{Base: caller, Agent: caller.NativeSubagent.Agents[0]})
	if err != nil {
		t.Fatal(err)
	}
	// The tools config as the owner run's prepare finishes it for the native toolset tools and,
	// with the agent's sandbox when it covers mcp, for the toolset MCP servers.
	agentTools := native.NewAgentToolConfigs(caller)
	tools := agentTools.Native["extra/search"]
	if len(agentTools.MCP) != 1 || agentTools.MCP[0].Toolset != "extra/search" || !reflect.DeepEqual(agentTools.MCP[0].Config, tools) {
		t.Fatalf("the MCP view is not the native tools config: %+v", agentTools.MCP)
	}

	field := func(config sessionfs.SessionConfig, name string) any {
		return reflect.ValueOf(config).FieldByName(name).Interface()
	}
	for _, name := range names {
		callerValue, runValue, toolsValue := field(caller, name), field(run, name), field(tools, name)
		switch agentConfigFields[name] {
		case fieldLayer:
			if !reflect.DeepEqual(callerValue, toolsValue) || !reflect.DeepEqual(callerValue, runValue) {
				t.Errorf("layer field %s differs: caller %v, run %v, tools %v", name, callerValue, runValue, toolsValue)
			}
		case fieldCallerOnly, fieldToolSurface:
			if name == "ModelPath" || name == "ReasoningEffort" {
				continue // the agent's own model, checked below
			}
			if !reflect.DeepEqual(callerValue, runValue) {
				t.Errorf("the run does not keep the caller's %s: %v, want %v", name, runValue, callerValue)
			}
		case fieldConflict:
			if reflect.DeepEqual(runValue, toolsValue) {
				t.Errorf("conflict field %s is equal for the run and the tools (%v); reclassify it", name, runValue)
			}
		}
	}
	if run.Model.Name != "d" || run.Model.APIKeyEnv != "AGENT_KEY" || tools.Model.APIKeyEnv != "CALLER_KEY" {
		t.Fatalf("run model %q key %q, tools key %q", run.Model.Name, run.Model.APIKeyEnv, tools.Model.APIKeyEnv)
	}
	if !reflect.DeepEqual(run.Hooks, caller.Hooks) || run.NativeSubagent != caller.NativeSubagent || !reflect.DeepEqual(run.StateContext, caller.StateContext) {
		t.Fatal("the run does not take the caller's hooks, subagent section and state context")
	}
	if tools.NativeSubagent != nil || tools.StateContext.Enabled || len(tools.Hooks.PreTool) != 0 || len(caller.Hooks.PreTool) == 0 || !caller.StateContext.Enabled {
		t.Fatalf("fixture: tools subagent %v, state context %v, hooks %d (caller %d, state context %v)",
			tools.NativeSubagent, tools.StateContext.Enabled, len(tools.Hooks.PreTool), len(caller.Hooks.PreTool), caller.StateContext.Enabled)
	}
}
