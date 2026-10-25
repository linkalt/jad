package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const (
	execToolID = "native.exec"
	// execResultEnv names the file a program may write its answer to.
	execResultEnv = "JAD_RESULT"

	asyncKindExec asyncmgr.Kind = "exec"
)

var execRunID = regexp.MustCompile("^exec_[0-9a-f]{8}$")

var execAsync = shellAsyncSpec{
	kind: asyncKindExec, idPrefix: "exec_", idPattern: execRunID,
	noun: "Program", labelPrefix: "exec: ", resultsDir: "exec",
}

var execInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "lang":{"type":"string","minLength":1,"description":"Interpreter name, e.g. python, node, bash. Defaults to bash."},
    "code":{"type":"string","minLength":1,"description":"Program source."},
    "input":{"description":"Any JSON value, written to the program's standard input as one line of JSON. Without input, stdin is empty."},
    "args":{"type":"array","items":{"type":"string"},"description":"Extra arguments passed to the program after the code, with no shell quoting. Prefer input for data."},
    "cwd":{"type":"string","minLength":1,"description":"Directory to run in, relative to the workspace root or absolute. Defaults to the workspace root."},
    "timeout_ms":{"type":"integer","description":"Wall-clock budget for this call in milliseconds; clamped to the configured range."},
    "block_ms":{"type":"integer","minimum":0,"description":"Most time to block for the result. If the program is still running an id is returned; collect it with the async tool."}
  },
  "required":["code"],
  "additionalProperties":false
}`)

const execDescription = "Run inline code in an interpreter (python, node, bash, ...). Use it to compute, filter or combine data in one call. Pass data as input: the program reads it from stdin as JSON. Print the answer to stdout; for a short structured answer write to the file named by $JAD_RESULT to see it ahead of the raw output. It waits up to block_ms; if the program is still running you get an id and it keeps going: use the async tool to wait for, list, inspect or stop it."

// NewExecTool constructs native.exec. How a child runs comes from its own settings (Child);
// exec adds the interpreter table, argv without a shell, line-aware cutting and a result file.
// A non-nil host lets a program that outlasts block_ms continue as a handle of the async tool.
func NewExecTool(settings sessionfs.NativeExecSettings, apiKeyEnv string, host *AsyncHost) (tools.Tool, error) {
	validator, err := tools.CompileSchema(execInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native exec schema: %w", err)
	}
	if host != nil {
		host.attachShell(execAsync)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          execToolID,
			Source:      "native",
			Name:        "exec",
			Description: execDescription,
			InputSchema: append(json.RawMessage(nil), execInputSchema...),
		},
		Validator:          validator,
		RejectedProperties: execRejectedProperties,
		ExampleOperation:   json.RawMessage(`{"code":"echo hello"}`),
		// The configured default language is bound here, where the settings are known.
		NormalizeArguments: func(arguments map[string]any) map[string]any {
			return normalizeExecArguments(arguments, settings.DefaultLang)
		},
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeExecAsync(ctx, arguments, settings, apiKeyEnv, execution, host)
		},
	}, nil
}

// execRejectedProperties names the right key for a name an older exec, or another tool,
// accepted. There is one way to say each thing.
var execRejectedProperties = map[string]string{
	"language":    "exec has no language; use lang",
	"interpreter": "exec has no interpreter; use lang",
	"source":      "exec has no source; use code",
	"script":      "exec has no script; use code",
	"program":     "exec has no program; use code",
	"command":     "exec has no command; use code with lang",
	"stdin":       "exec has no stdin; use input (JSON) for data, and code for the program",
	"workdir":     "exec has no workdir; use cwd",
	"dir":         "exec has no dir; use cwd",
	"argv":        "exec has no argv here; use args for extra arguments after the code",
}

// normalizeExecArguments repairs what a model gets wrong without changing the intent: a
// null or blank lang means "not given", block_ms is clamped, and numbers or booleans
// given as args become strings.
func normalizeExecArguments(arguments map[string]any, defaultLang string) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	if value, has := normalized["lang"]; has && value == nil {
		// A model that fills an optional field with null means "not given".
		delete(normalized, "lang")
	}
	if block, ok := safeInteger(normalized["block_ms"]); ok {
		normalized["block_ms"] = json.Number(fmt.Sprint(max(block, 0)))
	}
	if lang, ok := normalized["lang"].(string); ok {
		// A blank language is "not given" too.
		if lang = trimJSSpace(lang); lang == "" {
			delete(normalized, "lang")
		} else {
			normalized["lang"] = lang
		}
	}
	if _, hasCode := normalized["code"]; hasCode && defaultLang != "" && normalized["lang"] == nil {
		// Filled here, not at run time, so the arguments the hooks and the log see are what runs.
		normalized["lang"] = defaultLang
	}
	if list, ok := normalized["args"].([]any); ok {
		converted := make([]any, len(list))
		for index, item := range list {
			converted[index] = execArgumentText(item)
		}
		normalized["args"] = converted
	}
	return normalized
}

// execArgumentText renders a number or boolean the way JavaScript's String() does; any
// other value is left for the schema to reject.
func execArgumentText(value any) any {
	switch item := value.(type) {
	case bool:
		return strconv.FormatBool(item)
	case json.Number:
		return item.String()
	case float64:
		return strconv.FormatFloat(item, 'f', -1, 64)
	case int:
		return strconv.Itoa(item)
	case int64:
		return strconv.FormatInt(item, 10)
	default:
		return value
	}
}

// defaultLangSuggestion is the advice on a failed call that ran in the default language.
// It is true whether the model left `lang` out or named the default itself, because the
// call cannot tell which.
func defaultLangSuggestion(name string, available []string) string {
	return "ran as lang=" + name + ", the default when lang is omitted; if the code is for another interpreter, pass lang (available: " + strings.Join(available, ", ") + ")"
}

// findExecLang returns the entry a name or alias selects, matched without regard to case.
func findExecLang(settings sessionfs.NativeExecSettings, requested string) (string, sessionfs.NativeExecLang, bool) {
	wanted := strings.ToLower(trimJSSpace(requested))
	for _, name := range settings.LangNames {
		lang := settings.Langs[name]
		if name == wanted {
			return name, lang, true
		}
		for _, alias := range lang.Aliases {
			if alias == wanted {
				return name, lang, true
			}
		}
	}
	return "", sessionfs.NativeExecLang{}, false
}

// executeNativeExec runs the program to its end.
func executeNativeExec(
	ctx context.Context,
	arguments map[string]any,
	settings sessionfs.NativeExecSettings,
	apiKeyEnv string,
	execution tools.ExecutionContext,
) string {
	text, _ := executeNativeExecAsync(ctx, arguments, settings, apiKeyEnv, execution, nil)
	return text
}

// executeNativeExecAsync validates the call, then runs the program: to its end when host is
// nil or the call has no owner for a handle, else as a handle that the call waits for up
// to block_ms.
func executeNativeExecAsync(
	ctx context.Context,
	arguments map[string]any,
	settings sessionfs.NativeExecSettings,
	apiKeyEnv string,
	execution tools.ExecutionContext,
	host *AsyncHost,
) (string, error) {
	callID := execution.CallID
	rawLang, _ := arguments["lang"].(string)
	if rawLang == "" {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_lang", "The language is empty"), nil
	}
	requested := trimJSSpace(rawLang)
	code, _ := arguments["code"].(string)
	if trimJSSpace(code) == "" {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_code", "The code is empty"), nil
	}
	name, lang, found := findExecLang(settings, requested)
	if !found {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "unknown_lang", "Unknown language: "+requested,
			tools.Information{Key: "available", Value: strings.Join(settings.LangNames, ", ")},
			tools.Information{Key: "suggestion", Value: "use one of the available names, or add the interpreter to tools.native.exec.langs"}), nil
	}
	if codeBytes := int64(len(code)); codeBytes > settings.CodeMaxBytes {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "code_too_large",
			fmt.Sprintf("code is %d bytes, over the limit of %d", codeBytes, settings.CodeMaxBytes),
			tools.Information{Key: "suggestion", Value: "write the program to a file with the write tool and run it with bash"}), nil
	}
	if lang.Mode == sessionfs.NativeExecModeArg {
		if int64(len(code)) > settings.ArgMaxBytes {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "code_too_large",
				fmt.Sprintf("code is %d bytes, over the limit of %d for a runner that takes it as an argument", len(code), settings.ArgMaxBytes),
				tools.Information{Key: "suggestion", Value: "write the program to a file with the write tool and run it with bash, or give the runner mode file"}), nil
		}
		if strings.ContainsRune(code, 0) {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_code", "code contains a NUL byte, which an argument cannot carry"), nil
		}
	}
	stdin := ""
	if given, present := arguments["input"]; present && given != nil {
		if lang.Mode == sessionfs.NativeExecModeStdin {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "input_unsupported",
				"runner "+name+" reads its code from stdin, so a call with input cannot use it",
				tools.Information{Key: "suggestion", Value: "set tools.native.exec.runners." + name + ".mode to arg or file"}), nil
		}
		encoded, err := json.Marshal(given)
		if err != nil {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input", "input is not valid JSON data",
				tools.Information{Key: "system_error", Value: err.Error()}), nil
		}
		if size := int64(len(encoded)) + 1; size > settings.InputMaxBytes {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "input_too_large",
				fmt.Sprintf("input is %d bytes, over the limit of %d", size, settings.InputMaxBytes),
				tools.Information{Key: "suggestion", Value: "write the data to a file with the write tool and pass its path in input"}), nil
		}
		stdin = string(encoded) + "\n"
	}
	var extra []string
	if raw, present := arguments["args"]; present && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_args", "args must be a list of strings"), nil
		}
		for _, item := range list {
			text, ok := item.(string)
			if !ok {
				return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_args", "args must be a list of strings"), nil
			}
			extra = append(extra, text)
		}
	}

	common := parseBashArguments(arguments)
	// A call without `lang` was given the default, so a failure may mean the code was
	// written for another interpreter (a model that assumes Python sends Python).
	failureSuggestion := ""
	if name == settings.DefaultLang {
		failureSuggestion = defaultLangSuggestion(name, settings.LangNames)
	}
	run := func(ctx context.Context) shellRunResult {
		// A private directory per call holds the code file (file delivery) and the result file;
		// it is removed afterwards, whatever the program did.
		workDir, err := os.MkdirTemp("", "exec-")
		if err != nil {
			return shellRunResult{text: tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The working files could not be created",
				tools.Information{Key: "system_error", Value: err.Error()})}
		}
		defer os.RemoveAll(workDir)
		resultPath := filepath.Join(workDir, "result.txt")

		programArgs := make([]string, len(lang.Argv)-1)
		codePath := filepath.Join(workDir, "code"+lang.Ext)
		if lang.Mode == sessionfs.NativeExecModeFile {
			if err := os.WriteFile(codePath, []byte(code), 0o600); err != nil {
				return shellRunResult{text: tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The code file could not be written",
					tools.Information{Key: "system_error", Value: err.Error()})}
			}
		}
		for index, flag := range lang.Argv[1:] {
			flag = strings.ReplaceAll(flag, "{file}", codePath)
			programArgs[index] = strings.ReplaceAll(flag, "{code}", code)
		}
		runStdin := stdin
		if lang.Mode == sessionfs.NativeExecModeStdin {
			runStdin = code
		}
		return runShellChildDetailed(ctx, shellChildRun{
			file:       lang.Argv[0],
			argv:       append(append([]string(nil), programArgs...), extra...),
			stdin:      runStdin,
			cwd:        common.cwd,
			timeoutMs:  common.timeoutMs,
			lineAware:  settings.LineAware,
			section:    "tools.native.exec",
			extraEnv:   map[string]string{execResultEnv: resultPath},
			resultFile: &shellResultFile{path: resultPath, maxBytes: settings.ResultMaxBytes},
			program:    &shellProgram{name: name, setting: "tools.native.exec.runners." + name + ".argv", failureSuggestion: failureSuggestion},
		}, settings.Child, apiKeyEnv, execution)
	}
	if _, canHandle := handleOwner(execution); host == nil || !canHandle {
		return run(ctx).text, nil
	}
	var blockMs *int64
	if value, ok := safeInteger(arguments["block_ms"]); ok {
		blockMs = &value
	}
	return startShellAsync(ctx, host, execAsync, execution, shellLabel(execAsync, code), blockMs, run)
}
