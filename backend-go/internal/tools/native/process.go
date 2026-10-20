package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const processToolID = "native.process"

type processAction string

const (
	processActionStart  processAction = "start"
	processActionStatus processAction = "status"
	processActionList   processAction = "list"
	processActionWait   processAction = "wait"
	processActionStop   processAction = "stop"
)

var processInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["start","status","list","wait","stop"],"description":"Usually inferred from command or id."},
    "command":{"type":"string","minLength":1,"description":"Shell command to start."},
    "stdin":{"type":"string","description":"Text written once to the started command's standard input, e.g. a script for ` + "`python3 -` or `node -`" + `; the input is then closed."},
    "cwd":{"type":"string","minLength":1,"description":"Working directory for start. Defaults to the workspace."},
    "id":{"type":"string","minLength":1,"description":"Id returned by a prior start."},
    "filter":{"type":"string","enum":["running","exited","stopped","failed","lost"],"description":"State filter for list."},
    "timeout_ms":{"type":"integer","minimum":1,"description":"Maximum time wait blocks; clamped to session policy."}
  },
  "required":[],
  "additionalProperties":false,
  "allOf":[
    {"if":{"properties":{"action":{"const":"start"}},"required":["action"]},"then":{"properties":{"command":{}},"required":["command"]}},
    {"if":{"properties":{"action":{"enum":["status","wait","stop"]}},"required":["action"]},"then":{"properties":{"id":{}},"required":["id"]}}
  ]
}`)

// NewProcessTool binds normalized process policy to a shared process manager.
func NewProcessTool(settings sessionfs.NativeProcessSettings, manager ProcessManager, apiKeyEnv string) (tools.Tool, error) {
	validator, err := tools.CompileSchema(processInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native process schema: %w", err)
	}
	if manager == nil {
		manager = NewConfiguredProcessManager(settings)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          processToolID,
			Source:      "native",
			Name:        "process",
			Description: "Start, inspect, wait for, or stop a managed long-running process.",
			InputSchema: append(json.RawMessage(nil), processInputSchema...),
		},
		Validator:          validator,
		ExampleOperation:   json.RawMessage(`{"command":"npm run dev"}`),
		NormalizeArguments: normalizeProcessArguments,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeProcess(ctx, arguments, execution, settings, manager, apiKeyEnv)
		},
	}, nil
}

func normalizeProcessArguments(arguments map[string]any) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	_, suppliedFilter := arguments["filter"].(string)
	for _, key := range []string{"command", "cwd", "id", "filter"} {
		if value, ok := normalized[key].(string); ok {
			normalized[key] = strings.TrimSpace(value)
		}
	}
	aliases := map[string]string{
		"run": "start", "launch": "start", "state": "status", "info": "status", "kill": "stop", "terminate": "stop",
	}
	if action, ok := normalized["action"].(string); ok {
		action = strings.ToLower(strings.TrimSpace(action))
		if alias, exists := aliases[action]; exists {
			action = alias
		}
		normalized["action"] = action
	}
	if filter, ok := normalized["filter"].(string); ok {
		filter = strings.ToLower(strings.TrimSpace(filter))
		if filter == "all" || filter == "" {
			delete(normalized, "filter")
		} else {
			normalized["filter"] = filter
		}
	}
	if timeout, ok := safeInteger(normalized["timeout_ms"]); ok {
		if timeout < 1 {
			timeout = 1
		}
		normalized["timeout_ms"] = json.Number(fmt.Sprint(timeout))
	}
	// `input` is the other common name for text piped to a command; the named field wins.
	if _, hasStdin := normalized["stdin"]; !hasStdin {
		if input, ok := arguments["input"].(string); ok {
			normalized["stdin"] = input
		}
	}
	delete(normalized, "input")
	if _, hasAction := normalized["action"]; !hasAction && suppliedFilter {
		normalized["action"] = string(processActionList)
	}
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
	action := processActionFor(arguments)
	command := processText(arguments["command"])
	id := processText(arguments["id"])
	filter := processText(arguments["filter"])
	if !validProcessAction(action) {
		return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_action", "Process action is invalid"), nil
	}
	// Unlike the other text fields this one is not trimmed: leading and trailing
	// whitespace can be part of a script. An empty text is the same as none.
	stdin, _ := arguments["stdin"].(string)
	if stdin != "" && action != processActionStart {
		return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_stdin", "stdin is only accepted when starting a process"), nil
	}
	if action == processActionStart {
		if command == "" || id != "" {
			return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_start", "Start requires command and no process id"), nil
		}
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
		snapshot, err := manager.Start(ProcessStartRequest{
			CWD:         resolved.realPath,
			DisplayCWD:  resolved.displayPath,
			Command:     command,
			Stdin:       stdin,
			Policy:      settings,
			Environment: os.Environ(),
			APIKeyEnv:   apiKeyEnv,
			OwnerLimit:  int(execution.ProcessOwnerLimit),
		})
		if err != nil {
			return tools.ExecutionError(execution.CallID, tools.StatusError, "process_start_failed", err.Error()), nil
		}
		return renderProcessSnapshot(execution.CallID, action, snapshot, nil)
	}
	if action == processActionList {
		if command != "" || id != "" {
			return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_list", "List accepts no command or process id"), nil
		}
		return renderProcessList(execution.CallID, manager.List(filter))
	}
	if id == "" || command != "" || filter != "" {
		return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_target", fmt.Sprintf("%s requires a process id and no incompatible fields", action)), nil
	}
	stdoutLimits := ProcessStreamLimits{HeadBytes: int(settings.Stdout.HeadBytes), TailBytes: int(settings.Stdout.TailBytes)}
	stderrLimits := ProcessStreamLimits{HeadBytes: int(settings.Stderr.HeadBytes), TailBytes: int(settings.Stderr.TailBytes)}
	existing, found := manager.SnapshotWithLogs(id, stdoutLimits, stderrLimits, int(settings.MaxLineChars), settings.StripANSI)
	if !found || existing == nil {
		return processNotFound(execution.CallID, id, manager), nil
	}
	if action == processActionStatus {
		return renderProcessSnapshot(execution.CallID, action, *existing, nil)
	}
	if action == processActionWait {
		requested := settings.WaitTimeoutMs
		if timeout, ok := safeInteger(arguments["timeout_ms"]); ok {
			requested = timeout
		}
		timeoutMS := min(max(requested, 1), settings.MaxWaitTimeoutMs)
		// Wait only returns on exit or its timeout, so a stopped run would sit in it.
		result := make(chan ProcessWaitResult, 1)
		go func() { result <- manager.Wait(id, timeoutMS) }()
		var waited ProcessWaitResult
		select {
		case waited = <-result:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		snapshot, found := manager.SnapshotWithLogs(id, stdoutLimits, stderrLimits, int(settings.MaxLineChars), settings.StripANSI)
		if !found || snapshot == nil {
			return processNotFound(execution.CallID, id, manager), nil
		}
		return renderProcessSnapshot(execution.CallID, action, *snapshot, []tools.Information{{Key: "waited", Value: waited.Waited}})
	}
	if existing.State == ProcessLost {
		return tools.ExecutionError(execution.CallID, tools.StatusBlocked, "process_lost", "The process belongs to a previous backend instance and cannot be safely signaled", tools.Information{Key: "process_id", Value: id}), nil
	}
	_, found = manager.Stop(id, "stopped_by_agent")
	if !found {
		return processNotFound(execution.CallID, id, manager), nil
	}
	stopped, found := manager.SnapshotWithLogs(id, stdoutLimits, stderrLimits, int(settings.MaxLineChars), settings.StripANSI)
	if !found || stopped == nil {
		return processNotFound(execution.CallID, id, manager), nil
	}
	return renderProcessSnapshot(execution.CallID, action, *stopped, nil)
}

func processActionFor(arguments map[string]any) processAction {
	if action := processText(arguments["action"]); action != "" {
		return processAction(action)
	}
	if processText(arguments["command"]) != "" {
		return processActionStart
	}
	if processText(arguments["id"]) != "" {
		return processActionStatus
	}
	return processActionList
}

func validProcessAction(action processAction) bool {
	switch action {
	case processActionStart, processActionStatus, processActionList, processActionWait, processActionStop:
		return true
	default:
		return false
	}
}

func processText(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

// processNotFound lists the ids the manager knows, so the agent can pick one.
func processNotFound(callID, id string, manager ProcessManager) string {
	information := []tools.Information{{Key: "process_id", Value: id}}
	if known := manager.List(""); len(known) > 0 {
		ids := make([]string, 0, len(known))
		for _, process := range known {
			ids = append(ids, process.ID)
		}
		information = append(information, tools.Information{Key: "available_ids", Value: strings.Join(ids, ", ")})
	}
	return tools.ExecutionError(callID, tools.StatusNotFound, "process_not_found", "No managed process has that id", information...)
}

func renderProcessList(callID string, processes []ProcessSnapshot) (string, error) {
	result, err := tools.RenderToolResult(tools.ResultOptions{
		ID:      callID,
		Status:  tools.StatusOK,
		Summary: "Listed managed processes",
		Information: []tools.Information{
			{Key: "action", Value: string(processActionList)},
			{Key: "processes", Value: processes},
		},
	})
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "tool_output_failed", "The process list could not be rendered"), nil
	}
	return result, nil
}

func renderProcessSnapshot(callID string, action processAction, snapshot ProcessSnapshot, extra []tools.Information) (string, error) {
	information := []tools.Information{
		{Key: "action", Value: string(action)},
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
