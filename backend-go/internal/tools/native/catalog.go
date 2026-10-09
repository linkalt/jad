package native

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"example.com/agent-harness/backend-go/internal/imageresize"
	"example.com/agent-harness/backend-go/internal/imagestore"
	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
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
          "source":{"type":"string","enum":["native","mcp"]},
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
	return BuildCatalogWithSubagents(config, locks, manager, taskStore, memoryStore, fetchStore, nil, nil, nil)
}

// BuildCatalogWithSubagents is BuildCatalogWithResources plus the launcher that runs
// the children of the subagent tool. A nil launcher still lists the tool, and a call
// to it answers that sub-agent runs are unavailable; listing tools needs no launcher.
func BuildCatalogWithSubagents(config sessionfs.SessionConfig, locks *PathLocks, manager ProcessManager, taskStore *TaskStore, memoryStore *MemoryStore, fetchStore *FetchStore, launcher SubagentLauncher, mcp *SubagentMCP, subagentManager *SubagentManager) (*tools.Catalog, error) {
	if config.NativeRead == nil && config.NativeWrite == nil && config.NativeEdit == nil && config.NativeGrep == nil && config.NativeBash == nil && config.NativeExec == nil && config.NativeGlob == nil && config.NativeProcess == nil && config.NativeTask == nil && config.NativeMemory == nil && config.NativeFetch == nil && config.NativeRequestDraft == nil && config.NativeSkill == nil && config.NativeDecide == nil && config.NativeSubagent == nil {
		return tools.NewCatalog(nil)
	}
	if locks == nil {
		locks = NewPathLocks()
	}
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
		for _, enabled := range []struct{ name, id string }{
			{"read", readToolID}, {"write", writeToolID}, {"edit", editToolID}, {"grep", grepToolID}, {"bash", bashToolID}, {"exec", execToolID}, {"glob", globToolID}, {"process", processToolID}, {"task", taskToolID}, {"memory", memoryToolID}, {"fetch", fetchToolID}, {"request_draft", requestDraftToolID}, {"skill", skillToolID}, {"decide", decideToolID}, {"subagent", subagentToolID},
		} {
			if nativeToolConfigured(config, enabled.name) {
				return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: tools.native.%s is configured but %s is missing from tool_definition_paths", config.SettingPath, enabled.name, enabled.id)}
			}
		}
	}
	definitions, err := loadDefinitions(config.ToolDefinitionPaths)
	if err != nil {
		return nil, err
	}
	var loaded []tools.Tool
	if config.NativeRead != nil {
		definition, err := requireNativeDefinition(config, definitions, "read", readToolID, readInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewReadTool(*config.NativeRead)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeWrite != nil {
		definition, err := requireNativeDefinition(config, definitions, "write", writeToolID, writeInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewWriteTool(*config.NativeWrite, locks)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		tool.Approval = approvalMode(config.NativeWrite.Approval)
		tool, err = withConfiguredHooks(config, tool)
		if err != nil {
			return nil, err
		}
		loaded = append(loaded, tool)
	}
	if config.NativeEdit != nil {
		definition, err := requireNativeDefinition(config, definitions, "edit", editToolID, editInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewEditTool(*config.NativeEdit, locks)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		tool.Approval = approvalMode(config.NativeEdit.Approval)
		tool, err = withConfiguredHooks(config, tool)
		if err != nil {
			return nil, err
		}
		loaded = append(loaded, tool)
	}
	if config.NativeGrep != nil {
		definition, err := requireNativeDefinition(config, definitions, "grep", grepToolID, grepInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewGrepTool(*config.NativeGrep)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeBash != nil {
		definition, err := requireNativeDefinition(config, definitions, "bash", bashToolID, bashInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewBashTool(*config.NativeBash, config.Model.APIKeyEnv)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		tool.Approval = approvalMode(config.NativeBash.Approval)
		loaded = append(loaded, tool)
	}
	if config.NativeExec != nil {
		definition, err := requireNativeDefinition(config, definitions, "exec", execToolID, execInputSchema)
		if err != nil {
			return nil, err
		}
		if config.NativeBash == nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: tools.native.exec needs tools.native.bash, whose child-process settings it uses", config.SettingPath)}
		}
		tool, err := NewExecTool(*config.NativeExec, *config.NativeBash, config.Model.APIKeyEnv)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		tool.Approval = approvalMode(config.NativeExec.Approval)
		loaded = append(loaded, tool)
	}
	if config.NativeGlob != nil {
		definition, err := requireNativeDefinition(config, definitions, "glob", globToolID, globInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewGlobTool(*config.NativeGlob)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeProcess != nil {
		definition, err := requireNativeDefinition(config, definitions, "process", processToolID, processInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewProcessTool(*config.NativeProcess, manager, config.Model.APIKeyEnv)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		tool.Approval = approvalMode(config.NativeProcess.Approval)
		loaded = append(loaded, tool)
	}
	if config.NativeTask != nil {
		definition, err := requireNativeDefinition(config, definitions, "task", taskToolID, taskInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewTaskTool(*config.NativeTask, taskStore)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeMemory != nil {
		definition, err := requireNativeDefinition(config, definitions, "memory", memoryToolID, memoryInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewMemoryTool(*config.NativeMemory, memoryStore)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeFetch != nil {
		definition, err := requireNativeDefinition(config, definitions, "fetch", fetchToolID, fetchInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewFetchTool(*config.NativeFetch, fetchStore)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeRequestDraft != nil {
		definition, err := requireNativeDefinition(config, definitions, "request_draft", requestDraftToolID, requestDraftInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewRequestDraftTool(*config.NativeRequestDraft)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeSkill != nil {
		definition, err := requireNativeDefinition(config, definitions, "skill", skillToolID, skillInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewSkillTool(*config.NativeSkill, config.GlobalJadRoot)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	if config.NativeDecide != nil {
		definition, err := requireNativeDefinition(config, definitions, "decide", decideToolID, decideInputSchema)
		if err != nil {
			return nil, err
		}
		images := DecideImages{Store: imagestore.FromSettings(config.ImageStore), Resizer: imageresize.FromSettings(config.ImageResize)}
		if config.NativeRead != nil {
			images.ReadPolicy = config.NativeRead.Policy
		}
		tool, err := NewDecideTool(*config.NativeDecide, images)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		tool.Definition = definition
		loaded = append(loaded, tool)
	}
	// Last, because a child gets the read-only tools that are already enabled here.
	if config.NativeSubagent != nil {
		definition, err := requireNativeDefinition(config, definitions, "subagent", subagentToolID, subagentInputSchema)
		if err != nil {
			return nil, err
		}
		tool, err := NewSubagentToolWithProcesses(*config.NativeSubagent, loaded, launcher, mcp, subagentManager, manager)
		if err != nil {
			return nil, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
		}
		// The agent list is part of the description the model sees, so it is kept on top
		// of the manifest's text and not replaced by it.
		tool.Definition.Description = definition.Description + subagentAgentList(config.NativeSubagent.Agents, config.NativeSubagent.ListMaxBytes)
		loaded = append(loaded, tool)
	}
	return tools.NewCatalog(loaded)
}

// approvalMode maps a tool's normalized approval setting to the catalog's field:
// only "ask" holds a call, so every other value leaves the tool ungated.
func approvalMode(configured string) string {
	if configured == sessionfs.ApprovalAsk {
		return tools.ApprovalAsk
	}
	return ""
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
	case "process":
		return config.NativeProcess != nil
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
	case "decide":
		return config.NativeDecide != nil
	case "subagent":
		return config.NativeSubagent != nil
	default:
		return false
	}
}

func requireNativeDefinition(config sessionfs.SessionConfig, definitions map[string]tools.Definition, name, id string, schema json.RawMessage) (tools.Definition, error) {
	definition, exists := definitions[id]
	if !exists {
		return tools.Definition{}, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: tools.native.%s is configured but %s is missing from tool_definition_paths", config.SettingPath, name, id)}
	}
	if definition.Name != name || definition.Source != "native" {
		return tools.Definition{}, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %s must define the native %s tool", config.SettingPath, id, name)}
	}
	matches, err := sameJSON(definition.InputSchema, schema)
	if err != nil {
		return tools.Definition{}, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: invalid native %s inputSchema: %v", config.SettingPath, name, err)}
	}
	if !matches {
		return tools.Definition{}, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %s inputSchema must exactly match the native tool schema", config.SettingPath, id)}
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
