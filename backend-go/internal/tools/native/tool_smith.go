package native

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const (
	toolSmithToolID     = "native.tool_smith"
	toolSmithInputEnv   = "TOOL_SMITH_INPUT"
	toolSmithRunExample = `{"run":{"function":"return 1;"}}`
)

//go:embed tool_smith_prelude.ts
var toolSmithPrelude string

var toolSmithInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "list":{
      "type":"object",
      "properties":{
        "filters":{
          "type":"array",
          "items":{"type":"string"},
          "description":"Case-insensitive substrings matched on function name and description."
        }
      },
      "additionalProperties":false
    },
    "run":{
      "type":"object",
      "properties":{
        "function":{"type":"string","description":"TypeScript async function body."},
        "input":{"type":"object","additionalProperties":true,"description":"JSON data bound to the input variable."}
      },
      "required":["function"],
      "additionalProperties":false
    }
  },
  "additionalProperties":false
}`)

const toolSmithDescription = "Run a TypeScript function body in a child process and return one value, or list the saved functions of the library. Saved functions are reusable files called as fn.<name>(input) inside a run."

// NewToolSmithTool creates the tool_smith tool. It is ReadOnly so several calls may
// share a response, and a call with run says so through MayChangeFiles.
func NewToolSmithTool(settings sessionfs.NativeToolSmithSettings, apiKeyEnv string, nested *ToolSmithNested) (tools.Tool, error) {
	validator, err := tools.CompileSchema(toolSmithInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native tool_smith schema: %w", err)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          toolSmithToolID,
			Source:      "native",
			Name:        "tool_smith",
			Description: toolSmithDescription,
			InputSchema: append(json.RawMessage(nil), toolSmithInputSchema...),
		},
		Validator:          validator,
		ReadOnly:           true,
		ExampleOperation:   json.RawMessage(toolSmithRunExample),
		NormalizeArguments: normalizeToolSmithArguments,
		MayChangeFiles: func(arguments map[string]any) bool {
			_, hasRun := arguments["run"]
			return hasRun
		},
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeToolSmith(ctx, arguments, settings, apiKeyEnv, nested, execution), nil
		},
	}, nil
}

// normalizeToolSmithArguments accepts run as the function body itself and the names
// script, code and body for function.
func normalizeToolSmithArguments(arguments map[string]any) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	switch run := arguments["run"].(type) {
	case string:
		normalized["run"] = map[string]any{"function": run}
	case map[string]any:
		if _, has := run["function"]; has {
			break
		}
		body, ok := firstString(run, "script", "code", "body")
		if !ok {
			break
		}
		fixed := make(map[string]any, len(run))
		for key, value := range run {
			if key != "script" && key != "code" && key != "body" {
				fixed[key] = value
			}
		}
		fixed["function"] = body
		normalized["run"] = fixed
	}
	return normalized
}

// requirePrivateDirectory fails unless dir is a directory only its owner can enter (mode 0700).
func requirePrivateDirectory(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s has mode %v, want 0700", dir, info.Mode().Perm())
	}
	return nil
}

func executeToolSmith(
	ctx context.Context,
	arguments map[string]any,
	settings sessionfs.NativeToolSmithSettings,
	apiKeyEnv string,
	nested *ToolSmithNested,
	execution tools.ExecutionContext,
) string {
	callID := execution.CallID
	_, hasList := arguments["list"]
	if _, hasRun := arguments["run"]; !hasRun {
		if hasList {
			return executeToolSmithList(arguments, settings, execution)
		}
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input", "Give run or list",
			tools.Information{Key: "suggestion", Value: "example: " + toolSmithRunExample})
	}
	run, _ := arguments["run"].(map[string]any)
	body, _ := run["function"].(string)
	if strings.TrimSpace(body) == "" {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input", "run.function is empty",
			tools.Information{Key: "suggestion", Value: "example: " + toolSmithRunExample})
	}
	input := map[string]any{}
	if given, present := run["input"]; present && given != nil {
		object, ok := given.(map[string]any)
		if !ok {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input", "run.input must be an object")
		}
		input = object
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input", "run.input is not valid JSON data",
			tools.Information{Key: "system_error", Value: err.Error()})
	}
	if size := int64(len(body) + len(inputJSON)); size > settings.CodeMaxBytes {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input",
			fmt.Sprintf("function and input are %d bytes, over the limit of %d", size, settings.CodeMaxBytes),
			tools.Information{Key: "suggestion", Value: "save the code as a library function and call it, or shorten the input"})
	}

	workDir, err := os.MkdirTemp("", "tool-smith-")
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The working files could not be created",
			tools.Information{Key: "system_error", Value: err.Error()})
	}
	defer os.RemoveAll(workDir)
	if err := requirePrivateDirectory(workDir); err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The working directory is not private",
			tools.Information{Key: "system_error", Value: err.Error()})
	}
	wrapperPath := filepath.Join(workDir, "wrapper.ts")
	inputPath := filepath.Join(workDir, "input.json")
	resultPath := filepath.Join(workDir, "result.txt")
	wrapper := "import { deliver, tools, fn } from \"./prelude.ts\";\nawait deliver(async (input) => {\n" + body + "\n});\n"
	for path, content := range map[string]string{
		wrapperPath:                          wrapper,
		filepath.Join(workDir, "prelude.ts"): toolSmithPrelude,
		inputPath:                            string(inputJSON),
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The script files could not be written",
				tools.Information{Key: "system_error", Value: err.Error()})
		}
	}

	if err := stageToolSmithFunctions(workDir, settings, execution.WorkspaceRoot); err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The saved functions could not be staged",
			tools.Information{Key: "system_error", Value: err.Error()})
	}

	argv := make([]string, len(settings.Interpreter))
	for index, item := range settings.Interpreter {
		argv[index] = strings.ReplaceAll(item, "{file}", wrapperPath)
	}
	extraEnv := map[string]string{execResultEnv: resultPath, toolSmithInputEnv: inputPath,
		toolSmithFunctionsEnv: filepath.Join(workDir, "fn-manifest.json")}
	var bridge *toolSmithBridge
	if allowed := nested.allowed(settings); len(allowed) > 0 {
		bridge, err = startToolSmithBridge(ctx, workDir, settings, allowed, execution)
		if err != nil {
			return tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The tool bridge could not be started",
				tools.Information{Key: "system_error", Value: err.Error()})
		}
		defer bridge.Close()
		extraEnv[toolSmithSocketEnv] = bridge.socketPath
		extraEnv[toolSmithTokenEnv] = bridge.token
	}
	result := runShellChild(ctx, shellChildRun{
		file:       argv[0],
		argv:       argv[1:],
		cwd:        ".",
		section:    "tools.native.tool_smith",
		extraEnv:   extraEnv,
		resultFile: &shellResultFile{path: resultPath, maxBytes: settings.ResultMaxBytes, cutMarker: true},
		program:    &shellProgram{name: filepath.Base(argv[0]), setting: "tools.native.tool_smith.interpreter"},
	}, settings.Child, apiKeyEnv, execution)
	var lines []string
	if bridge != nil {
		lines = bridge.Close().informationLines()
	}
	if hasList {
		lines = append(lines, "ignored: list")
	}
	return appendToolSmithInformation(result, lines)
}
