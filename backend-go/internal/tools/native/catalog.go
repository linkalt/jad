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

	"jad/backend-go/internal/imageresize"
	"jad/backend-go/internal/imagestore"
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
	return BuildCatalogWithProcessManager(config, locks, nil)
}

// BuildCatalogWithProcessManager allows a Resolver to share process state across
// independently resolved sessions with the same configured process storage.
func BuildCatalogWithProcessManager(config sessionfs.SessionConfig, locks *PathLocks, manager ProcessManager) (*tools.Catalog, error) {
	return BuildCatalogWithResources(config, locks, manager, nil, nil, nil)
}

// BuildCatalogWithResources shares resolver-owned stateful stores across tool
// catalogs created for the same configured storage directory.
func BuildCatalogWithResources(config sessionfs.SessionConfig, locks *PathLocks, manager ProcessManager, taskStore *TaskStore, memoryStore *MemoryStore, fetchStore *FetchStore) (*tools.Catalog, error) {
	return BuildCatalogWithSubagents(config, locks, manager, taskStore, memoryStore, fetchStore, nil, nil, nil, nil)
}

// BuildCatalogWithSubagents is BuildCatalogWithResources plus the launcher that runs
// the children of the subagent tool. A nil launcher still lists the tool, and a call
// to it answers that sub-agent runs are unavailable; listing tools needs no launcher.
func BuildCatalogWithSubagents(config sessionfs.SessionConfig, locks *PathLocks, manager ProcessManager, taskStore *TaskStore, memoryStore *MemoryStore, fetchStore *FetchStore, launcher SubagentLauncher, mcp *SubagentMCP, subagentManager *SubagentManager, toolsets ToolsetResources) (*tools.Catalog, error) {
	return BuildCatalogWithMessenger(config, locks, manager, taskStore, memoryStore, fetchStore, launcher, mcp, subagentManager, toolsets, nil)
}

// BuildCatalogWithMessenger is BuildCatalogWithSubagents plus the messenger behind the
// profile_message tool. Without a messenger the tool is not offered.
func BuildCatalogWithMessenger(config sessionfs.SessionConfig, locks *PathLocks, manager ProcessManager, taskStore *TaskStore, memoryStore *MemoryStore, fetchStore *FetchStore, launcher SubagentLauncher, mcp *SubagentMCP, subagentManager *SubagentManager, toolsets ToolsetResources, messenger ProfileMessenger) (*tools.Catalog, error) {
	return BuildCatalogWithAsync(config, locks, manager, taskStore, memoryStore, fetchStore, launcher, mcp, subagentManager, toolsets, messenger, nil)
}

// BuildCatalogWithAsync is BuildCatalogWithMessenger plus the resolver that shares one
// async host per tool storage folder. Without it the async tool opens a host of its own.
func BuildCatalogWithAsync(config sessionfs.SessionConfig, locks *PathLocks, manager ProcessManager, taskStore *TaskStore, memoryStore *MemoryStore, fetchStore *FetchStore, launcher SubagentLauncher, mcp *SubagentMCP, subagentManager *SubagentManager, toolsets ToolsetResources, messenger ProfileMessenger, asyncResources AsyncResources) (*tools.Catalog, error) {
	if config.NativeRead == nil && config.NativeWrite == nil && config.NativeEdit == nil && config.NativeGrep == nil && config.NativeBash == nil && config.NativeExec == nil && config.NativeGlob == nil && config.NativeReadImage == nil && config.NativeProcess == nil && config.NativeTask == nil && config.NativeMemory == nil && config.NativeFetch == nil && config.NativeRequestDraft == nil && config.NativeSkill == nil && config.ExtensionDecide == nil && config.NativeSubagent == nil && config.NativeProfileMessage == nil && config.NativeToolSmith == nil && config.NativeAsync == nil {
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
	loaded, definitions, manager, err := buildNativeTools(config, locks, manager, taskStore, memoryStore, fetchStore, asyncHost)
	if err != nil {
		return nil, err
	}
	// profile_message is built before the subagent tool so an agent can list it, but it is
	// added to the catalog after the subagent tool, where it always was.
	var profileMessageTool *tools.Tool
	if config.NativeProfileMessage != nil && messenger != nil {
		definition, err := requireNativeDefinition(config, definitions, "profile_message", profileMessageToolID, profileMessageInputSchema)
		if err != nil {
			return nil, err
		}
		targets := sessionfs.ProfileMessageTargets(config.Profiles, config.Profile, config.NativeProfileMessage.DenyProfiles)
		if len(targets) == 0 {
			return nil, sessionfs.ProfileMessageNoTargetsError(config.SettingPath, config.Profile)
		}
		tool, err := NewProfileMessageTool(*config.NativeProfileMessage, targets, messenger, asyncHost)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s (profile %s): %v", config.SettingPath, config.Profile, err)}
		}
		tool.Definition.Description = definition.Description + profileMessageListText(targets)
		wrapped := WithToolEventHooks(tool, config.Hooks, config.Model.APIKeyEnv)
		profileMessageTool = &wrapped
	}
	// Last, because a child gets the read-only tools that are already enabled here.
	if config.NativeSubagent != nil {
		definition, err := requireNativeDefinition(config, definitions, "subagent", subagentToolID, subagentInputSchema)
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
		tool, err := newSubagentToolAsync(subagentSettings, childPool, agentToolsets, launcher, mcp, subagentManager, manager, asyncHost)
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
	return tools.NewCatalog(loaded)
}

// ToolsetResources gives the stateful stores of a toolset that a sub-agent names, so the
// tools built from it share their state with every other session that uses the same
// storage directory. A nil ToolsetResources makes each store private, which suits tests.
type ToolsetResources interface {
	Resources(config sessionfs.SessionConfig) (ProcessManager, *TaskStore, *MemoryStore, *FetchStore)
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
		var manager ProcessManager
		var taskStore *TaskStore
		var memoryStore *MemoryStore
		var fetchStore *FetchStore
		if resources != nil {
			manager, taskStore, memoryStore, fetchStore = resources.Resources(derived)
		}
		loaded, _, manager, err := buildNativeTools(derived, locks, manager, taskStore, memoryStore, fetchStore, asyncHost)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("toolset %s: %v", id, err)}
		}
		built[id] = subagentToolset{native: loaded, processes: manager}
	}
	return built, nil
}

// buildNativeTools builds every native tool the settings enable except the subagent
// tool, with the definitions it read and the process manager it used.
func buildNativeTools(config sessionfs.SessionConfig, locks *PathLocks, manager ProcessManager, taskStore *TaskStore, memoryStore *MemoryStore, fetchStore *FetchStore, asyncHost *AsyncHost) ([]tools.Tool, map[string]tools.Definition, ProcessManager, error) {
	if config.NativeProcess != nil && manager == nil {
		manager = NewConfiguredProcessManager(*config.NativeProcess)
	}
	if config.NativeMemory != nil && memoryStore == nil {
		memoryStore = NewMemoryStore(config.NativeMemory.StorageDir, locks)
	}
	if config.NativeFetch != nil && fetchStore == nil {
		fetchStore = NewFetchStore(config.NativeFetch.StorageDir)
	}
	if len(config.ToolDefinitionPaths) == 0 {
		for _, name := range sessionfs.NativeToolNames() {
			if nativeToolConfigured(config, name) {
				return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: tools.native.%s is configured but native.%s is missing from tool_definition_paths", config.SettingPath, name, name)}
			}
		}
		if config.ExtensionDecide != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: tools.extension.decide is configured but extension.decide is missing from tool_definition_paths", config.SettingPath)}
		}
	}
	definitions, err := loadDefinitions(config.ToolDefinitionPaths)
	if err != nil {
		return nil, nil, nil, err
	}
	var loaded []tools.Tool
	toolSmithNested := &ToolSmithNested{}
	if config.NativeRead != nil {
		definition, err := requireNativeDefinition(config, definitions, "read", readToolID, readInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewReadTool(*config.NativeRead)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeWrite != nil {
		definition, err := requireNativeDefinition(config, definitions, "write", writeToolID, writeInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewWriteTool(*config.NativeWrite, locks)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		tool = withConfiguredHooks(config, tool)
		loaded = append(loaded, tool)
	}
	if config.NativeEdit != nil {
		definition, err := requireNativeDefinition(config, definitions, "edit", editToolID, editInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewEditTool(*config.NativeEdit, locks)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		tool = withConfiguredHooks(config, tool)
		loaded = append(loaded, tool)
	}
	if config.NativeGrep != nil {
		definition, err := requireNativeDefinition(config, definitions, "grep", grepToolID, grepInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewGrepTool(*config.NativeGrep)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeBash != nil {
		definition, err := requireNativeDefinition(config, definitions, "bash", bashToolID, bashInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewBashTool(*config.NativeBash, config.Model.APIKeyEnv)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeExec != nil {
		definition, err := requireNativeDefinition(config, definitions, "exec", execToolID, execInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewExecTool(*config.NativeExec, config.Model.APIKeyEnv, asyncHost)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeGlob != nil {
		definition, err := requireNativeDefinition(config, definitions, "glob", globToolID, globInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewGlobTool(*config.NativeGlob)
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
	if config.NativeProcess != nil {
		definition, err := requireNativeDefinition(config, definitions, "process", processToolID, processInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewProcessToolWithAsync(*config.NativeProcess, manager, config.Model.APIKeyEnv, asyncHost)
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
		definition, err := requireNativeDefinition(config, definitions, "task", taskToolID, taskInputSchema)
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
	if config.NativeMemory != nil {
		definition, err := requireNativeDefinition(config, definitions, "memory", memoryToolID, memoryInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewMemoryTool(*config.NativeMemory, memoryStore)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeFetch != nil {
		definition, err := requireNativeDefinition(config, definitions, "fetch", fetchToolID, fetchInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		tool, err := NewFetchTool(*config.NativeFetch, fetchStore)
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
		loaded = append(loaded, tool)
	}
	if config.ExtensionDecide != nil {
		definition, err := requireDefinition(config, definitions, "extension", "decide", extension.DecideToolID, extension.DecideInputSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		images := extension.DecideImages{Store: imagestore.FromSettings(config.ImageStore), Resizer: imageresize.FromSettings(config.ImageResize), ReadFile: readImageFile}
		tool, err := extension.NewDecideTool(*config.ExtensionDecide, images)
		if err != nil {
			return nil, nil, nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	loaded = WithToolEventHooksAll(loaded, config.Hooks, config.Model.APIKeyEnv)
	toolSmithNested.Bind(loaded, config.ProfileTools.Deny)
	return loaded, definitions, manager, nil
}

func nativeToolConfigured(config sessionfs.SessionConfig, name string) bool {
	switch name {
	case "read":
		return config.NativeRead != nil
	case "write":
		return config.NativeWrite != nil
	case "edit":
		return config.NativeEdit != nil
	case "grep":
		return config.NativeGrep != nil
	case "bash":
		return config.NativeBash != nil
	case "exec":
		return config.NativeExec != nil
	case "glob":
		return config.NativeGlob != nil
	case "read_image":
		return config.NativeReadImage != nil
	case "process":
		return config.NativeProcess != nil
	case "async":
		return config.NativeAsync != nil
	case "task":
		return config.NativeTask != nil
	case "memory":
		return config.NativeMemory != nil
	case "fetch":
		return config.NativeFetch != nil
	case "request_draft":
		return config.NativeRequestDraft != nil
	case "skill":
		return config.NativeSkill != nil
	case "subagent":
		return config.NativeSubagent != nil
	case "profile_message":
		return config.NativeProfileMessage != nil
	case "tool_smith":
		return config.NativeToolSmith != nil
	default:
		return false
	}
}

func requireNativeDefinition(config sessionfs.SessionConfig, definitions map[string]tools.Definition, name, id string, schema json.RawMessage) (tools.Definition, error) {
	return requireDefinition(config, definitions, "native", name, id, schema)
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
		return tools.Definition{}, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %s inputSchema must exactly match the %s tool schema", config.SettingPath, id, source)}
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
