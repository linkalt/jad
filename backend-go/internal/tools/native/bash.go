package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const bashToolID = "native.bash"

var bashInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "command":{"type":"string","minLength":1,"description":"Shell command line, executed with the configured shell."},
    "stdin":{"type":"string","description":"Text piped to the command's standard input, e.g. a script for ` + "`python3 -` or `node -`" + `, or data for ` + "`jq`" + `."},
    "cwd":{"type":"string","minLength":1,"description":"Directory to run in, relative to the workspace root or absolute. Defaults to the workspace root."},
    "timeout_ms":{"type":"integer","description":"Wall-clock budget for this call in milliseconds; clamped to the configured range."}
  },
  "required":["command"],
  "additionalProperties":false
}`)

var bashEnvironmentAllowlist = []string{
	"PATH", "HOME", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "TMP", "TEMP",
	"SYSTEMROOT", "WINDIR", "ComSpec", "PATHEXT",
}

// NewBashTool constructs native.bash from normalized per-session settings.
func NewBashTool(settings sessionfs.NativeBashSettings, apiKeyEnv string) (tools.Tool, error) {
	validator, err := tools.CompileSchema(bashInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native bash schema: %w", err)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          bashToolID,
			Source:      "native",
			Name:        "bash",
			Description: "Run a bounded shell command. Use file tools for file operations.",
			InputSchema: append(json.RawMessage(nil), bashInputSchema...),
		},
		Validator:          validator,
		ExampleOperation:   json.RawMessage(`{"command":"npm test"}`),
		NormalizeArguments: normalizeBashArguments,
		LeafBudget:         true,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeBash(ctx, arguments, settings, apiKeyEnv, execution), nil
		},
	}, nil
}

func normalizeBashArguments(arguments map[string]any) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	if command, ok := normalized["command"].(string); ok {
		normalized["command"] = strings.TrimSpace(command)
	} else if command, ok := arguments["cmd"].(string); ok {
		normalized["command"] = strings.TrimSpace(command)
	}
	if _, hasCwd := normalized["cwd"]; !hasCwd {
		alias, exists := arguments["workdir"]
		if !exists || alias == nil {
			alias = arguments["dir"]
		}
		if cwd, ok := alias.(string); ok {
			normalized["cwd"] = cwd
		}
	}
	// `input` is the other common name for text piped to a command.
	if _, hasStdin := normalized["stdin"]; !hasStdin {
		if input, ok := arguments["input"].(string); ok {
			normalized["stdin"] = input
		}
	}
	delete(normalized, "cmd")
	delete(normalized, "workdir")
	delete(normalized, "dir")
	delete(normalized, "input")
	return normalized
}

type bashArguments struct {
	command   string
	stdin     string
	cwd       string
	timeoutMs *int64
}

func parseBashArguments(arguments map[string]any) bashArguments {
	parsed := bashArguments{cwd: "."}
	parsed.command, _ = arguments["command"].(string)
	parsed.stdin, _ = arguments["stdin"].(string)
	if cwd, ok := arguments["cwd"].(string); ok {
		parsed.cwd = cwd
	}
	if timeout, ok := bashArgumentInteger(arguments["timeout_ms"]); ok {
		parsed.timeoutMs = &timeout
	}
	return parsed
}

func executeNativeBash(
	ctx context.Context,
	arguments map[string]any,
	settings sessionfs.NativeBashSettings,
	apiKeyEnv string,
	execution tools.ExecutionContext,
) string {
	args := parseBashArguments(arguments)
	commandText := strings.TrimSpace(args.command)
	if commandText == "" {
		return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_command", "The command is empty")
	}
	// An empty stdin is the same as none: a closed input reads as empty anyway.
	if stdinBytes := int64(len(args.stdin)); stdinBytes > settings.StdinMaxBytes {
		return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "stdin_too_large",
			fmt.Sprintf("stdin is %d bytes, over the limit of %d", stdinBytes, settings.StdinMaxBytes),
			tools.Information{Key: "suggestion", Value: "write the text to a file and run it from there"})
	}
	commandArgs := []string{"-c", commandText}
	if settings.LoginShell {
		commandArgs = []string{"-l", "-c", commandText}
	}
	return runShellChild(ctx, shellChildRun{
		file:  settings.Shell,
		argv:  commandArgs,
		stdin: args.stdin,
		cwd:   args.cwd,
		// timeoutMs is applied by runShellChild from the same argument.
		timeoutMs: args.timeoutMs,
	}, settings, apiKeyEnv, execution)
}

// shellChildRun is what differs between the tools that run a child under bash's policy.
type shellChildRun struct {
	file      string
	argv      []string
	stdin     string
	cwd       string
	timeoutMs *int64
	// lineAware cuts a truncated stream at line boundaries.
	lineAware bool
	// extraEnv is added after the allowlist and denials were applied.
	extraEnv map[string]string
	// prependEnv puts a directory in front of the list variable of that name (PATH, PYTHONPATH, ...),
	// keeping the value the environment already has.
	prependEnv map[string]string
	// resultFile is a file the program may write its answer to; its text, cut to maxBytes,
	// is shown ahead of the raw output, which is then labeled [output].
	resultFile *shellResultFile
	// program names the interpreter in the "not installed" error; nil means the shell.
	program *shellProgram
	// section is the settings block the run reads, named in advice: tools.native.bash or tools.native.exec.
	section string
	// standby is a waiting interpreter (exec) that runs standbyCode instead of a fresh start of
	// file and argv; file and argv still start the program when the standby declines it.
	standby     *standbyProcess
	standbyCode string
}

type shellResultFile struct {
	path     string
	maxBytes int64
	// cutMarker adds a visible line with the cut counts to an answer that was cut.
	cutMarker bool
}

type shellProgram struct {
	name    string
	setting string
	// failureSuggestion is advice added to a non-zero exit, when the caller knows a likely cause.
	failureSuggestion string
	// subject names what ran in summaries ("Command" when empty).
	subject string
}

// settingsSection is the block advice points the user at; a run that names none is bash's.
func (run shellChildRun) settingsSection() string {
	if run.section == "" {
		return "tools.native.bash"
	}
	return run.section
}

// runShellChild runs one child in a resolved working directory under the settings it is
// given (timeout, spill, stream budgets, environment) and renders the result. bash and exec
// each pass their own and share it, so the two cannot drift in how output is bounded or reported.
func runShellChild(
	ctx context.Context,
	run shellChildRun,
	settings sessionfs.NativeBashSettings,
	apiKeyEnv string,
	execution tools.ExecutionContext,
) string {
	return runShellChildDetailed(ctx, run, settings, apiKeyEnv, execution).text
}

// shellRunResult is a rendered shell child result with what it was made from. child and
// answer are nil when the run failed before the child started.
type shellRunResult struct {
	text   string
	child  *bashChildResult
	answer *shellResult
}

func runShellChildDetailed(
	ctx context.Context,
	run shellChildRun,
	settings sessionfs.NativeBashSettings,
	apiKeyEnv string,
	execution tools.ExecutionContext,
) shellRunResult {
	// A standby the call does not reach stops here; one it reaches is the run's from then on.
	standbyHandedOver := false
	defer func() {
		if run.standby != nil && !standbyHandedOver {
			run.standby.stop()
		}
	}()
	resolved, pathError := resolveExistingTarget(run.cwd, execution.WorkspaceRoot, settings.Policy, execution.CallID)
	if pathError != "" {
		return shellRunResult{text: pathError}
	}
	info, err := os.Stat(resolved.realPath)
	if err != nil {
		return shellRunResult{text: tools.ExecutionError(execution.CallID, tools.StatusError, "cwd_unavailable", "The working directory is not readable", tools.Information{Key: "path", Value: resolved.displayPath})}
	}
	if !info.IsDir() {
		return shellRunResult{text: tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "not_a_directory", "The working directory is not a directory", tools.Information{Key: "path", Value: resolved.displayPath})}
	}

	timeoutMs := effectiveBashTimeoutMs(run.timeoutMs, settings)
	spillPlan := planBashSpill(settings.Spill, execution.WorkspaceRoot, execution.SessionDir, execution.CallID)
	var spillWriters map[bashStream]bashSpillSink
	if spillPlan != nil {
		spillWriters = spillPlan.writers
	}
	collector := newBashOutputCollector(
		bashStreamLimits{headBytes: int(settings.Stdout.HeadBytes), tailBytes: int(settings.Stdout.TailBytes)},
		bashStreamLimits{headBytes: int(settings.Stderr.HeadBytes), tailBytes: int(settings.Stderr.TailBytes)},
		int(settings.MaxLineChars),
		settings.StripANSI,
	)
	collector.lineAware = run.lineAware
	if watch := outputWatchFrom(ctx); watch != nil {
		watch.set(collector)
	}
	environment := shellChildEnvironment(run, settings, apiKeyEnv)
	request := bashChildRequest{
		file:                     run.file,
		args:                     run.argv,
		cwd:                      resolved.realPath,
		environment:              environment,
		timeout:                  time.Duration(timeoutMs) * time.Millisecond,
		killGrace:                time.Duration(settings.KillGraceMs) * time.Millisecond,
		killDeadline:             time.Duration(settings.KillDeadlineMs) * time.Millisecond,
		drainAfterExit:           time.Duration(settings.DrainAfterExitMs) * time.Millisecond,
		killProcessGroupWhenDone: settings.KillProcessGroup == "always",
		sweepAtRunEnd:            settings.KillChildrenOnRunEnd,
		collector:                collector,
		spill:                    spillWriters,
		stdin:                    run.stdin,
	}
	var child bashChildResult
	if run.standby != nil {
		standbyHandedOver = true
		child = runStandbyCommand(ctx, request, run.standby, run.standbyCode)
	} else {
		child = runBashCommand(ctx, request)
	}
	spillDecision := applyBashSpillPolicy(settings.Spill, spillPlan, child, execution.WorkspaceRoot)
	var answer *shellResult
	if run.resultFile != nil {
		answer = readShellResultFile(run.resultFile.path, run.resultFile.maxBytes)
		if answer != nil && answer.cutBytes > 0 && run.resultFile.cutMarker {
			answer.text += fmt.Sprintf("\n[result cut: %d bytes omitted after %d bytes]", answer.cutBytes, len(answer.text))
			answer.marked = true
		}
	}
	text := renderBashExecutionResult(execution.CallID, resolved.displayPath, run.file, timeoutMs, child, spillDecision, answer, run.program, run.settingsSection())
	return shellRunResult{text: text, child: &child, answer: answer}
}

// shellChildEnvironment is the environment a run's child gets: the process environment under
// the settings' allowlist and denials, then the run's own variables and list prefixes.
func shellChildEnvironment(run shellChildRun, settings sessionfs.NativeBashSettings, apiKeyEnv string) []string {
	environment := buildNativeBashEnvironment(os.Environ(), settings, apiKeyEnv)
	for _, name := range sortedKeys(run.extraEnv) {
		environment = append(environment, name+"="+run.extraEnv[name])
	}
	for _, name := range sortedKeys(run.prependEnv) {
		environment = prependEnvironmentList(environment, name, run.prependEnv[name])
	}
	return environment
}

func sortedKeys(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// shellResult is the answer a program wrote to its result file.
type shellResult struct {
	text     string
	cutBytes int64
	// marked is set when the text carries a cut marker, so the result also says truncated.
	marked bool
}

// readShellResultFile reads the answer, cut to maxBytes without splitting a character and
// with trailing whitespace removed. A missing, empty or blank file is no answer.
func readShellResultFile(path string, maxBytes int64) *shellResult {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return nil
	}
	// Read the cap plus one character's worth of bytes: enough to find a character
	// boundary at the cut, without loading a file the program made far larger.
	want := info.Size()
	if want > maxBytes+utf8.UTFMax {
		want = maxBytes + utf8.UTFMax
	}
	data := make([]byte, want)
	read, err := io.ReadFull(file, data)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	data = data[:read]
	end := len(data)
	if int64(end) > maxBytes {
		end = int(maxBytes)
	}
	for end > 0 && end < len(data) && !utf8.RuneStart(data[end]) {
		end--
	}
	text := strings.TrimRightFunc(string(data[:end]), unicode.IsSpace)
	if text == "" {
		return nil
	}
	return &shellResult{text: text, cutBytes: info.Size() - int64(end)}
}

func effectiveBashTimeoutMs(requested *int64, settings sessionfs.NativeBashSettings) int64 {
	if requested == nil {
		return settings.TimeoutMs
	}
	floor := sessionfs.NativeBashMinCallTimeoutMs
	if floor > settings.MaxTimeoutMs {
		floor = settings.MaxTimeoutMs
	}
	if *requested < floor {
		return floor
	}
	if *requested > settings.MaxTimeoutMs {
		return settings.MaxTimeoutMs
	}
	return *requested
}

func bashArgumentInteger(value any) (int64, bool) {
	const maxSafeInteger = int64(9_007_199_254_740_991)
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil && parsed >= -maxSafeInteger && parsed <= maxSafeInteger
	case int:
		return int64(number), true
	case int64:
		return number, number >= -maxSafeInteger && number <= maxSafeInteger
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number < -float64(maxSafeInteger) || number > float64(maxSafeInteger) {
			return 0, false
		}
		return int64(number), true
	case string:
		parsed, err := strconv.ParseInt(number, 10, 64)
		return parsed, err == nil && parsed >= -maxSafeInteger && parsed <= maxSafeInteger
	default:
		return 0, false
	}
}

func buildNativeBashEnvironment(base []string, settings sessionfs.NativeBashSettings, apiKeyEnv string) []string {
	denied := map[string]struct{}{"JAD_API_TOKEN": {}}
	if apiKeyEnv != "" {
		denied[apiKeyEnv] = struct{}{}
	}
	for _, name := range settings.EnvDeny {
		denied[name] = struct{}{}
	}
	allowed := make(map[string]struct{}, len(bashEnvironmentAllowlist))
	for _, name := range bashEnvironmentAllowlist {
		allowed[name] = struct{}{}
	}
	values := make(map[string]string, len(allowed)+len(settings.Env))
	for _, item := range base {
		name, value, found := strings.Cut(item, "=")
		if !found {
			continue
		}
		if _, ok := allowed[name]; !ok {
			continue
		}
		if _, isDenied := denied[name]; !isDenied {
			values[name] = value
		}
	}
	for name, value := range settings.Env {
		if _, isDenied := denied[name]; !isDenied {
			values[name] = value
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	environment := make([]string, 0, len(names))
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}
	return environment
}

func renderBashExecutionResult(callID, cwd, shell string, timeoutMs int64, result bashChildResult, spill bashSpillDecision, answer *shellResult, program *shellProgram, section string) string {
	information := []tools.Information{{Key: "cwd", Value: cwd}}
	if result.leftRunning {
		information = append(information, tools.Information{Key: "left_running", Value: true})
	}
	contentText := result.output.text
	if answer != nil {
		if contentText != "" {
			contentText = "[result]\n" + answer.text + "\n[output]\n" + contentText
		} else {
			contentText = "[result]\n" + answer.text
		}
	}
	content := (*tools.Content)(nil)
	if contentText != "" {
		content = &tools.Content{Text: contentText, Encoding: "utf8"}
	}
	contentStream := ""
	if result.output.stdout.text != "" && result.output.stderr.text != "" {
		contentStream = "mixed"
	} else if result.output.stderr.text != "" {
		contentStream = "stderr"
	} else if result.output.stdout.text != "" {
		contentStream = "stdout"
	}
	truncated := result.output.truncated || result.timedOut
	measured := make([]tools.Information, 0, 14)
	if result.hasExitCode && result.exitCode != 0 {
		measured = append(measured, tools.Information{Key: "exit_code", Value: result.exitCode})
	}
	if result.signal != "" {
		measured = append(measured, tools.Information{Key: "signal", Value: result.signal})
	}
	if result.timedOut {
		measured = append(measured, tools.Information{Key: "timed_out", Value: true})
	}
	if contentStream != "" && truncated {
		measured = append(measured, tools.Information{Key: "content_stream", Value: contentStream})
	}
	if truncated {
		measured = append(measured, tools.Information{Key: "truncated", Value: true})
		reason := "max_bytes"
		if result.timedOut {
			reason = "timeout"
		}
		measured = append(measured, tools.Information{Key: "truncation_reason", Value: reason})
	}
	if result.output.omittedBytes > 0 {
		measured = append(measured, tools.Information{Key: "omitted_bytes", Value: result.output.omittedBytes})
	}
	if spill.stdout != nil {
		measured = append(measured, tools.Information{Key: "stdout_full", Value: spill.stdout.display})
		if spill.stdout.capped {
			measured = append(measured, tools.Information{Key: "stdout_full_capped", Value: true})
		}
	}
	if spill.stderr != nil {
		measured = append(measured, tools.Information{Key: "stderr_full", Value: spill.stderr.display})
		if spill.stderr.capped {
			measured = append(measured, tools.Information{Key: "stderr_full_capped", Value: true})
		}
	}
	if spill.err != "" {
		measured = append(measured, tools.Information{Key: "spill_error", Value: spill.err})
	}
	if spill.hint != "" {
		measured = append(measured, tools.Information{Key: "hint", Value: spill.hint})
	}
	if result.output.binary {
		measured = append(measured, tools.Information{Key: "binary_output", Value: true})
	}
	if answer != nil && answer.cutBytes > 0 {
		measured = append(measured, tools.Information{Key: "result_truncated_bytes", Value: answer.cutBytes})
	}
	if answer != nil && answer.marked && !truncated {
		measured = append(measured,
			tools.Information{Key: "truncated", Value: true},
			tools.Information{Key: "truncation_reason", Value: "max_bytes"})
	}

	subject := "Command"
	if program != nil && program.subject != "" {
		subject = program.subject
	}
	render := func(status tools.Status, summary string, errorFields ...tools.Information) string {
		items := append([]tools.Information(nil), errorFields...)
		items = append(items, information...)
		items = append(items, measured...)
		text, err := tools.RenderToolResult(tools.ResultOptions{ID: callID, Status: status, Summary: summary, Information: items, Content: content})
		if err != nil {
			return ""
		}
		return text
	}
	if result.spawnError != nil {
		unavailable := errors.Is(result.spawnError, exec.ErrNotFound) || errors.Is(result.spawnError, os.ErrNotExist)
		// An interpreter is named "interpreter" and the shell "shell", so each message
		// points at the setting that would fix it.
		programKey := "shell"
		if program != nil {
			programKey = "interpreter"
		}
		if unavailable {
			summary, code := "The shell is not available", "shell_unavailable"
			suggestion := "install the shell or point tools.native.bash.shell at an executable"
			if program != nil {
				summary, code = program.name+" is not available", "interpreter_unavailable"
				suggestion = "install " + program.name + " or point " + program.setting + " at it"
				if program.setting == "" {
					suggestion = "install " + program.name + " or check its path"
				}
			}
			return render(tools.StatusError, summary,
				tools.Information{Key: "error_type", Value: "execution"},
				tools.Information{Key: "error_code", Value: code},
				tools.Information{Key: programKey, Value: shell},
				tools.Information{Key: "suggestion", Value: suggestion},
			)
		}
		return render(tools.StatusError, "The command could not be started",
			tools.Information{Key: "error_type", Value: "execution"},
			tools.Information{Key: "error_code", Value: "spawn_failed"},
			tools.Information{Key: programKey, Value: shell},
			tools.Information{Key: "system_error", Value: result.spawnError.Error()},
		)
	}
	if result.timedOut {
		summary := fmt.Sprintf("%s timed out after %d ms", subject, timeoutMs)
		if result.leftRunning {
			summary += " and survived SIGKILL"
		}
		suggestion := "raise timeout_ms for this call or " + section + ".timeout_ms, or narrow the command"
		if result.leftRunning {
			suggestion = "the process is blocked in the kernel and was left to the operating system; nothing of it can be reached from here"
		}
		return render(tools.StatusError, summary,
			tools.Information{Key: "error_type", Value: "execution"},
			tools.Information{Key: "error_code", Value: "timeout"},
			tools.Information{Key: "timeout_ms", Value: timeoutMs},
			tools.Information{Key: "suggestion", Value: suggestion},
		)
	}
	if result.signal != "" {
		return render(tools.StatusError, subject+" was killed by "+result.signal,
			tools.Information{Key: "error_type", Value: "execution"},
			tools.Information{Key: "error_code", Value: "signaled"},
		)
	}
	if !result.hasExitCode || result.exitCode != 0 {
		exitCode := "null"
		if result.hasExitCode {
			exitCode = strconv.Itoa(result.exitCode)
		}
		fields := []tools.Information{
			{Key: "error_type", Value: "execution"},
			{Key: "error_code", Value: "nonzero_exit"},
		}
		if result.hasExitCode && (result.exitCode == 126 || result.exitCode == 127) {
			fields = append(fields, tools.Information{Key: "hint", Value: "exit code 127 means the command was not found; 126 means it is not executable"})
		}
		if program != nil && program.failureSuggestion != "" {
			fields = append(fields, tools.Information{Key: "suggestion", Value: program.failureSuggestion})
		}
		return render(tools.StatusError, subject+" exited with "+exitCode, fields...)
	}
	return render(tools.StatusOK, fmt.Sprintf("%s completed in %d ms", subject, result.durationMs))
}

// prependEnvironmentList sets name to dir followed by the list separator and the value name
// already has; with no value it is just dir.
func prependEnvironmentList(environment []string, name, dir string) []string {
	for index := len(environment) - 1; index >= 0; index-- {
		if existing, found := strings.CutPrefix(environment[index], name+"="); found {
			if existing != "" {
				dir += string(os.PathListSeparator) + existing
			}
			break
		}
	}
	return append(environment, name+"="+dir)
}
