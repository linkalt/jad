package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const processToolID = "native.process"

var processInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "command":{"type":"string","minLength":1,"description":"Shell command. It already runs in the background; do not add &, nohup or disown."},
    "stdin":{"type":"string","description":"Text written once to the command's standard input, e.g. a script for ` + "`python3 -` or `node -`" + `; the input is then closed."},
    "cwd":{"type":"string","minLength":1,"description":"Working directory. Defaults to the workspace."},
    "block_ms":{"type":"integer","minimum":0,"description":"Most time to block for the exit. If it is still running an id is returned; collect it with the async tool."}
  },
  "required":["command"],
  "additionalProperties":false
}`)

// NewProcessTool binds normalized process policy to a shared process manager.
func NewProcessTool(settings sessionfs.NativeProcessSettings, manager ProcessManager, apiKeyEnv string) (tools.Tool, error) {
	return NewProcessToolWithAsync(settings, manager, apiKeyEnv, nil)
}

// NewProcessToolWithAsync is NewProcessTool plus the async host that tracks every process
// the session starts as a handle (waited for, listed, inspected and stopped with the async
// tool). A nil host starts untracked processes and uses the default block times.
func NewProcessToolWithAsync(settings sessionfs.NativeProcessSettings, manager ProcessManager, apiKeyEnv string, host *AsyncHost) (tools.Tool, error) {
	validator, err := tools.CompileSchema(processInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native process schema: %w", err)
	}
	if manager == nil {
		manager = NewConfiguredProcessManager(settings)
	}
	if host != nil {
		if stopper, ok := manager.(processStopper); ok {
			host.attachProcess(stopper)
		}
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          processToolID,
			Source:      "native",
			Name:        "process",
			Description: processDescription,
			InputSchema: append(json.RawMessage(nil), processInputSchema...),
		},
		Validator:          validator,
		ExampleOperation:   json.RawMessage(`{"command":"npm run dev","block_ms":5000}`),
		NormalizeArguments: normalizeProcessArguments,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeProcessAsync(ctx, arguments, execution, settings, manager, apiKeyEnv, host)
		},
	}, nil
}

const processDescription = "Start a long-running shell command in the background and wait up to block_ms for it. If it finishes in time you get its exit code and output; otherwise you get an id and it keeps running: use the async tool to wait for, list, inspect or stop it. The command already runs in the background: do not use &, nohup, disown or daemonizing. Background children are killed when the command exits."

func normalizeProcessArguments(arguments map[string]any) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	for _, key := range []string{"command", "cwd"} {
		if value, ok := normalized[key].(string); ok {
			normalized[key] = strings.TrimSpace(value)
		}
	}
	if block, ok := safeInteger(normalized["block_ms"]); ok {
		normalized["block_ms"] = json.Number(fmt.Sprint(max(block, 0)))
	}
	// `input` is the other common name for text piped to a command; the named field wins.
	if _, hasStdin := normalized["stdin"]; !hasStdin {
		if input, ok := arguments["input"].(string); ok {
			normalized["stdin"] = input
		}
	}
	delete(normalized, "input")
	return normalized
}

func executeNativeProcess(
	ctx context.Context,
	arguments map[string]any,
	execution tools.ExecutionContext,
	settings sessionfs.NativeProcessSettings,
	manager ProcessManager,
	apiKeyEnv string,
) (string, error) {
	return executeNativeProcessAsync(ctx, arguments, execution, settings, manager, apiKeyEnv, nil)
}

func executeNativeProcessAsync(
	ctx context.Context,
	arguments map[string]any,
	execution tools.ExecutionContext,
	settings sessionfs.NativeProcessSettings,
	manager ProcessManager,
	apiKeyEnv string,
	host *AsyncHost,
) (string, error) {
	if manager == nil {
		return tools.ExecutionError(execution.CallID, tools.StatusError, "process_manager_unavailable", "The process manager is not available"), nil
	}
	if execution.ProcessOwner != "" {
		scoped, ok := scopeProcessManager(manager, execution.ProcessOwner)
		if !ok {
			return processOwnerUnavailable(execution.CallID), nil
		}
		manager = scoped
	}
	if err := manager.Recover(); err != nil {
		return tools.ExecutionError(execution.CallID, tools.StatusError, "process_recovery_failed", "Could not recover managed processes"), nil
	}
	command := processText(arguments["command"])
	if command == "" {
		return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_start", "Start requires a command"), nil
	}
	// Unlike the other text fields this one is not trimmed: leading and trailing
	// whitespace can be part of a script. An empty text is the same as none.
	stdin, _ := arguments["stdin"].(string)
	if stdinBytes := int64(len(stdin)); stdinBytes > settings.StdinMaxBytes {
		return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "stdin_too_large",
			fmt.Sprintf("stdin is %d bytes, over the limit of %d", stdinBytes, settings.StdinMaxBytes),
			tools.Information{Key: "suggestion", Value: "write the text to a file and run it from there"}), nil
	}
	requestedCWD := processText(arguments["cwd"])
	if requestedCWD == "" {
		requestedCWD = "."
	}
	resolved, pathError := resolveExistingTarget(requestedCWD, execution.WorkspaceRoot, settings.Policy, execution.CallID)
	if pathError != "" {
		return pathError, nil
	}
	info, err := os.Stat(resolved.realPath)
	if err != nil || !info.IsDir() {
		return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "not_a_directory", "The working directory is not a directory", tools.Information{Key: "path", Value: resolved.displayPath}), nil
	}
	// A child's processes (ProcessOwner) are handles owned by its run when it has the async tool
	// (ProcessAsync): only that tool sees them, and they are stopped when the run ends.
	var tracked ProcessAsync
	owner := ""
	config := asyncmgr.DefaultConfig()
	if host != nil {
		config = host.Config()
		owner = execution.SessionKey
		if execution.ProcessOwner != "" {
			owner = ""
			if execution.ProcessAsync {
				owner = execution.ProcessOwner
			}
		}
		if owner != "" {
			tracked = newProcessAsyncHandle(host, owner, command, settings.StorageDir)
		}
	}
	var blockMs *int64
	if value, ok := safeInteger(arguments["block_ms"]); ok {
		blockMs = &value
	}
	snapshot, err := manager.Start(ProcessStartRequest{
		CWD:         resolved.realPath,
		DisplayCWD:  resolved.displayPath,
		Command:     command,
		Stdin:       stdin,
		Policy:      settings,
		Environment: os.Environ(),
		APIKeyEnv:   apiKeyEnv,
		OwnerLimit:  int(execution.ProcessOwnerLimit),
		Async:       tracked,
	})
	if errors.Is(err, asyncmgr.ErrCapacity) {
		return tools.ExecutionError(execution.CallID, tools.StatusError, "async_capacity_reached", err.Error()), nil
	}
	if err != nil {
		return tools.ExecutionError(execution.CallID, tools.StatusError, "process_start_failed", err.Error()), nil
	}
	// The block ends the wait, never the process: a cancelled call leaves it running.
	if blockFor := config.BlockDuration(blockMs); blockFor > 0 && snapshot.State == ProcessRunning {
		waitProcess(ctx, manager, snapshot.ID, max(blockFor.Milliseconds(), 1))
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	stdoutLimits := ProcessStreamLimits{HeadBytes: int(settings.Stdout.HeadBytes), TailBytes: int(settings.Stdout.TailBytes)}
	stderrLimits := ProcessStreamLimits{HeadBytes: int(settings.Stderr.HeadBytes), TailBytes: int(settings.Stderr.TailBytes)}
	current, found := manager.SnapshotWithLogs(snapshot.ID, stdoutLimits, stderrLimits, int(settings.MaxLineChars), settings.StripANSI)
	if !found || current == nil {
		current = &snapshot
	}
	var extra []tools.Information
	if processTerminal(current.State) {
		// The result is in this response, so the signal must not be delivered again.
		if tracked != nil {
			host.Manager().MarkRead(owner, current.ID)
		}
	} else if tracked != nil {
		extra = append(extra, tools.Information{Key: "hint", Value: "still running; wait for it with the async tool in a later call, or stop it there"})
	}
	return renderProcessSnapshot(execution.CallID, "start", *current, extra)
}

func processText(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func renderProcessSnapshot(callID, action string, snapshot ProcessSnapshot, extra []tools.Information) (string, error) {
	information := []tools.Information{
		{Key: "action", Value: action},
		{Key: "process_id", Value: snapshot.ID},
		{Key: "state", Value: string(snapshot.State)},
		{Key: "cwd", Value: snapshot.CWD},
	}
	if snapshot.PID != 0 {
		information = append(information, tools.Information{Key: "pid", Value: snapshot.PID})
	}
	if value, exists := processManifestValue(snapshot.ExitCode); exists {
		information = append(information, tools.Information{Key: "exit_code", Value: value})
	}
	if value, exists := processManifestValue(snapshot.Signal); exists {
		information = append(information, tools.Information{Key: "signal", Value: value})
	}
	if snapshot.LeftRunning {
		information = append(information, tools.Information{Key: "left_running", Value: true})
	}
	information = append(information,
		tools.Information{Key: "stdout_produced_bytes", Value: snapshot.Output.stdout.producedBytes},
		tools.Information{Key: "stderr_produced_bytes", Value: snapshot.Output.stderr.producedBytes},
	)
	if snapshot.StdoutCapped {
		information = append(information, tools.Information{Key: "stdout_log_capped", Value: true})
	}
	if snapshot.StderrCapped {
		information = append(information, tools.Information{Key: "stderr_log_capped", Value: true})
	}
	information = append(information, extra...)
	status := tools.StatusOK
	if snapshot.State == ProcessFailed {
		status = tools.StatusError
	}
	options := tools.ResultOptions{
		ID:          callID,
		Status:      status,
		Summary:     fmt.Sprintf("Process %s is %s", snapshot.ID, snapshot.State),
		Information: information,
	}
	if snapshot.Output.text != "" {
		options.Content = &tools.Content{Text: snapshot.Output.text, Encoding: "utf8"}
	}
	result, err := tools.RenderToolResult(options)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "tool_output_failed", "The process snapshot could not be rendered"), nil
	}
	return result, nil
}

func processManifestValue(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	return value, true
}
