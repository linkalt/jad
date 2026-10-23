package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const (
	execToolID = "native.exec"
	// execResultEnv names the file a program may write its answer to.
	execResultEnv = "EXEC_RESULT"
)

var execInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "lang":{"type":"string","minLength":1,"description":"Interpreter name, e.g. python, node, bash. Defaults to bash."},
    "code":{"type":"string","minLength":1,"description":"Program source."},
    "args":{"type":"array","items":{"type":"string"},"description":"Arguments passed to the program as argv, with no shell quoting."},
    "cwd":{"type":"string","minLength":1,"description":"Directory to run in, relative to the workspace root or absolute. Defaults to the workspace root."},
    "timeout_ms":{"type":"integer","description":"Wall-clock budget for this call in milliseconds; clamped to the configured range."}
  },
  "required":["code"],
  "additionalProperties":false
}`)

const execDescription = "Run inline code in an interpreter (python, node, bash, ...). Use it to compute, filter or combine data in one call. Write a short answer to the file named by $EXEC_RESULT to see it ahead of the raw output."

// NewExecTool constructs native.exec. How a child runs comes from its own settings (Child);
// exec adds the interpreter table, argv without a shell, line-aware cutting and a result file.
func NewExecTool(settings sessionfs.NativeExecSettings, apiKeyEnv string) (tools.Tool, error) {
	validator, err := tools.CompileSchema(execInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native exec schema: %w", err)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          execToolID,
			Source:      "native",
			Name:        "exec",
			Description: execDescription,
			InputSchema: append(json.RawMessage(nil), execInputSchema...),
		},
		Validator:        validator,
		ExampleOperation: json.RawMessage(`{"code":"echo hello"}`),
		// The configured default language is bound here, where the settings are known.
		NormalizeArguments: func(arguments map[string]any) map[string]any {
			return normalizeExecArguments(arguments, settings.DefaultLang)
		},
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeExec(ctx, arguments, settings, apiKeyEnv, execution), nil
		},
	}, nil
}

// normalizeExecArguments turns the near-miss names a model uses for the same intent, and
// numbers or booleans given as arguments, into what the schema asks for.
func normalizeExecArguments(arguments map[string]any, defaultLang string) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	if value, has := normalized["lang"]; has && value == nil {
		// A model that fills an optional field with null means "not given".
		delete(normalized, "lang")
	}
	if _, has := normalized["lang"]; !has {
		if alias, ok := firstString(arguments, "language", "interpreter"); ok {
			normalized["lang"] = alias
		}
	}
	if lang, ok := normalized["lang"].(string); ok {
		// A blank language is "not given" too.
		if lang = trimJSSpace(lang); lang == "" {
			delete(normalized, "lang")
		} else {
			normalized["lang"] = lang
		}
	}
	if _, has := normalized["lang"]; !has && defaultLang != "" {
		// Filled here, not at run time, so the arguments the hooks and the log see are what runs.
		normalized["lang"] = defaultLang
	}
	if _, has := normalized["code"]; !has {
		if alias, ok := firstString(arguments, "source", "script", "program"); ok {
			normalized["code"] = alias
		}
	}
	if _, has := normalized["args"]; !has {
		if argv, ok := arguments["argv"].([]any); ok {
			normalized["args"] = argv
		}
	}
	if list, ok := normalized["args"].([]any); ok {
		converted := make([]any, len(list))
		for index, item := range list {
			converted[index] = execArgumentText(item)
		}
		normalized["args"] = converted
	}
	if _, has := normalized["cwd"]; !has {
		if alias, ok := firstString(arguments, "workdir", "dir"); ok {
			normalized["cwd"] = alias
		}
	}
	for _, key := range []string{"language", "interpreter", "source", "script", "program", "argv", "workdir", "dir"} {
		delete(normalized, key)
	}
	return normalized
}

// firstString returns the first of the named arguments that is a string. An earlier name
// whose value is not a string does not hide a later one that is: `a ?? b` in the
// TypeScript backend picks the first present value, so a non-string stops the search.
func firstString(arguments map[string]any, names ...string) (string, bool) {
	for _, name := range names {
		value, present := arguments[name]
		if !present || value == nil {
			continue
		}
		text, ok := value.(string)
		return text, ok
	}
	return "", false
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

func executeNativeExec(
	ctx context.Context,
	arguments map[string]any,
	settings sessionfs.NativeExecSettings,
	apiKeyEnv string,
	execution tools.ExecutionContext,
) string {
	callID := execution.CallID
	rawLang, _ := arguments["lang"].(string)
	if rawLang == "" {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_lang", "The language is empty")
	}
	requested := trimJSSpace(rawLang)
	code, _ := arguments["code"].(string)
	if trimJSSpace(code) == "" {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_code", "The code is empty")
	}
	name, lang, found := findExecLang(settings, requested)
	if !found {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "unknown_lang", "Unknown language: "+requested,
			tools.Information{Key: "available", Value: strings.Join(settings.LangNames, ", ")},
			tools.Information{Key: "suggestion", Value: "use one of the available names, or add the interpreter to tools.native.exec.langs"})
	}
	if codeBytes := int64(len(code)); codeBytes > settings.CodeMaxBytes {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "code_too_large",
			fmt.Sprintf("code is %d bytes, over the limit of %d", codeBytes, settings.CodeMaxBytes),
			tools.Information{Key: "suggestion", Value: "write the program to a file with the write tool and run it with bash"})
	}
	var extra []string
	if raw, present := arguments["args"]; present && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_args", "args must be a list of strings")
		}
		for _, item := range list {
			text, ok := item.(string)
			if !ok {
				return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_args", "args must be a list of strings")
			}
			extra = append(extra, text)
		}
	}

	// A private directory per call holds the code file (file delivery) and the result file;
	// it is removed afterwards, whatever the program did.
	workDir, err := os.MkdirTemp("", "exec-")
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The working files could not be created",
			tools.Information{Key: "system_error", Value: err.Error()})
	}
	defer os.RemoveAll(workDir)
	resultPath := filepath.Join(workDir, "result.txt")

	flags := lang.Argv[1:]
	stdin := ""
	programArgs := flags
	if lang.Code == sessionfs.NativeExecCodeFile {
		codePath := filepath.Join(workDir, "code"+lang.Ext)
		if err := os.WriteFile(codePath, []byte(code), 0o600); err != nil {
			return tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The code file could not be written",
				tools.Information{Key: "system_error", Value: err.Error()})
		}
		programArgs = make([]string, len(flags))
		for index, flag := range flags {
			programArgs[index] = strings.ReplaceAll(flag, "{file}", codePath)
		}
	} else {
		stdin = code
	}
	common := parseBashArguments(arguments)
	// A call without `lang` was given the default, so a failure may mean the code was
	// written for another interpreter (a model that assumes Python sends Python).
	failureSuggestion := ""
	if name == settings.DefaultLang {
		failureSuggestion = defaultLangSuggestion(name, settings.LangNames)
	}
	return runShellChild(ctx, shellChildRun{
		file:       lang.Argv[0],
		argv:       append(append([]string(nil), programArgs...), extra...),
		stdin:      stdin,
		cwd:        common.cwd,
		timeoutMs:  common.timeoutMs,
		lineAware:  settings.LineAware,
		section:    "tools.native.exec",
		extraEnv:   map[string]string{execResultEnv: resultPath},
		resultFile: &shellResultFile{path: resultPath, maxBytes: settings.ResultMaxBytes},
		program:    &shellProgram{name: name, setting: "tools.native.exec.langs." + name + ".argv", failureSuggestion: failureSuggestion},
	}, settings.Child, apiKeyEnv, execution)
}
