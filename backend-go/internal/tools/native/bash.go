package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
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
	resolved, pathError := resolveExistingTarget(args.cwd, execution.WorkspaceRoot, settings.Policy, execution.CallID)
	if pathError != "" {
		return pathError
	}
	info, err := os.Stat(resolved.realPath)
	if err != nil {
		return tools.ExecutionError(execution.CallID, tools.StatusError, "cwd_unavailable", "The working directory is not readable", tools.Information{Key: "path", Value: resolved.displayPath})
	}
	if !info.IsDir() {
		return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "not_a_directory", "The working directory is not a directory", tools.Information{Key: "path", Value: resolved.displayPath})
	}

	timeoutMs := effectiveBashTimeoutMs(args.timeoutMs, settings)
	spillPlan := planBashSpill(settings.Spill, execution.WorkspaceRoot, execution.SessionDir, execution.CallID)
	var spillWriters map[bashStream]bashSpillSink
	if spillPlan != nil {
		spillWriters = spillPlan.writers
	}
	commandArgs := []string{"-c", commandText}
	if settings.LoginShell {
		commandArgs = []string{"-l", "-c", commandText}
	}
	collector := newBashOutputCollector(
		bashStreamLimits{headBytes: int(settings.Stdout.HeadBytes), tailBytes: int(settings.Stdout.TailBytes)},
		bashStreamLimits{headBytes: int(settings.Stderr.HeadBytes), tailBytes: int(settings.Stderr.TailBytes)},
		int(settings.MaxLineChars),
		settings.StripANSI,
	)
	child := runBashCommand(ctx, bashChildRequest{
		file:                     settings.Shell,
		args:                     commandArgs,
		cwd:                      resolved.realPath,
		environment:              buildNativeBashEnvironment(os.Environ(), settings, apiKeyEnv),
		timeout:                  time.Duration(timeoutMs) * time.Millisecond,
		killGrace:                time.Duration(settings.KillGraceMs) * time.Millisecond,
		killProcessGroupWhenDone: settings.KillProcessGroup == "always",
		collector:                collector,
		spill:                    spillWriters,
		stdin:                    args.stdin,
	})
	spillDecision := applyBashSpillPolicy(settings.Spill, spillPlan, child, execution.WorkspaceRoot)
	return renderBashExecutionResult(execution.CallID, resolved.displayPath, settings.Shell, timeoutMs, child, spillDecision)
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
	denied := map[string]struct{}{"AGENT_API_TOKEN": {}}
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

func renderBashExecutionResult(callID, cwd, shell string, timeoutMs int64, result bashChildResult, spill bashSpillDecision) string {
	information := []tools.Information{{Key: "cwd", Value: cwd}}
	if result.leftRunning {
		information = append(information, tools.Information{Key: "left_running", Value: true})
	}
	content := (*tools.Content)(nil)
	if result.output.text != "" {
		content = &tools.Content{Text: result.output.text, Encoding: "utf8"}
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
		if unavailable {
			return render(tools.StatusError, "The shell is not available",
				tools.Information{Key: "error_type", Value: "execution"},
				tools.Information{Key: "error_code", Value: "shell_unavailable"},
				tools.Information{Key: "shell", Value: shell},
				tools.Information{Key: "suggestion", Value: "install the shell or point tools.native.bash.shell at an executable"},
			)
		}
		return render(tools.StatusError, "The command could not be started",
			tools.Information{Key: "error_type", Value: "execution"},
			tools.Information{Key: "error_code", Value: "spawn_failed"},
			tools.Information{Key: "shell", Value: shell},
			tools.Information{Key: "system_error", Value: result.spawnError.Error()},
		)
	}
	if result.timedOut {
		summary := fmt.Sprintf("Command timed out after %d ms", timeoutMs)
		if result.leftRunning {
			summary += " and survived SIGKILL"
		}
		suggestion := fmt.Sprintf("raise timeout_ms for this call or tools.native.bash.timeout_ms, or narrow the command")
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
		return render(tools.StatusError, "Command was killed by "+result.signal,
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
		return render(tools.StatusError, "Command exited with "+exitCode, fields...)
	}
	return render(tools.StatusOK, fmt.Sprintf("Command completed in %d ms", result.durationMs))
}
