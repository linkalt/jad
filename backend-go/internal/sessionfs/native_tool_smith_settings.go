package sessionfs

import (
	"encoding/json"
	"strings"
)

const defaultNativeToolSmithFunctionsDir = ".jad/functions"

var defaultNativeToolSmithInterpreter = []string{"bun", "run", nativeExecFilePlaceholder}

const defaultNativeToolSmithCodeMax int64 = 256 * 1024
const defaultNativeToolSmithResultMax int64 = 16 * 1024
const defaultNativeToolSmithNestedResultMax int64 = 16 * 1024
const defaultNativeToolSmithNestedRequestMax int64 = 64 * 1024
const defaultNativeToolSmithMaxNestedCalls int64 = 100
const maxNativeToolSmithMaxNestedCalls int64 = 10000
const defaultNativeToolSmithMaxParallelCalls int64 = 4
const maxNativeToolSmithMaxParallelCalls int64 = 64
const defaultNativeToolSmithListMaxResults int64 = 50
const maxNativeToolSmithListMaxResults int64 = 1000
const defaultNativeToolSmithListMaxFiles int64 = 200
const maxNativeToolSmithListMaxFiles int64 = 10000
const defaultNativeToolSmithListHeaderBytes int64 = 2048
const maxNativeToolSmithListHeaderBytes int64 = 65536
const defaultNativeToolSmithFunctionMax int64 = 64 * 1024

var (
	// nativeToolSmithOwnKeys and the bash-style keys form the section; paths is left out on
	// purpose, so tool_smith has no path policy of its own.
	nativeToolSmithOwnKeys = []string{
		"functions_dir", "code_max_bytes", "result_max_bytes", "nested_result_max_bytes",
		"nested_request_max_bytes", "max_nested_calls", "max_parallel_calls", "list_max_results", "list_max_files", "list_header_bytes", "function_max_bytes", "tools",
		"interpreter",
	}
	nativeToolSmithKeys = append(nativeBashStyleKeysWithout("paths"), nativeToolSmithOwnKeys...)

	// defaultNativeToolSmithTools are the native tools a script may call without configuration.
	defaultNativeToolSmithTools = []string{"read", "grep", "glob"}
	// nativeToolSmithExcludedTools can never be called from a script.
	nativeToolSmithExcludedTools = []string{"bash", "exec", "process", "async", "subagent", "tool_smith", "task"}
)

// NativeToolSmithExcludedTools returns the native tools a script can never call.
func NativeToolSmithExcludedTools() []string {
	return append([]string(nil), nativeToolSmithExcludedTools...)
}

func nativeBashStyleKeysWithout(excluded ...string) []string {
	keys := make([]string, 0, len(nativeBashStyleKeys))
	for _, key := range nativeBashStyleKeys {
		if !containsString(excluded, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

// NativeToolSmithSettings holds the normalized settings of native.tool_smith. Child is how
// the script process runs, read from tools.native.tool_smith with the keys and defaults of
// native.bash; tool_smith never reads bash.
type NativeToolSmithSettings struct {
	Child NativeBashSettings
	// FunctionsDir is the function library directory as configured (relative to the working directory).
	FunctionsDir string
	// CodeMaxBytes is the largest script a call may carry; more is refused, never cut.
	CodeMaxBytes int64
	// ResultMaxBytes is the largest value a run returns; more is cut.
	ResultMaxBytes int64
	// NestedResultMaxBytes is the largest result of one nested tool call handed to a script.
	NestedResultMaxBytes int64
	// NestedRequestMaxBytes is the largest request frame a script may send over the bridge.
	NestedRequestMaxBytes int64
	MaxNestedCalls        int64
	MaxParallelCalls      int64
	ListMaxResults        int64
	ListMaxFiles          int64
	ListHeaderBytes       int64
	// FunctionMaxBytes is the largest saved function file fn copies; larger files count as malformed.
	FunctionMaxBytes int64
	// Tools are the native tools a script may call, in configured order.
	Tools []string
	// Interpreter is the argv that launches the script file; {file} stands for its path.
	Interpreter []string
}

func normalizeNativeToolSmithSettings(
	setting map[string]json.RawMessage,
	globalJadRoot, settingPath, cwd, sessionDir, toolStorageDir, envFileBase string,
	readPolicy, grepPolicy NativePathPolicy,
) (*NativeToolSmithSettings, error) {
	nativeRaw, exists, err := nativeEnvelope(setting, settingPath)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["tool_smith"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.tool_smith"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	if err := rejectRemovedApproval(fields, section, settingPath); err != nil {
		return nil, err
	}
	for _, key := range keys {
		if !containsString(nativeToolSmithKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeToolSmithKeys, "")
		}
	}

	child, err := normalizeBashStyleFields(fields, section, globalJadRoot, settingPath, cwd, sessionDir, toolStorageDir, envFileBase, readPolicy, grepPolicy)
	if err != nil {
		return nil, err
	}
	settings := &NativeToolSmithSettings{Child: *child}
	if settings.FunctionsDir, err = bashString(fields, "functions_dir", defaultNativeToolSmithFunctionsDir, settingPath+": "+section, false); err != nil {
		return nil, err
	}
	if settings.CodeMaxBytes, err = configuredInteger(fields, "code_max_bytes", defaultNativeToolSmithCodeMax, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.ResultMaxBytes, err = configuredInteger(fields, "result_max_bytes", defaultNativeToolSmithResultMax, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.NestedResultMaxBytes, err = configuredInteger(fields, "nested_result_max_bytes", defaultNativeToolSmithNestedResultMax, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.NestedRequestMaxBytes, err = configuredInteger(fields, "nested_request_max_bytes", defaultNativeToolSmithNestedRequestMax, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxNestedCalls, err = configuredInteger(fields, "max_nested_calls", defaultNativeToolSmithMaxNestedCalls, 1, maxNativeToolSmithMaxNestedCalls, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxParallelCalls, err = configuredInteger(fields, "max_parallel_calls", defaultNativeToolSmithMaxParallelCalls, 1, maxNativeToolSmithMaxParallelCalls, settingPath, section); err != nil {
		return nil, err
	}
	if settings.ListMaxResults, err = configuredInteger(fields, "list_max_results", defaultNativeToolSmithListMaxResults, 1, maxNativeToolSmithListMaxResults, settingPath, section); err != nil {
		return nil, err
	}
	if settings.ListMaxFiles, err = configuredInteger(fields, "list_max_files", defaultNativeToolSmithListMaxFiles, 1, maxNativeToolSmithListMaxFiles, settingPath, section); err != nil {
		return nil, err
	}
	if settings.ListHeaderBytes, err = configuredInteger(fields, "list_header_bytes", defaultNativeToolSmithListHeaderBytes, 1, maxNativeToolSmithListHeaderBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.FunctionMaxBytes, err = configuredInteger(fields, "function_max_bytes", defaultNativeToolSmithFunctionMax, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.Tools, err = normalizeNativeToolSmithTools(fields, settingPath+": "+section+".tools"); err != nil {
		return nil, err
	}
	if settings.Interpreter, err = normalizeNativeToolSmithInterpreter(fields, settingPath+": "+section+".interpreter"); err != nil {
		return nil, err
	}
	return settings, nil
}

// normalizeNativeToolSmithInterpreter reads the argv that launches a script file, checked like an exec lang argv.
func normalizeNativeToolSmithInterpreter(fields map[string]json.RawMessage, path string) ([]string, error) {
	raw, present := nullableField(fields, "interpreter")
	if !present {
		return append([]string(nil), defaultNativeToolSmithInterpreter...), nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 || len(items) > maxNativeExecArgv {
		return nil, configError("%s must be 1 to 32 non-empty strings: the interpreter and its flags", path)
	}
	argv := make([]string, 0, len(items))
	placeholders := 0
	for _, item := range items {
		var text string
		if json.Unmarshal(item, &text) != nil || text == "" {
			return nil, configError("%s must be 1 to 32 non-empty strings: the interpreter and its flags", path)
		}
		if strings.Contains(text, nativeExecFilePlaceholder) {
			placeholders++
		}
		argv = append(argv, text)
	}
	if placeholders == 0 {
		return nil, configError("%s must contain %s", path, nativeExecFilePlaceholder)
	}
	return argv, nil
}

// normalizeNativeToolSmithTools reads the allowlist of native tools a script may call.
func normalizeNativeToolSmithTools(fields map[string]json.RawMessage, path string) ([]string, error) {
	raw, present := nullableField(fields, "tools")
	if !present {
		return append([]string(nil), defaultNativeToolSmithTools...), nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || items == nil {
		return nil, configError("%s must be an array of native tool names", path)
	}
	tools := make([]string, 0, len(items))
	for index, item := range items {
		var name string
		if json.Unmarshal(item, &name) != nil || name == "" {
			return nil, configError("%s[%d] must be a native tool name", path, index)
		}
		if containsString(nativeToolSmithExcludedTools, name) {
			return nil, configError("%s[%d] \"%s\" cannot be called from a script", path, index, name)
		}
		if !containsString(nativeToolNames, name) && !containsString(extensionToolNames, name) {
			return nil, configError("%s[%d] \"%s\" is not a native tool", path, index, name)
		}
		if containsString(tools, name) {
			return nil, configError("%s[%d] \"%s\" is listed twice", path, index, name)
		}
		tools = append(tools, name)
	}
	return tools, nil
}
