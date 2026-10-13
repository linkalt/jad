package native

import (
	"context"
	"encoding/json"
	"strings"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

const (
	hookEventPreTool  = "pre_tool"
	hookEventPostTool = "post_tool"
)

// toolEventPayload is the JSON a pre_tool or post_tool command reads on stdin.
type toolEventPayload struct {
	Event            string          `json:"event"`
	Tool             string          `json:"tool"`
	ToolID           string          `json:"tool_id"`
	CallID           string          `json:"call_id"`
	Agent            string          `json:"agent,omitempty"`
	WorkspaceRoot    string          `json:"workspace_root"`
	Arguments        json.RawMessage `json:"arguments,omitempty"`
	ArgumentsOmitted bool            `json:"arguments_omitted,omitempty"`
	Status           string          `json:"status,omitempty"`
	Result           string          `json:"result,omitempty"`
	ResultCut        bool            `json:"result_cut,omitempty"`
}

// WithToolEventHooks returns tool unchanged unless a pre_tool or post_tool command covers
// it. Otherwise it returns a copy that runs the pre_tool commands before the call and the
// post_tool commands after it, and appends their report to the call's own result. A command
// never changes the status of the call, and a call that returns an error runs no post_tool
// command. It wraps Execute and ExecuteOutput alike, so native and MCP tools are covered.
func WithToolEventHooks(tool tools.Tool, hooks sessionfs.HooksSettings, apiKeyEnv string) tools.Tool {
	if !hooks.ToolEventsActive() {
		return tool
	}
	pre := toolEventHooksFor(hooks.PreTool, tool.Definition)
	post := toolEventHooksFor(hooks.PostTool, tool.Definition)
	if len(pre) == 0 && len(post) == 0 {
		return tool
	}
	fire := func(ctx context.Context, event string, list []sessionfs.ToolEventHook, arguments map[string]any, execution tools.ExecutionContext, result string) string {
		if len(list) == 0 {
			return ""
		}
		return fireToolEvent(ctx, hooks, apiKeyEnv, event, list, tool.Definition, arguments, execution, result)
	}
	if inner := tool.Execute; inner != nil {
		tool.Execute = func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			preReport := fire(ctx, hookEventPreTool, pre, arguments, execution, "")
			result, err := inner(ctx, arguments, execution)
			if err != nil {
				return result, err
			}
			report := joinHookReports(preReport, fire(ctx, hookEventPostTool, post, arguments, execution, result))
			if report == "" {
				return result, nil
			}
			return tools.AppendContent(result, tools.Content{Text: report})
		}
	}
	if inner := tool.ExecuteOutput; inner != nil {
		tool.ExecuteOutput = func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (json.RawMessage, error) {
			preReport := fire(ctx, hookEventPreTool, pre, arguments, execution, "")
			output, err := inner(ctx, arguments, execution)
			if err != nil {
				return output, err
			}
			report := joinHookReports(preReport, fire(ctx, hookEventPostTool, post, arguments, execution, string(output)))
			if report == "" {
				return output, nil
			}
			return appendOutputText(output, report), nil
		}
	}
	return tool
}

// WithToolEventHooksAll applies WithToolEventHooks to every tool of the list.
func WithToolEventHooksAll(list []tools.Tool, hooks sessionfs.HooksSettings, apiKeyEnv string) []tools.Tool {
	if !hooks.ToolEventsActive() {
		return list
	}
	wrapped := make([]tools.Tool, len(list))
	for index, tool := range list {
		wrapped[index] = WithToolEventHooks(tool, hooks, apiKeyEnv)
	}
	return wrapped
}

func toolEventHooksFor(list []sessionfs.ToolEventHook, definition tools.Definition) []sessionfs.ToolEventHook {
	var covered []sessionfs.ToolEventHook
	for _, hook := range list {
		if hook.Match == "*" || tools.EntryMatches(hook.Match, definition) {
			covered = append(covered, hook)
		}
	}
	return covered
}

func joinHookReports(reports ...string) string {
	var kept []string
	for _, report := range reports {
		if report != "" {
			kept = append(kept, report)
		}
	}
	return strings.Join(kept, "\n")
}

// fireToolEvent runs every command of list for one call, in order, and returns their report
// ("" when none had anything to say).
func fireToolEvent(ctx context.Context, hooks sessionfs.HooksSettings, apiKeyEnv, event string, list []sessionfs.ToolEventHook, definition tools.Definition, arguments map[string]any, execution tools.ExecutionContext, result string) string {
	payload := buildToolEventPayload(hooks, event, definition, arguments, execution, result)
	var sections []string
	for _, hook := range list {
		section := runHookChild(ctx, hooks, apiKeyEnv, execution.WorkspaceRoot, hookChild{
			name:          hook.Name,
			setting:       "hooks." + event + "[" + hook.Name + "].argv",
			argv:          hook.Argv,
			cwd:           execution.WorkspaceRoot,
			stdin:         payload,
			onFailureOnly: hook.OnFailureOnly,
		})
		if section != "" {
			sections = append(sections, section)
		}
	}
	if len(sections) == 0 {
		return ""
	}
	label := "pre-tool hooks for "
	if event == hookEventPostTool {
		label = "post-tool hooks for "
	}
	return label + definition.Name + "\n" + strings.Join(sections, "\n")
}

// buildToolEventPayload encodes the event within hooks.MaxPayloadBytes: arguments over half
// of it are left out, and the result is cut to the rest.
func buildToolEventPayload(hooks sessionfs.HooksSettings, event string, definition tools.Definition, arguments map[string]any, execution tools.ExecutionContext, result string) string {
	maximum := int(hooks.MaxPayloadBytes)
	payload := toolEventPayload{
		Event:         event,
		Tool:          definition.Name,
		ToolID:        definition.ID,
		CallID:        execution.CallID,
		Agent:         execution.AgentName,
		WorkspaceRoot: execution.WorkspaceRoot,
	}
	if encoded, err := json.Marshal(arguments); err == nil && len(encoded) <= maximum/2 {
		payload.Arguments = encoded
	} else {
		payload.ArgumentsOmitted = true
	}
	if event == hookEventPostTool {
		payload.Status = resultStatus(result)
		payload.Result, payload.ResultCut = cutUTF8(result, int64(maximum/2))
	}
	for {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return ""
		}
		// JSON escapes (\u003c for <, \n for a newline) make the text longer than the
		// bytes counted above, so the result is cut again until the whole event fits.
		if len(encoded) <= maximum {
			return string(encoded)
		}
		if payload.Result == "" {
			if payload.Arguments == nil {
				return string(encoded)
			}
			payload.Arguments, payload.ArgumentsOmitted = nil, true
			continue
		}
		smaller := int64(len(payload.Result)) * int64(maximum) / int64(len(encoded))
		if smaller >= int64(len(payload.Result)) {
			smaller = int64(len(payload.Result)) - 1
		}
		payload.Result, _ = cutUTF8(payload.Result, smaller)
		payload.ResultCut = true
	}
}

// resultStatus reads the status of a native tool result, whose first line is a JSON header.
func resultStatus(result string) string {
	line, _, _ := strings.Cut(result, "\n")
	var header struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(line), &header) != nil {
		return ""
	}
	return header.Status
}

// appendOutputText adds a text part to a tool output that is a list of parts. Any other
// shape is returned as it was, so a report can never corrupt an output.
func appendOutputText(output json.RawMessage, text string) json.RawMessage {
	var parts []json.RawMessage
	if json.Unmarshal(output, &parts) != nil || parts == nil {
		return output
	}
	extra, err := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "input_text", Text: text})
	if err != nil {
		return output
	}
	encoded, err := json.Marshal(append(parts, extra))
	if err != nil {
		return output
	}
	return encoded
}
