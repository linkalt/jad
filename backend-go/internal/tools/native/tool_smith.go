package native

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const (
	toolSmithToolID     = "native.tool_smith"
	toolSmithInputEnv   = "TOOL_SMITH_INPUT"
	toolSmithRunExample = `{"code":"console.log(1);"}`
)

// toolSmithResultEnv is the name the prelude writes a returned value under; it predates exec's JAD_RESULT.
const toolSmithResultEnv = "EXEC_RESULT"

//go:embed tool_smith_prelude.ts
var toolSmithPrelude string

var toolSmithInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "filters":{
      "type":"array",
      "items":{"type":"string"},
      "description":"Only with no code: list the saved functions whose name or description matches every filter, case-insensitive. A filter with * or ? is a wildcard pattern (read*, *file*); any other is a substring."
    },
    "code":{"type":"string","description":"TypeScript async function body to run. Write output with console.log; return is optional."},
    "input":{"type":"object","additionalProperties":true,"description":"JSON data bound to the input variable of the function body."},
    "block_ms":{"type":"integer","minimum":0,"description":"Most time to block for the result. If the script is still running an id is returned; collect it with the async tool."}
  },
  "additionalProperties":false
}`)

const toolSmithDescription = "Call with no parameters (or only filters, e.g. read*) to list the saved functions of the library. Give code to execute a TypeScript function body in a child process, waiting up to block_ms. Write output with console.log: stdout is the result of the script. return is optional and only for structured values; a returned value is shown in a [result] block, and a body without return gives no [result] block; input is bound to the input variable. If the script is still running you get an id and it keeps going: use the async tool to wait for, list, inspect or stop it. Saved functions are reusable files called as fn.<name>(input) inside a function body. The script runs with full interpreter privileges: the nested allow-list limits only the tools.* bridge, not what the script itself can do."

// NewToolSmithTool creates the tool_smith tool. It is ReadOnly so several calls may
// share a response, and a call with code says so through MayChangeFiles.
// A non-nil host lets a run that outlasts block_ms continue as a handle of the async tool.
func NewToolSmithTool(settings sessionfs.NativeToolSmithSettings, apiKeyEnv string, nested *ToolSmithNested, host *AsyncHost) (tools.Tool, error) {
	validator, err := tools.CompileSchema(toolSmithInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native tool_smith schema: %w", err)
	}
	sweepStaleScratchOnce()
	if host != nil {
		host.attachShell(toolSmithAsync)
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
		RejectedProperties: map[string]string{
			"run":      "run is not a parameter: pass the script body as the top-level code string, with input and block_ms beside it, for example " + toolSmithRunExample,
			"function": "function is not a parameter any more: pass the script body as the top-level code string, with input and block_ms beside it, for example " + toolSmithRunExample,
		},
		MayChangeFiles: func(arguments map[string]any) bool {
			_, hasCode := arguments["code"]
			return hasCode
		},
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeToolSmith(ctx, arguments, settings, apiKeyEnv, nested, execution, host)
		},
	}, nil
}

// normalizeToolSmithArguments turns a negative or fractional block_ms into a valid one.
func normalizeToolSmithArguments(arguments map[string]any) map[string]any {
	block, ok := safeInteger(arguments["block_ms"])
	if !ok {
		return arguments
	}
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	normalized["block_ms"] = json.Number(fmt.Sprint(max(block, 0)))
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
	host *AsyncHost,
) (string, error) {
	callID := execution.CallID
	if _, hasCode := arguments["code"]; !hasCode {
		return executeToolSmithList(arguments, settings, execution), nil
	}
	body, _ := arguments["code"].(string)
	if strings.TrimSpace(body) == "" {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input", "code is empty",
			tools.Information{Key: "suggestion", Value: "example: " + toolSmithRunExample}), nil
	}
	input := map[string]any{}
	if given, present := arguments["input"]; present && given != nil {
		object, ok := given.(map[string]any)
		if !ok {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input", "input must be an object"), nil
		}
		input = object
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input", "input is not valid JSON data",
			tools.Information{Key: "system_error", Value: err.Error()}), nil
	}
	if size := int64(len(body) + len(inputJSON)); size > settings.CodeMaxBytes {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input",
			fmt.Sprintf("code and input are %d bytes, over the limit of %d", size, settings.CodeMaxBytes),
			tools.Information{Key: "suggestion", Value: "save the code as a library function and call it, or shorten the input"}), nil
	}
	script := toolSmithScript{body: body, inputJSON: inputJSON, settings: settings, apiKeyEnv: apiKeyEnv, nested: nested, execution: execution}
	if _, hasFilters := arguments["filters"]; hasFilters {
		script.notes = []string{"ignored: filters"}
	}
	if _, canHandle := handleOwner(execution); host == nil || !canHandle {
		return runToolSmithScript(ctx, script).text, nil
	}
	var blockMs *int64
	if value, ok := safeInteger(arguments["block_ms"]); ok {
		blockMs = &value
	}
	return startToolSmithAsync(ctx, host, script, blockMs)
}

// toolSmithScript is one validated run: what to execute and under which settings.
type toolSmithScript struct {
	body      string
	inputJSON []byte
	settings  sessionfs.NativeToolSmithSettings
	apiKeyEnv string
	nested    *ToolSmithNested
	execution tools.ExecutionContext
	// notes are information lines added to the result.
	notes []string
}

// runToolSmithScript runs the script to its end in a private work directory and renders the result.
func runToolSmithScript(ctx context.Context, script toolSmithScript) shellRunResult {
	settings, execution, callID := script.settings, script.execution, script.execution.CallID
	workDir, err := makeToolSmithWorkDir(os.TempDir(), toolSmithShortTempDir)
	if err != nil {
		return shellRunResult{text: tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The working files could not be created",
			tools.Information{Key: "system_error", Value: err.Error()})}
	}
	defer os.RemoveAll(workDir)
	if err := requirePrivateDirectory(workDir); err != nil {
		return shellRunResult{text: tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The working directory is not private",
			tools.Information{Key: "system_error", Value: err.Error()})}
	}
	wrapperPath := filepath.Join(workDir, "wrapper.ts")
	inputPath := filepath.Join(workDir, "input.json")
	resultPath := filepath.Join(workDir, "result.txt")
	wrapper := "import { deliver, tools, fn } from \"./prelude.ts\";\nawait deliver(async (input) => {\n" + script.body + "\n});\n"
	for path, content := range map[string]string{
		wrapperPath:                          wrapper,
		filepath.Join(workDir, "prelude.ts"): toolSmithPrelude,
		inputPath:                            string(script.inputJSON),
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return shellRunResult{text: tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The script files could not be written",
				tools.Information{Key: "system_error", Value: err.Error()})}
		}
	}

	if err := stageToolSmithFunctions(workDir, settings, execution.WorkspaceRoot); err != nil {
		return shellRunResult{text: tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The saved functions could not be staged",
			tools.Information{Key: "system_error", Value: err.Error()})}
	}

	argv := make([]string, len(settings.Interpreter))
	for index, item := range settings.Interpreter {
		argv[index] = strings.ReplaceAll(item, "{file}", wrapperPath)
	}
	extraEnv := map[string]string{toolSmithResultEnv: resultPath, toolSmithInputEnv: inputPath,
		toolSmithFunctionsEnv: filepath.Join(workDir, "fn-manifest.json")}
	var bridge *toolSmithBridge
	if allowed := script.nested.allowed(settings); len(allowed) > 0 {
		bridge, err = startToolSmithBridge(ctx, workDir, settings, allowed, execution)
		if err != nil {
			return shellRunResult{text: tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The tool bridge could not be started",
				tools.Information{Key: "system_error", Value: err.Error()})}
		}
		defer bridge.Close()
		extraEnv[toolSmithSocketEnv] = bridge.socketPath
		extraEnv[toolSmithTokenEnv] = bridge.token
	}
	result := runShellChildDetailed(ctx, shellChildRun{
		file:       argv[0],
		argv:       argv[1:],
		cwd:        ".",
		section:    "tools.native.tool_smith",
		extraEnv:   extraEnv,
		resultFile: &shellResultFile{path: resultPath, maxBytes: settings.ResultMaxBytes, cutMarker: true},
		program:    &shellProgram{name: filepath.Base(argv[0]), setting: "tools.native.tool_smith.interpreter", subject: "Script"},
	}, settings.Child, script.apiKeyEnv, execution)
	var lines []string
	if bridge != nil {
		lines = bridge.Close().informationLines()
	}
	lines = append(lines, script.notes...)
	result.text = appendToolSmithInformation(result.text, lines)
	return result
}

// toolSmithMaxSocketPath stays under sun_path (107 bytes on Linux, 103 on macOS).
const toolSmithMaxSocketPath = 100

const toolSmithShortTempDir = "/tmp"

// makeToolSmithWorkDir creates the private work directory under base, or under
// fallback when the bridge socket path inside it would be too long for a unix socket.
func makeToolSmithWorkDir(base, fallback string) (string, error) {
	dir, err := os.MkdirTemp(base, "tool-smith-")
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" || len(filepath.Join(dir, toolSmithSocketName)) <= toolSmithMaxSocketPath {
		return dir, nil
	}
	os.RemoveAll(dir)
	dir, err = os.MkdirTemp(fallback, "tool-smith-")
	if err != nil {
		return "", err
	}
	if length := len(filepath.Join(dir, toolSmithSocketName)); length > toolSmithMaxSocketPath {
		os.RemoveAll(dir)
		return "", fmt.Errorf("the bridge socket path would be %d bytes, over the unix socket limit of %d; use a shorter TMPDIR", length, toolSmithMaxSocketPath)
	}
	return dir, nil
}
