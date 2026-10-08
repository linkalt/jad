package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

const subagentToolID = "native.subagent"

var subagentInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "task":{"type":"string","minLength":1,"description":"Self-contained instructions. The agent sees nothing of this conversation."},
    "agent":{"type":"string","description":"Agent name. Optional when only one is configured."}
  },
  "required":["task"],
  "additionalProperties":false
}`)

const subagentDescription = "Hand a task to a separate read-only agent with its own fresh context and get back only its final answer. Use it for searches or reading that would fill your context."

// subagentExample is shown to the model when its arguments are rejected.
var subagentExample = json.RawMessage(`{"task":"Find where the retry delay is computed and report the file paths and line numbers."}`)

// SubagentRequest is one child run: the agent to start, its task, and the tools it may call.
type SubagentRequest struct {
	Agent sessionfs.NativeSubagentAgent
	Task  string
	// CallID is the parent's tool call, used to name the child's transcript.
	CallID string
	// Tools is the child's catalog: read-only native tools only, never the subagent tool.
	Tools *tools.Catalog
	// Execution carries the workspace roots the child's tools resolve paths against.
	Execution tools.ExecutionContext
}

// SubagentResult is what a finished child run reports.
type SubagentResult struct {
	// Text is the child's final answer.
	Text string
	// Rounds is the number of provider responses the child used.
	Rounds int
	// Tokens is the usage the provider reported for the child, summed over its rounds.
	Tokens int64
}

// SubagentLauncher runs one child to completion. The app package implements it,
// because starting a run needs the model, credential and transcript wiring that the
// tool package does not know. ctx ends when the parent run stops or the agent's
// timeout passes; the launcher must stop promptly then.
type SubagentLauncher interface {
	Run(ctx context.Context, request SubagentRequest) (SubagentResult, error)
}

type subagentAgent struct {
	config  sessionfs.NativeSubagentAgent
	catalog *tools.Catalog
}

type subagentTool struct {
	settings sessionfs.NativeSubagentSettings
	agents   []subagentAgent
	launcher SubagentLauncher
	// slots bounds the children running at once. The catalog is built per run, so
	// the bound applies within one run.
	slots chan struct{}
}

// NewSubagentTool builds the subagent tool over the already built tools of the same
// catalog. A child may use only the read-only ones, optionally narrowed by its
// agent's tools list, and never the subagent tool itself, so the depth is one.
func NewSubagentTool(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, launcher SubagentLauncher) (tools.Tool, error) {
	validator, err := tools.CompileSchema(subagentInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native subagent schema: %w", err)
	}
	if len(settings.Agents) == 0 {
		return tools.Tool{}, errors.New("tools.native.subagent needs at least one agent")
	}
	if settings.MaxConcurrent < 1 {
		return tools.Tool{}, errors.New("tools.native.subagent.max_concurrent must be at least 1")
	}
	readOnly := make([]tools.Tool, 0, len(parentTools))
	for _, tool := range parentTools {
		if tool.ReadOnly && tool.Definition.Source == "native" && tool.Definition.ID != subagentToolID {
			readOnly = append(readOnly, tool)
		}
	}
	runtime := &subagentTool{settings: settings, launcher: launcher, slots: make(chan struct{}, settings.MaxConcurrent)}
	for _, agent := range settings.Agents {
		childTools, err := subagentChildTools(agent, readOnly)
		if err != nil {
			return tools.Tool{}, err
		}
		catalog, err := tools.NewCatalog(childTools)
		if err != nil {
			return tools.Tool{}, fmt.Errorf("tools.native.subagent.agents.%s: %w", agent.Name, err)
		}
		runtime.agents = append(runtime.agents, subagentAgent{config: agent, catalog: catalog})
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          subagentToolID,
			Source:      "native",
			Name:        "subagent",
			Description: subagentDescription + subagentAgentList(settings.Agents),
			InputSchema: append(json.RawMessage(nil), subagentInputSchema...),
		},
		Validator:        validator,
		ExampleOperation: subagentExample,
		// The child cannot change files and the tool keeps its own state outside the
		// workspace, so several calls of one response may run side by side.
		ReadOnly:           true,
		NormalizeArguments: normalizeSubagentArguments,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return runtime.execute(ctx, arguments, execution)
		},
	}, nil
}

// subagentAgentList returns the agent list appended to a manifest description, so
// the model sees which agents exist without an extra call.
func subagentAgentList(agents []sessionfs.NativeSubagentAgent) string {
	var builder strings.Builder
	builder.WriteString("\nAgents:")
	for _, agent := range agents {
		builder.WriteString("\n- ")
		builder.WriteString(agent.Name)
		if agent.Description != "" {
			builder.WriteString(": ")
			builder.WriteString(agent.Description)
		}
	}
	return builder.String()
}

// subagentChildTools picks the tools one agent may use from the enabled read-only ones.
func subagentChildTools(agent sessionfs.NativeSubagentAgent, readOnly []tools.Tool) ([]tools.Tool, error) {
	available := make([]string, 0, len(readOnly))
	byName := make(map[string]tools.Tool, len(readOnly))
	for _, tool := range readOnly {
		available = append(available, tool.Definition.Name)
		byName[tool.Definition.Name] = tool
	}
	sort.Strings(available)
	chosen := readOnly
	if agent.HasTools {
		wanted := make(map[string]struct{}, len(agent.Tools))
		for _, name := range agent.Tools {
			if _, exists := byName[name]; !exists {
				return nil, fmt.Errorf("tools.native.subagent.agents.%s.tools lists %q, which is not an enabled read-only native tool (enabled: %s)", agent.Name, name, strings.Join(available, ", "))
			}
			wanted[name] = struct{}{}
		}
		chosen = make([]tools.Tool, 0, len(wanted))
		for _, tool := range readOnly {
			if _, ok := wanted[tool.Definition.Name]; ok {
				chosen = append(chosen, tool)
			}
		}
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("tools.native.subagent.agents.%s has no tool to use: enable at least one read-only native tool (read, grep, glob, fetch or skill)", agent.Name)
	}
	return chosen, nil
}

// normalizeSubagentArguments treats a blank agent as absent, so a model that sends an
// empty string for "the default" is not rejected.
func normalizeSubagentArguments(arguments map[string]any) map[string]any {
	if name, ok := arguments["agent"].(string); ok {
		if trimmed := strings.TrimSpace(name); trimmed == "" {
			delete(arguments, "agent")
		} else {
			arguments["agent"] = trimmed
		}
	}
	return arguments
}

func (s *subagentTool) selectAgent(arguments map[string]any) (subagentAgent, string) {
	name, named := arguments["agent"].(string)
	if !named {
		if len(s.agents) == 1 {
			return s.agents[0], ""
		}
		return subagentAgent{}, "Several agents are configured; name one in agent."
	}
	for _, agent := range s.agents {
		if agent.config.Name == name {
			return agent, ""
		}
	}
	return subagentAgent{}, fmt.Sprintf("No agent named %q.", name)
}

func (s *subagentTool) agentNames() string {
	names := make([]string, 0, len(s.agents))
	for _, agent := range s.agents {
		names = append(names, agent.config.Name)
	}
	return strings.Join(names, ", ")
}

func (s *subagentTool) execute(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
	callID := execution.CallID
	task, _ := arguments["task"].(string)
	task = strings.TrimSpace(task)
	if task == "" {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input", "task must not be blank"), nil
	}
	agent, problem := s.selectAgent(arguments)
	if problem != "" {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "unknown_agent", problem,
			tools.Information{Key: "available", Value: s.agentNames()}), nil
	}
	if s.launcher == nil {
		return tools.ExecutionError(callID, tools.StatusError, "subagent_unavailable", "Sub-agent runs are not available in this session"), nil
	}

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	childContext, cancel := context.WithTimeout(ctx, time.Duration(agent.config.TimeoutMS)*time.Millisecond)
	defer cancel()
	result, err := s.launcher.Run(childContext, SubagentRequest{
		Agent:  agent.config,
		Task:   task,
		CallID: callID,
		Tools:  agent.catalog,
		Execution: tools.ExecutionContext{
			WorkspaceRoot: execution.WorkspaceRoot,
			SessionDir:    execution.SessionDir,
		},
	})
	if err != nil {
		// The parent stopping is not a sub-agent failure; let the runner record the stop.
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if childContext.Err() != nil {
			return tools.ExecutionError(callID, tools.StatusError, "subagent_timeout",
				fmt.Sprintf("Agent %s did not finish within %d ms", agent.config.Name, agent.config.TimeoutMS),
				tools.Information{Key: "agent", Value: agent.config.Name}), nil
		}
		return tools.ExecutionError(callID, tools.StatusError, "subagent_failed",
			fmt.Sprintf("Agent %s failed: %s", agent.config.Name, boundedReason(err.Error())),
			tools.Information{Key: "agent", Value: agent.config.Name}), nil
	}
	text := strings.TrimSpace(result.Text)
	if text == "" {
		return tools.ExecutionError(callID, tools.StatusError, "subagent_empty_result",
			fmt.Sprintf("Agent %s finished without an answer", agent.config.Name),
			tools.Information{Key: "agent", Value: agent.config.Name},
			tools.Information{Key: "rounds", Value: result.Rounds},
			tools.Information{Key: "tokens", Value: result.Tokens}), nil
	}
	text, cut := cutUTF8(text, s.settings.ResultMaxBytes)
	information := []tools.Information{
		{Key: "agent", Value: agent.config.Name},
		{Key: "rounds", Value: result.Rounds},
		{Key: "tokens", Value: result.Tokens},
	}
	if cut {
		information = append(information, tools.Information{Key: "truncated", Value: true})
	}
	return tools.RenderToolResult(tools.ResultOptions{
		ID:          callID,
		Status:      tools.StatusOK,
		Summary:     fmt.Sprintf("Agent %s finished", agent.config.Name),
		Information: information,
		Content:     &tools.Content{Encoding: "utf8", Text: text},
	})
}

// subagentReasonBytes bounds an error text copied into a result, so a provider error
// body cannot fill the parent's context.
const subagentReasonBytes = 400

func boundedReason(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if len(reason) <= subagentReasonBytes {
		return reason
	}
	cut, _ := cutUTF8(reason, subagentReasonBytes)
	return cut + "..."
}
