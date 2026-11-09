package native

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"jad/backend-go/internal/imagestore"
	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/extension"
)

var definitionDocumentSchema = json.RawMessage(`{
  "type":"object",
  "required":["tools"],
  "properties":{
    "schema_version":{"type":"integer","const":1},
    "tools":{
      "type":"array",
      "minItems":1,
      "items":{
        "type":"object",
        "required":["id","source","name","description","inputSchema"],
        "properties":{
          "id":{"type":"string","minLength":1},
          "source":{"type":"string","enum":["native","extension","mcp"]},
          "server":{"type":"string","minLength":1},
          "name":{"type":"string","minLength":1},
          "description":{"type":"string"},
          "inputSchema":{"type":"object"}
        },
        "additionalProperties":true
      }
    }
  },
  "additionalProperties":true
}`)

// BuildCatalog loads enabled native tools from normalized session settings and
// checks their manifest definitions against the in-code schemas.
func BuildCatalog(config sessionfs.SessionConfig) (*tools.Catalog, error) {
	return BuildCatalogWithLocks(config, NewPathLocks())
}

// BuildCatalogWithLocks shares mutation locks across catalogs owned by one
// Resolver, so write and edit calls serialize even when sessions are resolved
// independently.
func BuildCatalogWithLocks(config sessionfs.SessionConfig, locks *PathLocks) (*tools.Catalog, error) {
	return BuildCatalogWithResources(config, locks, nil, nil)
}

// BuildCatalogWithResources shares resolver-owned stateful stores across tool
// catalogs created for the same configured storage directory.
func BuildCatalogWithResources(config sessionfs.SessionConfig, locks *PathLocks, taskStore *TaskStore, recordsStore *RecordsStore) (*tools.Catalog, error) {
	return BuildCatalogWithSubagents(config, locks, taskStore, recordsStore, nil, nil, nil, nil)
}

// BuildCatalogWithSubagents is BuildCatalogWithResources plus the launcher that runs
// the children of the subagent tool. A nil launcher still lists the tool, and a call
// to it answers that sub-agent runs are unavailable; listing tools needs no launcher.
func BuildCatalogWithSubagents(config sessionfs.SessionConfig, locks *PathLocks, taskStore *TaskStore, recordsStore *RecordsStore, launcher SubagentLauncher, mcp *SubagentMCP, subagentManager *SubagentManager, toolsets ToolsetResources) (*tools.Catalog, error) {
	return BuildCatalogWithMessenger(config, locks, taskStore, recordsStore, launcher, mcp, subagentManager, toolsets, nil)
}

// BuildCatalogWithMessenger is BuildCatalogWithSubagents plus the messenger behind the
// profile_message tool. Without a messenger the tool is not offered.
func BuildCatalogWithMessenger(config sessionfs.SessionConfig, locks *PathLocks, taskStore *TaskStore, recordsStore *RecordsStore, launcher SubagentLauncher, mcp *SubagentMCP, subagentManager *SubagentManager, toolsets ToolsetResources, messenger ProfileMessenger) (*tools.Catalog, error) {
	return BuildCatalogWithAsync(config, locks, taskStore, recordsStore, launcher, mcp, subagentManager, toolsets, messenger, nil, nil)
}

// BuildCatalogWithAsync is BuildCatalogWithMessenger plus the resolver that shares one
// async host per tool storage folder. Without it the async tool opens a host of its own.
func BuildCatalogWithAsync(config sessionfs.SessionConfig, locks *PathLocks, taskStore *TaskStore, recordsStore *RecordsStore, launcher SubagentLauncher, mcp *SubagentMCP, subagentManager *SubagentManager, toolsets ToolsetResources, messenger ProfileMessenger, asyncResources AsyncResources, lazy *tools.LazySet) (*tools.Catalog, error) {
	if config.NativeLSP == nil && config.NativeExec == nil && config.NativeReadImage == nil && config.NativeTask == nil && config.NativeCodeContext == nil && config.NativeRecords == nil && config.ExtensionWebSearch == nil && config.NativeRequestDraft == nil && config.NativeSkill == nil && config.ExtensionDecideSystemone == nil && config.NativeSubagent == nil && config.NativeProfileMessage == nil && config.NativeToolSmith == nil && config.NativeTranscript == nil && config.NativeTranscripts == nil && config.NativeProfile == nil && config.NativeLoadTool == nil && config.NativeAsync == nil {
		return tools.NewCatalog(nil)
	}
	if locks == nil {
		locks = NewPathLocks()
	}
	var asyncHost *AsyncHost
	if config.NativeAsync != nil {
		var err error
		asyncHost, err = asyncHostFor(asyncHostSource{dir: config.StoragePaths.ToolStorage, cfg: config.NativeAsync.Config}, asyncResources)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: tools.native.async: %v", config.SettingPath, err)}
		}
	}
	loaded, definitions, toolSmithNested, err := buildNativeTools(config, locks, taskStore, recordsStore, asyncHost, lazy)
	if err != nil {
		return nil, err
	}
	// transcript is read-only, so it is built before the subagent tool and an agent can list it.
	if config.NativeTranscript != nil {
		definition, err := requireNativeDefinition(config, definitions, "transcript", transcriptToolID, transcriptInputSchema)
		if err != nil {
			return nil, err
		}
		profiles, _ := messenger.(TranscriptProfiles)
		remotes, _, err := remoteDirectory(config, "transcript", config.NativeTranscript.Remotes, messenger)
		if err != nil {
			return nil, err
		}
		tool, err := NewTranscriptToolWithRemotes(*config.NativeTranscript, TranscriptFiles{Messages: config.StoragePaths.Messages, Backup: config.StoragePaths.MessagesBackup}, profiles, subagentManager, remotes)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, WithToolEventHooks(tool, config.Hooks, config.Model.APIKeyEnv))
	}
	if config.NativeTranscripts != nil {
		definition, err := requireNativeDefinition(config, definitions, "transcripts", transcriptsToolID, transcriptsInputSchema)
		if err != nil {
			return nil, err
		}
		profiles, _ := messenger.(TranscriptProfiles)
		sessions, _ := messenger.(TranscriptSessions)
		remotes, _, err := remoteDirectory(config, "transcripts", config.NativeTranscripts.Remotes, messenger)
		if err != nil {
			return nil, err
		}
		tool, err := NewTranscriptsToolWithRemotes(*config.NativeTranscripts, TranscriptFiles{Messages: config.StoragePaths.Messages, Backup: config.StoragePaths.MessagesBackup}, profiles, sessions, subagentManager, remotes)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, WithToolEventHooks(tool, config.Hooks, config.Model.APIKeyEnv))
	}
	if config.NativeProfile != nil {
		definition, err := requireNativeDefinition(config, definitions, "profile", profileToolID, profileInputSchema)
		if err != nil {
			return nil, err
		}
		profiles, _ := messenger.(TranscriptProfiles)
		actions, _ := messenger.(ProfileActions)
		remotes, remoteNames, err := remoteDirectory(config, "profile", config.NativeProfile.Remotes, messenger)
		if err != nil {
			return nil, err
		}
		tool, err := NewProfileToolWithRemotes(*config.NativeProfile, TranscriptFiles{Messages: config.StoragePaths.Messages, Backup: config.StoragePaths.MessagesBackup}, profiles, actions, remoteNames, remotes)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, WithToolEventHooks(tool, config.Hooks, config.Model.APIKeyEnv))
	}
	// profile_message is built before the subagent tool so an agent can list it, but it is
	// added to the catalog after the subagent tool, where it always was.
	var profileMessageTool *tools.Tool
	if config.NativeProfileMessage != nil && messenger != nil {
		definition, err := requireOptionalPropertiesDefinition(config, definitions, "profile_message", profileMessageToolID, profileMessageInputSchema, profileMessageModelProperty, profileMessageSandboxProperty)
		if err != nil {
			return nil, err
		}
		targets := sessionfs.ProfileMessageTargets(config.Profiles, config.Profile, config.NativeProfileMessage.DenyProfiles)
		remotes, remoteNames, err := remoteDirectory(config, "profile_message", config.NativeProfileMessage.Remotes, messenger)
		if err != nil {
			return nil, err
		}
		if len(targets) == 0 && len(remoteNames) == 0 && len(config.NativeProfileMessage.Sessions) == 0 {
			return nil, sessionfs.ProfileMessageNoTargetsError(config.SettingPath, config.Profile)
		}
		tool, err := NewProfileMessageToolWithRemotes(*config.NativeProfileMessage, targets, messenger, asyncHost, remotes)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s (profile %s): %v", config.SettingPath, config.Profile, err)}
		}
		tool.Definition.Description = definition.Description + profileMessageListText(targets, config.NativeProfileMessage.Sessions, remoteNames) + sandboxChoiceText(config.NativeProfileMessage.SandboxChoices)
		hooked := WithToolEventHooks(tool, config.Hooks, config.Model.APIKeyEnv)
		profileMessageTool = &hooked
	}
	// Last, because a child gets the tools that are already enabled here.
	if config.NativeSubagent != nil {
		definition, err := requireOptionalPropertiesDefinition(config, definitions, "subagent", subagentToolID, subagentInputSchema, subagentModelProperty, subagentSandboxProperty)
		if err != nil {
			return nil, err
		}
		subagentSettings := *config.NativeSubagent
		subagentSettings.DenyTools = config.ProfileTools.Deny
		agentToolsets, err := buildAgentToolsets(config, locks, toolsets, asyncHost)
		if err != nil {
			return nil, err
		}
		childPool := loaded
		if profileMessageTool != nil {
			childPool = append(append([]tools.Tool(nil), loaded...), *profileMessageTool)
		}
		subagentRemotes, _, err := remoteDirectory(config, "subagent", subagentSettings.Remotes, messenger)
		if err != nil {
			return nil, err
		}
		// A caller without MCP wiring still gets the nested hooks, so the handle is never nil here.
		if mcp == nil {
			mcp = NewSubagentMCP(config.Hooks, config.Model.APIKeyEnv)
		}
		tool, err := newSubagentToolDescribed(subagentSettings, childPool, sandboxBaseDescriptions(definitions), agentToolsets, launcher, mcp, subagentManager, asyncHost, subagentRemotes)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		// The agent list is part of the description the model sees, so it is kept on top
		// of the manifest's text and not replaced by it. The tool's own description is the code's
		// text plus the list, whose markers depend on which agents may run in the background.
		tool.Definition.Description = definition.Description + strings.TrimPrefix(tool.Definition.Description, subagentDescription)
		loaded = append(loaded, WithToolEventHooks(tool, config.Hooks, config.Model.APIKeyEnv))
	}
	if profileMessageTool != nil {
		loaded = append(loaded, *profileMessageTool)
	}
	// load_tool is last, after the subagent tool, so it is not in the child pool: a child gets it only
	// through SetLazy, from the owner, so it runs the owner's lazy tools. It lists the lazy tools bound in lazy, which is nil or
	// empty when the profile has none; it then lists nothing and runs nothing.
	if config.NativeLoadTool != nil {
		definition, err := requireNativeDefinition(config, definitions, "load_tool", loadToolID, loadToolInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewLoadToolTool(lazy, int(config.ProfileTools.LazyLoadMax))
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		tool = WithToolEventHooks(tool, config.Hooks, config.Model.APIKeyEnv)
		loaded = append(loaded, tool)
	}
	// The late tools were appended after buildNativeTools bound the early list, so the script bridge is bound
	// again with the whole list: the same gated, hooked copies the model sees. tool_smith itself is excluded
	// by name (NativeToolSmithExcludedTools), so the rebind cannot reach it and recurse.
	toolSmithNested.Bind(loaded, config.ProfileTools)
	return tools.NewCatalog(loaded)
}

// gateLazy returns the lazy gate of tool when the set has bound it and not yet gated it, and
// tool itself otherwise. The set binds only MCP and extension tools, so a native tool is returned as it is.
func gateLazy(tool tools.Tool, lazy *tools.LazySet) tools.Tool {
	if lazy == nil {
		return tool
	}
	gated, _ := lazy.Gate(tool)
	return gated
}

// GateLazyTools gates the lazy tools of a list with the set, before the event hooks wrap them.
// Each tool is gated at most once; a tool the set did not bind is returned unchanged.
func GateLazyTools(list []tools.Tool, lazy *tools.LazySet) []tools.Tool {
	gated := make([]tools.Tool, len(list))
	for index, tool := range list {
		gated[index] = gateLazy(tool, lazy)
	}
	return gated
}

// ToolsetResources gives the stateful stores of a toolset that a sub-agent names, so the
// tools built from it share their state with every other session that uses the same
// storage directory. A nil ToolsetResources makes each store private, which suits tests.
type ToolsetResources interface {
	Resources(config sessionfs.SessionConfig) (*TaskStore, *RecordsStore)
}

// buildAgentToolsets builds the native tools of every toolset a sub-agent names. The
// tools are not part of the catalog the parent's model sees.
func buildAgentToolsets(config sessionfs.SessionConfig, locks *PathLocks, resources ToolsetResources, asyncHost *AsyncHost) (map[string]subagentToolset, error) {
	if len(config.AgentToolsets) == 0 {
		return nil, nil
	}
	built := make(map[string]subagentToolset, len(config.AgentToolsets))
	for _, id := range slices.Sorted(maps.Keys(config.AgentToolsets)) {
		derived := config.AgentToolsets[id]
		// The toolset was loaded before the model file was read, so it has no model. Its
		// bash, exec and process hide the running model's key from a child's environment
		// the way the session's do.
		derived.Model = config.Model
		// A toolset shares the session's async host; without one it has no async tool.
		if asyncHost == nil {
			derived.NativeAsync = nil
		}
		var taskStore *TaskStore
		var recordsStore *RecordsStore
		if resources != nil {
			taskStore, recordsStore = resources.Resources(derived)
		}
		loaded, definitions, _, err := buildNativeTools(derived, locks, taskStore, recordsStore, asyncHost, nil)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("toolset %s: %v", id, err)}
		}
		built[id] = subagentToolset{native: loaded, descriptions: sandboxBaseDescriptions(definitions)}
	}
	return built, nil
}

// buildNativeTools builds every native tool the settings enable except the subagent
// tool, with the definitions it read.
func buildNativeTools(config sessionfs.SessionConfig, locks *PathLocks, taskStore *TaskStore, recordsStore *RecordsStore, asyncHost *AsyncHost, lazy *tools.LazySet) ([]tools.Tool, map[string]tools.Definition, *ToolSmithNested, error) {
	if config.NativeRecords != nil && recordsStore == nil {
		recordsStore = NewRecordsStore(config.NativeRecords.StorageDir, locks)
	}
	if len(config.ToolDefinitionPaths) == 0 {
		for _, name := range sessionfs.NativeToolNames() {
			if nativeToolConfigured(config, name) {
				return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: tools.native.%s is configured but native.%s is missing from tool_definition_paths", config.SettingPath, name, name)}
			}
		}
		for _, name := range sessionfs.ExtensionToolNames() {
			if extensionToolConfigured(config, name) {
				return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: tools.extension.%s is configured but extension.%s is missing from tool_definition_paths", config.SettingPath, name, name)}
			}
		}
	}
	definitions, err := loadDefinitions(config.ToolDefinitionPaths)
	if err != nil {
		return nil, nil, nil, err
	}
	var loaded []tools.Tool
	toolSmithNested := &ToolSmithNested{}
	if config.NativeExec != nil {
		definition, err := requireExecDefinition(config, definitions)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewExecTool(*config.NativeExec, config.Model.APIKeyEnv, asyncHost)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		tool.Definition.Description += sandboxDescription(config.NativeExec.Child.Sandbox)
		tool = withConfiguredHooks(config, WithShellChangeScan(tool, config.Hooks))
		loaded = append(loaded, tool)
	}
	if config.NativeLSP != nil {
		definition, err := requireNativeDefinition(config, definitions, "lsp", lspToolID, lspInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewLSPTool(*config.NativeLSP)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeReadImage != nil {
		definition, err := requireNativeDefinition(config, definitions, "read_image", readImageToolID, readImageInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewReadImageTool(*config.NativeReadImage, imagestore.FromSettings(config.ImageStore))
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeAsync != nil {
		definition, err := requireNativeDefinition(config, definitions, "async", asyncToolID, asyncInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewAsyncTool(asyncHost)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeTask != nil {
		definition, err := requireTaskDefinition(config, definitions)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewTaskTool(*config.NativeTask, taskStore)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeCodeContext != nil {
		definition, err := requireNativeDefinition(config, definitions, "code_context", codeContextToolID, codeContextInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewCodeContextTool(*config.NativeCodeContext)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeRecords != nil {
		definition, err := requireNativeDefinition(config, definitions, "records", recordsToolID, recordsInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewRecordsTool(*config.NativeRecords, recordsStore)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeRequestDraft != nil {
		definition, err := requireNativeDefinition(config, definitions, "request_draft", requestDraftToolID, requestDraftInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewRequestDraftTool(*config.NativeRequestDraft)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeSkill != nil {
		definition, err := requireNativeDefinition(config, definitions, "skill", skillToolID, skillInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewSkillTool(*config.NativeSkill, config.GlobalJadRoot)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeToolSmith != nil {
		definition, err := requireNativeDefinition(config, definitions, "tool_smith", toolSmithToolID, toolSmithInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewToolSmithTool(*config.NativeToolSmith, config.Model.APIKeyEnv, toolSmithNested, asyncHost)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		tool.Definition.Description += sandboxDescription(config.NativeToolSmith.Child.Sandbox)
		loaded = append(loaded, tool)
	}
	if config.ExtensionDecideSystemone != nil {
		definition, err := requireDefinition(config, definitions, "extension", "decide_systemone", extension.DecideSystemoneToolID, extension.DecideSystemoneInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := extension.NewDecideSystemoneTool(*config.ExtensionDecideSystemone)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.ExtensionWebSearch != nil {
		definition, err := requireDefinition(config, definitions, "extension", "web_search", extension.WebSearchToolID, extension.WebSearchInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := extension.NewWebSearchTool(*config.ExtensionWebSearch)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	// Each early tool is gated and then hooked once, here. Only an extension tool can be lazy (the set binds no
	// native tool). A child catalog is built from this gated list (childPool), so a child gets the owner's gate,
	// and a lazy tool is refused to it when named directly.
	if lazy != nil {
		lazy.Bind(loaded)
		for index, tool := range loaded {
			loaded[index] = gateLazy(tool, lazy)
		}
	}
	loaded = WithToolEventHooksAll(loaded, config.Hooks, config.Model.APIKeyEnv)
	toolSmithNested.Bind(loaded, config.ProfileTools)
	return loaded, definitions, toolSmithNested, nil
}

func nativeToolConfigured(config sessionfs.SessionConfig, name string) bool {
	switch name {
	case "lsp":
		return config.NativeLSP != nil
	case "exec":
		return config.NativeExec != nil
	case "read_image":
		return config.NativeReadImage != nil
	case "async":
		return config.NativeAsync != nil
	case "task":
		return config.NativeTask != nil
	case "code_context":
		return config.NativeCodeContext != nil
	case "records":
		return config.NativeRecords != nil
	case "request_draft":
		return config.NativeRequestDraft != nil
	case "skill":
		return config.NativeSkill != nil
	case "subagent":
		return config.NativeSubagent != nil
	case "profile_message":
		return config.NativeProfileMessage != nil
	case "transcript":
		return config.NativeTranscript != nil
	case "transcripts":
		return config.NativeTranscripts != nil
	case "profile":
		return config.NativeProfile != nil
	case "tool_smith":
		return config.NativeToolSmith != nil
	case "load_tool":
		return config.NativeLoadTool != nil
	default:
		return false
	}
}

// extensionToolConfigured reports whether tools.extension configures the named tool.
func extensionToolConfigured(config sessionfs.SessionConfig, name string) bool {
	switch name {
	case "decide_systemone":
		return config.ExtensionDecideSystemone != nil
	case "web_search":
		return config.ExtensionWebSearch != nil
	default:
		return false
	}
}

func requireNativeDefinition(config sessionfs.SessionConfig, definitions map[string]tools.Definition, name, id string, schema json.RawMessage) (tools.Definition, error) {
	return requireDefinition(config, definitions, "native", name, id, schema)
}

// requireExecDefinition is requireNativeDefinition for exec. A definition file may also hold
// the schema exec had before read_only, or one with an earlier read_only text (before it said
// a session may ignore it, or while tools.native.exec.read_only_calls existed); the model is then shown the current one, so a file copied from an older
// version still loads and read_only is offered and described all the same.
func requireExecDefinition(config sessionfs.SessionConfig, definitions map[string]tools.Definition) (tools.Definition, error) {
	if definition, exists := definitions[execToolID]; exists && definition.Name == "exec" && definition.Source == "native" {
		for _, older := range []json.RawMessage{execInputSchemaBeforeReadOnly, execInputSchemaPromiseOnly, execInputSchemaIgnorable} {
			if matches, err := sameJSON(definition.InputSchema, older); err == nil && matches {
				definition.InputSchema = append(json.RawMessage(nil), execInputSchema...)
				return definition, nil
			}
		}
	}
	return requireNativeDefinition(config, definitions, "exec", execToolID, execInputSchema)
}

// requireTaskDefinition is requireNativeDefinition for task. A definition file may also hold
// the schema task had before ops (taskInputSchemaV1); the model is then shown the current one,
// and the current description when the file still has the old one, so a file copied from an
// older version still loads and ops, offset, q and detail are offered all the same.
func requireTaskDefinition(config sessionfs.SessionConfig, definitions map[string]tools.Definition) (tools.Definition, error) {
	if definition, exists := definitions[taskToolID]; exists && definition.Name == "task" && definition.Source == "native" {
		if matches, err := sameJSON(definition.InputSchema, taskInputSchemaV1); err == nil && matches {
			definition.InputSchema = append(json.RawMessage(nil), taskInputSchema...)
			if definition.Description == taskDescriptionV1 {
				definition.Description = taskDescription
			}
			return definition, nil
		}
	}
	return requireNativeDefinition(config, definitions, "task", taskToolID, taskInputSchema)
}

// requireOptionalPropertiesDefinition is requireNativeDefinition for a tool with optional
// properties (model, then sandbox; optionalPropertiesSchema): the definition file may hold the
// full schema, the schema without sandbox, or the schema without sandbox and model (the one
// the tool had before either existed), so a file copied from an older version still loads.
// The tool builds the schema the model sees itself, so the file's copy is only checked.
func requireOptionalPropertiesDefinition(config sessionfs.SessionConfig, definitions map[string]tools.Definition, name, id string, schema json.RawMessage, model, sandbox string) (tools.Definition, error) {
	if definition, exists := definitions[id]; exists && definition.Name == name && definition.Source == "native" {
		older := [][]optionalProperty{{{model, true}, {sandbox, false}}, {{model, false}, {sandbox, false}}}
		for _, properties := range older {
			if matches, err := sameJSON(definition.InputSchema, optionalPropertiesSchema(schema, properties)); err == nil && matches {
				return definition, nil
			}
		}
	}
	return requireNativeDefinition(config, definitions, name, id, schema)
}

// requireDefinition checks that the tool definition files define tool name with this id,
// source and input schema. source is also the settings node the tool is configured under.
func requireDefinition(config sessionfs.SessionConfig, definitions map[string]tools.Definition, source, name, id string, schema json.RawMessage) (tools.Definition, error) {
	definition, exists := definitions[id]
	if !exists {
		return tools.Definition{}, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: tools.%s.%s is configured but %s is missing from tool_definition_paths", config.SettingPath, source, name, id)}
	}
	if definition.Name != name || definition.Source != source {
		return tools.Definition{}, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %s must define the %s %s tool", config.SettingPath, id, source, name)}
	}
	matches, err := sameJSON(definition.InputSchema, schema)
	if err != nil {
		return tools.Definition{}, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: invalid %s %s inputSchema: %v", config.SettingPath, source, name, err)}
	}
	if !matches {
		// The usual cause is a definition file copied from an older version: the schema
		// changed with the binary, so the message names the file to copy again.
		return tools.Definition{}, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %s inputSchema must exactly match the %s tool schema; a definition file copied from an older version must be replaced with sample/tools/%s/%s.json of this version", config.SettingPath, id, source, source, name)}
	}
	return definition, nil
}

func loadDefinitions(paths []string) (map[string]tools.Definition, error) {
	validator, err := tools.CompileSchema(definitionDocumentSchema)
	if err != nil {
		return nil, fmt.Errorf("compile tool definition document schema: %w", err)
	}
	definitions := make(map[string]tools.Definition)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: could not read or parse tool definitions: %v", path, err)}
		}
		issues, err := validator.Validate(data)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: invalid tool definition document: %v", path, err)}
		}
		if len(issues) > 0 {
			encoded, _ := json.Marshal(issues)
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: invalid tool definition document: %s", path, encoded)}
		}
		var document struct {
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: could not decode tool definitions: %v", path, err)}
		}
		for _, rawTool := range document.Tools {
			var definition tools.Definition
			if err := json.Unmarshal(rawTool, &definition); err != nil {
				return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: could not decode tool definition: %v", path, err)}
			}
			if _, exists := definitions[definition.ID]; exists {
				return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: duplicate tool definition %s", path, definition.ID)}
			}
			definitions[definition.ID] = definition
		}
	}
	return definitions, nil
}

func sameJSON(left, right json.RawMessage) (bool, error) {
	leftValue, err := jsonschema.UnmarshalJSON(bytes.NewReader(left))
	if err != nil {
		return false, err
	}
	rightValue, err := jsonschema.UnmarshalJSON(bytes.NewReader(right))
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(leftValue, rightValue), nil
}

// RemoteDirectories is implemented by a messenger that can reach other JAD servers: it gives
// the hub's view of a remotes folder.
type RemoteDirectories interface {
	RemoteDirectory(dir string) *remote.Directory
}

// remoteDirectory resolves tools.native.profile_message.remotes: every name must have a valid
// file in the session's remotes folder, so a typo is a configuration error, not a failed call.
func remoteDirectory(config sessionfs.SessionConfig, tool string, names []string, messenger ProfileMessenger) (*remote.Directory, []string, error) {
	if len(names) == 0 {
		return nil, nil, nil
	}
	provider, ok := messenger.(RemoteDirectories)
	if !ok || config.RemotesDir == "" {
		return nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: tools.native.%s.remotes needs a catalog-layout session with a remotes folder", config.SettingPath, tool)}
	}
	directory := provider.RemoteDirectory(config.RemotesDir)
	available, _ := remote.List(config.RemotesDir)
	have := make([]string, 0, len(available))
	for _, entry := range available {
		have = append(have, entry.Name)
	}
	for _, name := range names {
		if _, err := directory.Entry(name); err != nil {
			return nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: tools.native.%s.remotes names remote %q: %v (remotes in %s: %s)", config.SettingPath, tool, name, remoteProblem(name, err), config.RemotesDir, strings.Join(have, ", "))}
		}
	}
	return directory, names, nil
}
