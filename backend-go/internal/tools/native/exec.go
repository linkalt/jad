package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const (
	execToolID = "native.exec"
	// execResultEnv names the file a program may write its answer to.
	execResultEnv = "JAD_RESULT"
	// execFunctionsDirEnv names the real root of the helper library for code that calls another language's helper.
	execFunctionsDirEnv = "JAD_FUNCTIONS_DIR"
	// execRunnerDirEnv names the runner's own helper folder, which a template's fn looks in at call time.
	execRunnerDirEnv = "JAD_RUNNER_DIR"

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
    "code":{"type":"string","minLength":1,"description":"Program source, run by the interpreter named by lang. A plain command is lang bash with the command as code."},
    "filters":{"type":"array","items":{"type":"string"},"description":"Only with no code: list the saved helpers whose name or description matches every filter, case-insensitive. A filter with * or ? is a wildcard pattern (read*, *file*); any other is a substring."},
    "input":{"description":"Any JSON value, written to the program's standard input as one line of JSON. Without input, stdin is empty."},
    "cwd":{"type":"string","minLength":1,"description":"Directory to run in, relative to the workspace root or absolute. Defaults to the workspace root."},
    "block_ms":{"type":"integer","minimum":0,"description":"Most time to block for the result. If the program is still running an id is returned; collect it with the async tool."},
    "read_only":{"type":"boolean","description":"Set true for a call that changes no file: searching, listing, printing files, git log, diff or status. Read-only calls of one response may run at the same time. The call runs in a sandbox where only the plan-mode writable paths (by default the temp folder) can be written; on a host where no such sandbox works, read_only does not count and the call runs in order. In plan mode every call runs in that sandbox, and where there is none plan mode refuses calls with code."}
  },
  "additionalProperties":false
}`)

// execReadOnlyProperty is the argument that marks an exec call as one that changes no file.
const execReadOnlyProperty = "read_only"

// execInputSchemaBeforeReadOnly is the schema exec had before read_only, which a tool
// definition file written then still holds (see requireExecDefinition).
var execInputSchemaBeforeReadOnly = schemaWithoutProperty(execInputSchema, execReadOnlyProperty)

// execReadOnlyPromiseText is read_only's description before it said that a session may
// ignore read_only (tools.native.exec.read_only_calls "sandboxed" with no sandbox).
const execReadOnlyPromiseText = "Set true for a call that changes no file: searching, listing, printing files, git log, diff or status. Read-only calls of one response may run at the same time, and plan mode runs only these. Under a bwrap or seatbelt sandbox only the plan-mode writable paths (by default the temp folder) are writable for the call; without one, read_only is your promise not to change files."

// execInputSchemaPromiseOnly is the schema with that description, which a tool definition
// file written then still holds (see requireExecDefinition).
var execInputSchemaPromiseOnly = schemaWithPropertyDescription(execInputSchema, execReadOnlyProperty, execReadOnlyPromiseText)

// execReadOnlyIgnorableText is read_only's description while tools.native.exec.read_only_calls
// chose whether it counted without a sandbox (removed with D3).
const execReadOnlyIgnorableText = "Set true for a call that changes no file: searching, listing, printing files, git log, diff or status. Read-only calls of one response may run at the same time, and plan mode runs only these. Under a bwrap or seatbelt sandbox only the plan-mode writable paths (by default the temp folder) are writable for the call; without one, read_only is your promise not to change files, and a session may be set to ignore it: the call then runs in order and plan mode refuses it."

// execInputSchemaIgnorable is the schema with that description, which a tool definition file
// written then still holds (see requireExecDefinition).
var execInputSchemaIgnorable = schemaWithPropertyDescription(execInputSchema, execReadOnlyProperty, execReadOnlyIgnorableText)

const execDescription = "Call with no parameters (or only filters, e.g. read*) to list the saved helpers per language. Run inline code in an interpreter (python, node, bash, ...). Use it to compute, filter or combine data in one call; a plain command is lang bash with the command as code. Code calls the saved helpers of its own language through fn (fn.<name>(...) in python, node, ts, deno, ruby and lua; fn->name(...) in perl; $fn->name(...) in php; fn$name(...) in r; found when the call is made, so a helper the program just wrote works) or by a bare command name (bash); the listing shows exactly what to type and which member runs. php code is a file, so it starts with <?php. Helpers may also import each other. Another language's helper is run as a subprocess from the directory in $JAD_FUNCTIONS_DIR. Pass data as input: the program reads it from stdin as JSON. Print the answer to stdout; for a short structured answer write to the file named by $JAD_RESULT to see it ahead of the raw output. It waits up to block_ms; if the program is still running you get an id and it keeps going: use the async tool to wait for, list, inspect or stop it."

// NewExecTool constructs native.exec. How a child runs comes from its own settings (Child);
// exec adds the interpreter table, argv without a shell, line-aware cutting and a result file.
// A non-nil host lets a program that outlasts block_ms continue as a handle of the async tool.
func NewExecTool(settings sessionfs.NativeExecSettings, apiKeyEnv string, host *AsyncHost) (tools.Tool, error) {
	validator, err := tools.CompileSchema(execInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native exec schema: %w", err)
	}
	sweepStaleScratchOnce()
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
		// ReadOnly lets the calls that change no file (a read_only call held by a sandbox that
		// keeps the project read-only, or a helper listing) share a response; every other call
		// says so through MayChangeFiles. In plan mode every call starts in that sandbox, so
		// PlanModeRefusal refuses only where there is none.
		ReadOnly: true,
		MayChangeFiles: func(arguments map[string]any) bool {
			return execCallMayChangeFiles(arguments, execReadOnlyEnforced(settings))
		},
		PlanModeRefusal: execPlanModeRefusal(settings),
		// The configured default language is bound here, where the settings are known.
		NormalizeArguments: func(arguments map[string]any) map[string]any {
			return normalizeExecArguments(arguments, settings.DefaultLang)
		},
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeExecAsync(ctx, arguments, settings, apiKeyEnv, execReadOnlyExecution(arguments, execution), host)
		},
	}, nil
}

// execReadOnly reports whether the call sets read_only true.
func execReadOnly(arguments map[string]any) bool {
	readOnly, _ := arguments[execReadOnlyProperty].(bool)
	return readOnly
}

// execReadOnlyExecution is the execution a call runs with: a read_only call's children start
// in the sandbox of a plan-mode call, where only sandbox.plan_writable_paths are writable
// (childCallSandbox). Where no sandbox works on the host nothing changes.
func execReadOnlyExecution(arguments map[string]any, execution tools.ExecutionContext) tools.ExecutionContext {
	if execReadOnly(arguments) {
		execution.PlanMode = true
	}
	return execution
}

// execCallMayChangeFiles is exec's per-call answer to "can this call change files": a call
// that runs code can, unless it sets read_only and enforced says a sandbox keeps such a call
// from writing the project (execReadOnlyEnforced). A call without code only lists helpers.
func execCallMayChangeFiles(arguments map[string]any, enforced bool) bool {
	return execGiven(arguments, "code") && !(enforced && execReadOnly(arguments))
}

// schemaWithoutProperty returns schema, a JSON object schema, without the named property.
// It panics on a schema that is not such an object: it is only called on the constants here.
func schemaWithoutProperty(schema json.RawMessage, name string) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(schema, &fields); err != nil {
		panic(fmt.Sprintf("schemaWithoutProperty: %v", err))
	}
	var properties map[string]json.RawMessage
	if err := json.Unmarshal(fields["properties"], &properties); err != nil {
		panic(fmt.Sprintf("schemaWithoutProperty: properties: %v", err))
	}
	delete(properties, name)
	encoded, err := json.Marshal(properties)
	if err != nil {
		panic(fmt.Sprintf("schemaWithoutProperty: %v", err))
	}
	fields["properties"] = encoded
	if encoded, err = json.Marshal(fields); err != nil {
		panic(fmt.Sprintf("schemaWithoutProperty: %v", err))
	}
	return encoded
}

// schemaWithPropertyDescription returns schema, a JSON object schema, with the named
// property's description replaced. It panics like schemaWithoutProperty.
func schemaWithPropertyDescription(schema json.RawMessage, name, description string) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(schema, &fields); err != nil {
		panic(fmt.Sprintf("schemaWithPropertyDescription: %v", err))
	}
	var properties map[string]map[string]any
	if err := json.Unmarshal(fields["properties"], &properties); err != nil || properties[name] == nil {
		panic(fmt.Sprintf("schemaWithPropertyDescription: properties.%s: %v", name, err))
	}
	properties[name]["description"] = description
	encoded, err := json.Marshal(properties)
	if err != nil {
		panic(fmt.Sprintf("schemaWithPropertyDescription: %v", err))
	}
	fields["properties"] = encoded
	if encoded, err = json.Marshal(fields); err != nil {
		panic(fmt.Sprintf("schemaWithPropertyDescription: %v", err))
	}
	return encoded
}

// execRejectedProperties names the right key for a name an older exec, or another tool,
// accepted. There is one way to say each thing.
var execRejectedProperties = map[string]string{
	"language":    "exec has no language; use lang",
	"interpreter": "exec has no interpreter; use lang",
	"source":      "exec has no source; use code",
	"script":      "exec has no script; use code",
	"program":     "exec has no program; use code",
	"command":     "command is only an alias of code (a string); send code, or the same text in both",
	"stdin":       "exec has no stdin; use input (JSON) for data, and code for the program",
	"workdir":     "exec has no workdir; use cwd",
	"dir":         "exec has no dir; use cwd",
	"function":    "exec has no function mode; saved helpers are called from code (call exec with no parameters to list them)",
	"argv":        "exec has no argv; use code with lang (a plain command is lang bash, code the command)",
	"args":        "exec has no args; pass data as input (JSON on stdin)",
	"timeout_ms":  "exec has no timeout_ms; the timeout is the configured tools.native.exec.timeout_ms",
}

// normalizeExecArguments repairs what a model gets wrong without changing the intent: a
// null or blank lang means "not given", and block_ms is clamped.
func normalizeExecArguments(arguments map[string]any, defaultLang string) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	if value, has := normalized["lang"]; has && value == nil {
		// A model that fills an optional field with null means "not given".
		delete(normalized, "lang")
	}
	foldExecCommandAlias(normalized)
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
	return normalized
}

// foldExecCommandAlias moves command into code. A null command is "not given". A command
// that cannot be folded (a different code, or not a string) stays, so the schema refuses it
// with the hint in execRejectedProperties instead of one value being chosen silently.
func foldExecCommandAlias(arguments map[string]any) {
	command, has := arguments["command"]
	if !has {
		return
	}
	if command == nil {
		delete(arguments, "command")
		return
	}
	text, isString := command.(string)
	if !isString {
		return
	}
	code, hasCode := arguments["code"]
	if !hasCode || code == nil {
		arguments["code"] = text
		delete(arguments, "command")
	} else if existing, ok := code.(string); ok && existing == text {
		delete(arguments, "command")
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
	settings.Child.Policy = workspacePolicy(settings.Child.Policy, execution)
	callID := execution.CallID
	if !execGiven(arguments, "code") {
		return executeExecList(arguments, settings, execution), nil
	}
	plan, problem := planExec(arguments, settings, execution)
	if problem != "" {
		return problem, nil
	}
	cwd := "."
	if value, ok := arguments["cwd"].(string); ok {
		cwd = value
	}
	run := func(ctx context.Context) shellRunResult {
		var template []byte
		if plan.lang.Template != "" {
			text, err := execTemplateText(settings, plan.lang, plan.langName, execution.WorkspaceRoot)
			if err != nil {
				return shellRunResult{text: execTemplateError(callID, plan, err)}
			}
			template = text
		}
		helperPrepend, helperEnv := execHelperEnvironment(settings, plan.lang, plan.langName, execution.WorkspaceRoot)
		standby := takeExecStandby(plan, settings, apiKeyEnv, execution, cwd, template, helperPrepend, helperEnv)
		// A private directory per call holds the code file (file delivery), the template and the
		// result file; it is removed afterwards, whatever the program did. A standby brings its own.
		var workDir string
		if standby != nil {
			workDir = standby.workDir
		} else {
			tmp, err := callTempDir(childCallSandbox("exec", settings.Child, execution), execution)
			if err != nil {
				return shellRunResult{text: sandboxTempError(callID, err)}
			}
			created, err := makeExecWorkDir(tmp)
			if err != nil {
				return shellRunResult{text: tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The working files could not be created",
					tools.Information{Key: "system_error", Value: err.Error()})}
			}
			workDir = created
		}
		defer os.RemoveAll(workDir)
		launch := plan.launch(workDir, plan.code, "code", template, helperPrepend, helperEnv)
		if err := writeStandbyFiles(launch.codeFiles); err != nil {
			if standby != nil {
				standby.stop()
			}
			return shellRunResult{text: execCodeFileError(callID, err)}
		}
		if standby == nil {
			// A standby's directory already holds the same template: it is part of its key.
			if err := writeStandbyFiles(launch.templateFiles); err != nil {
				return shellRunResult{text: execTemplateError(callID, plan, err)}
			}
		}
		return runShellChildDetailed(ctx, shellChildRun{
			file:        launch.file,
			argv:        launch.argv,
			stdin:       launch.stdin,
			cwd:         cwd,
			lineAware:   settings.LineAware,
			section:     "tools.native.exec",
			extraEnv:    launch.extraEnv,
			prependEnv:  launch.prependEnv,
			resultFile:  &shellResultFile{path: launch.resultPath, maxBytes: settings.ResultMaxBytes},
			program:     &plan.program,
			standby:     standby,
			standbyCode: plan.code,
		}, settings.Child, apiKeyEnv, execution)
	}
	if _, canHandle := handleOwner(execution); host == nil || !canHandle {
		return run(ctx).text, nil
	}
	var blockMs *int64
	if value, ok := safeInteger(arguments["block_ms"]); ok {
		blockMs = &value
	}
	return startShellAsync(ctx, host, execAsync, shellStopBound(settings.Child), execution, plan.label, blockMs, run)
}

// execPlan is one validated call: what to label, how to name the program in errors, and how
// to build the command inside the call's scratch directory.
type execPlan struct {
	label   string
	program shellProgram
	// langName and lang are the runner the code runs in.
	langName string
	lang     sessionfs.NativeExecLang
	// code is the call's program and stdin the text written to its standard input.
	code   string
	stdin  string
	callID string
}

// execLaunch is how one call's program starts inside its scratch directory.
type execLaunch struct {
	file       string
	argv       []string
	stdin      string
	extraEnv   map[string]string
	prependEnv map[string]string
	resultPath string
	// codeFiles holds the code file of a mode file runner; templateFiles the runner's template.
	codeFiles     []standbyFile
	templateFiles []standbyFile
}

// launch builds the program, its arguments, environment and files in workDir. body stands for
// {code} or is written to the code file, named name plus the runner's extension: the call's
// code, or a standby shim, which reads the code later. The template text is nil when the
// runner has none. Nothing is written here.
func (plan execPlan) launch(workDir, body, name string, template []byte, helperPrepend, helperEnv map[string]string) execLaunch {
	lang := plan.lang
	codePath := filepath.Join(workDir, name+lang.Ext)
	launch := execLaunch{file: lang.Argv[0], stdin: plan.stdin, resultPath: filepath.Join(workDir, "result.txt")}
	if lang.Mode == sessionfs.NativeExecModeFile {
		launch.codeFiles = []standbyFile{{path: codePath, data: []byte(body)}}
	}
	if lang.Mode == sessionfs.NativeExecModeStdin {
		launch.stdin = body
	}
	// The template is attached before the code is inserted: the model's program is not a
	// template, so a literal {template} inside it must reach the runner as written (P6BM-02).
	templatePath := ""
	if lang.Template != "" {
		templatePath = execTemplatePath(lang, workDir)
	}
	launch.argv = make([]string, 0, len(lang.Argv)-1)
	for _, flag := range lang.Argv[1:] {
		flag = strings.ReplaceAll(flag, "{file}", codePath)
		if templatePath != "" {
			flag = strings.ReplaceAll(flag, "{template}", templatePath)
		}
		launch.argv = append(launch.argv, strings.ReplaceAll(flag, "{code}", body))
	}
	launch.prependEnv = map[string]string{}
	for variable, dir := range helperPrepend {
		launch.prependEnv[variable] = dir
	}
	launch.extraEnv = map[string]string{execResultEnv: launch.resultPath}
	if template != nil {
		switch lang.Template {
		case sessionfs.NativeExecTemplatePython, sessionfs.NativeExecTemplatePerl:
			// A module in a folder in front of the language's own path list: sitecustomize.py,
			// which python runs at start-up, or JadFn.pm, which perl's -MJadFn loads.
			variable, module := "PYTHONPATH", "sitecustomize.py"
			if lang.Template == sessionfs.NativeExecTemplatePerl {
				variable, module = "PERL5LIB", "JadFn.pm"
			}
			dir := filepath.Join(workDir, "template")
			launch.templateFiles = []standbyFile{{path: dir, dir: true}, {path: filepath.Join(dir, module), data: template}}
			if existing := launch.prependEnv[variable]; existing != "" {
				launch.prependEnv[variable] = dir + string(os.PathListSeparator) + existing
			} else {
				launch.prependEnv[variable] = dir
			}
		case sessionfs.NativeExecTemplateR:
			// R sources the file R_PROFILE_USER names at start-up; the template sources the
			// user's own .Rprofile first.
			launch.templateFiles = []standbyFile{{path: templatePath, data: template}}
			launch.extraEnv["R_PROFILE_USER"] = templatePath
		default:
			launch.templateFiles = []standbyFile{{path: templatePath, data: template}}
		}
	}
	for variable, value := range helperEnv {
		launch.extraEnv[variable] = value
	}
	return launch
}

// prepare writes the call's code file into workDir and returns the program, its arguments and
// the stdin text; problem is a rendered error when it cannot.
func (plan execPlan) prepare(workDir string) (file string, argv []string, stdin, problem string) {
	launch := plan.launch(workDir, plan.code, "code", nil, nil, nil)
	if err := writeStandbyFiles(launch.codeFiles); err != nil {
		return "", nil, "", execCodeFileError(plan.callID, err)
	}
	return launch.file, launch.argv, launch.stdin, ""
}

func execCodeFileError(callID string, err error) string {
	return tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The code file could not be written",
		tools.Information{Key: "system_error", Value: err.Error()})
}

func execTemplateError(callID string, plan execPlan, err error) string {
	return tools.ExecutionError(callID, tools.StatusError, "template_failed", "The template for "+plan.langName+" could not be prepared",
		tools.Information{Key: "system_error", Value: err.Error()},
		tools.Information{Key: "suggestion", Value: "fix or remove functions_dir/" + plan.langName + "/_template" + plan.lang.Ext})
}

// makeExecWorkDir is the private scratch directory of one exec run or standby, in the call's
// temp folder tmp (callTempDir): the system temp folder when tmp is empty.
func makeExecWorkDir(tmp string) (string, error) {
	return os.MkdirTemp(tmp, "exec-")
}

// execWorkDirIn makes exec scratch directories in tmp, for a standby pool.
func execWorkDirIn(tmp string) func() (string, error) {
	return func() (string, error) { return makeExecWorkDir(tmp) }
}

// sandboxTempError is the result of a call whose sandbox temp folder could not be made.
func sandboxTempError(callID string, err error) string {
	return tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The sandbox's temp folder could not be made",
		tools.Information{Key: "system_error", Value: err.Error()})
}

// takeExecStandby returns a waiting interpreter started exactly as this call would start one,
// or nil, and starts a replacement in the background. A runner without a shim, a call whose
// directory does not resolve, or standby_pool 0 gets nil and starts its own.
func takeExecStandby(plan execPlan, settings sessionfs.NativeExecSettings, apiKeyEnv string, execution tools.ExecutionContext, cwd string, template []byte, helperPrepend, helperEnv map[string]string) *standbyProcess {
	if settings.StandbyPool <= 0 {
		return nil
	}
	pipe := plan.lang.Standby == sessionfs.NativeExecStandbyPipe && plan.lang.Mode == sessionfs.NativeExecModeFile
	shim := standbyShim(plan.lang)
	if !pipe && (shim == "" || (plan.lang.Mode == sessionfs.NativeExecModeArg && int64(len(shim)) > settings.ArgMaxBytes)) {
		return nil
	}
	resolved, problem := resolveExistingTarget(cwd, execution.WorkspaceRoot, settings.Child.Policy, execution.CallID)
	if problem != "" {
		return nil
	}
	if info, err := os.Stat(resolved.realPath); err != nil || !info.IsDir() {
		return nil
	}
	spawnFor := func(execution tools.ExecutionContext) func(workDir string) (standbySpawn, error) {
		sandbox := childCallSandbox("exec", settings.Child, execution)
		tmp, tmpErr := callTempDir(sandbox, execution)
		return func(workDir string) (standbySpawn, error) {
			if tmpErr != nil {
				return standbySpawn{}, tmpErr
			}
			if pipe {
				// The call's own launch: argv, environment and code file path are what a fresh
				// start of this call would have; only the code file is a named pipe for now.
				launch := plan.launch(workDir, "", "code", template, helperPrepend, helperEnv)
				environment := shellChildEnvironment(shellChildRun{extraEnv: launch.extraEnv, prependEnv: launch.prependEnv}, settings.Child, apiKeyEnv)
				return standbySpawn{file: launch.file, argv: launch.argv, cwd: resolved.realPath, environment: environment, files: launch.templateFiles, pipe: launch.codeFiles[0].path, sandbox: sandbox, workspace: execution.WorkspaceRoot, tmp: tmp, sessionDir: execution.SessionDir}, nil
			}
			launch := plan.launch(workDir, shim, standbyShimName, template, helperPrepend, helperEnv)
			environment := shellChildEnvironment(shellChildRun{extraEnv: launch.extraEnv, prependEnv: launch.prependEnv}, settings.Child, apiKeyEnv)
			files := append(append([]standbyFile(nil), launch.templateFiles...), launch.codeFiles...)
			return standbySpawn{file: launch.file, argv: launch.argv, cwd: resolved.realPath, environment: environment, files: files, sandbox: sandbox, workspace: execution.WorkspaceRoot, tmp: tmp, sessionDir: execution.SessionDir}, nil
		}
	}
	spawn := spawnFor(execution)
	keyed, err := spawn(standbyKeyDir)
	if err != nil {
		return nil
	}
	key := standbyKey(keyed)
	idle := time.Duration(settings.StandbyIdleMs) * time.Millisecond
	process := execStandby.take(key, idle)
	execStandby.refill(key, execWorkDirIn(keyed.tmp), spawn, int(settings.StandbyPool), int(settings.StandbyMax), idle)
	// A read_only call starts in the plan-mode sandbox (the fallback one where no sandbox
	// covers exec), so it cannot take a standby a call that may write started. A call outside
	// plan mode also warms that form, after its own, so the run's read_only calls find one
	// waiting too. A plan-mode call does not warm the other way: its run makes no other kind
	// of call.
	if !execution.PlanMode && execReadOnlyEnforced(settings) {
		readOnly := execution
		readOnly.PlanMode = true
		readOnlySpawn := spawnFor(readOnly)
		if keyed, err := readOnlySpawn(standbyKeyDir); err == nil {
			execStandby.refill(standbyKey(keyed), execWorkDirIn(keyed.tmp), readOnlySpawn, int(settings.StandbyPool), int(settings.StandbyMax), idle)
		}
	}
	return process
}

func execGiven(arguments map[string]any, key string) bool {
	value, present := arguments[key]
	return present && value != nil
}

// planExec validates the call and returns the plan, or a rendered error naming the keys at fault.
func planExec(arguments map[string]any, settings sessionfs.NativeExecSettings, execution tools.ExecutionContext) (execPlan, string) {
	callID := execution.CallID
	invalid := func(code, summary string, more ...tools.Information) (execPlan, string) {
		return execPlan{}, tools.ExecutionError(callID, tools.StatusInvalidInput, code, summary, more...)
	}
	if execGiven(arguments, "filters") {
		return invalid("conflicting_input", "filters is only for listing; call exec with no parameters (or only filters)")
	}
	stdin := ""
	if value, present := arguments["input"]; present && value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			return invalid("invalid_input", "input is not valid JSON data", tools.Information{Key: "system_error", Value: err.Error()})
		}
		if size := int64(len(encoded)) + 1; size > settings.InputMaxBytes {
			return invalid("input_too_large", fmt.Sprintf("input is %d bytes, over the limit of %d", size, settings.InputMaxBytes),
				tools.Information{Key: "suggestion", Value: "write the data to a file in earlier exec calls and pass its path in input"})
		}
		stdin = string(encoded) + "\n"
	}
	return planExecCode(arguments, settings, stdin, callID, invalid)
}

func planExecCode(arguments map[string]any, settings sessionfs.NativeExecSettings, stdin, callID string, invalid func(code, summary string, more ...tools.Information) (execPlan, string)) (execPlan, string) {
	rawLang, _ := arguments["lang"].(string)
	if rawLang == "" {
		return invalid("invalid_lang", "The language is empty")
	}
	requested := trimJSSpace(rawLang)
	code, _ := arguments["code"].(string)
	if trimJSSpace(code) == "" {
		return invalid("invalid_code", "The code is empty")
	}
	name, lang, found := findExecLang(settings, requested)
	if !found {
		return invalid("unknown_lang", "Unknown language: "+requested,
			tools.Information{Key: "available", Value: strings.Join(settings.LangNames, ", ")},
			tools.Information{Key: "suggestion", Value: "use one of the available names, or add the interpreter to tools.native.exec.runners"})
	}
	if codeBytes := int64(len(code)); codeBytes > settings.CodeMaxBytes {
		return invalid("code_too_large", fmt.Sprintf("code is %d bytes, over the limit of %d", codeBytes, settings.CodeMaxBytes),
			tools.Information{Key: "suggestion", Value: "write the program to a file in several exec calls (cat >> file) and run the file"})
	}
	if lang.Mode == sessionfs.NativeExecModeArg {
		if int64(len(code)) > settings.ArgMaxBytes {
			return invalid("code_too_large",
				fmt.Sprintf("code is %d bytes, over the limit of %d for a runner that takes it as an argument", len(code), settings.ArgMaxBytes),
				tools.Information{Key: "suggestion", Value: "write the program to a file in several exec calls (cat >> file) and run the file, or give the runner mode file"})
		}
		if strings.ContainsRune(code, 0) {
			return invalid("invalid_code", "code contains a NUL byte, which an argument cannot carry")
		}
	}
	if stdin != "" && lang.Mode == sessionfs.NativeExecModeStdin {
		return invalid("input_unsupported", "runner "+name+" reads its code from stdin, so a call with input cannot use it",
			tools.Information{Key: "suggestion", Value: "set tools.native.exec.runners." + name + ".mode to arg or file"})
	}
	// A call without lang was given the default, so a failure may mean the code was
	// written for another interpreter (a model that assumes Python sends Python).
	failureSuggestion := ""
	if name == settings.DefaultLang {
		failureSuggestion = defaultLangSuggestion(name, settings.LangNames)
	}
	return execPlan{
		label:    shellLabel(execAsync, code),
		langName: name,
		lang:     lang,
		program:  shellProgram{name: name, setting: "tools.native.exec.runners." + name + ".argv", failureSuggestion: failureSuggestion},
		code:     code,
		stdin:    stdin,
		callID:   callID,
	}, ""
}
