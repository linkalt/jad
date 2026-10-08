package native

import (
	"context"
	"fmt"
	"strings"
	"time"

	"example.com/agent-harness/backend-go/internal/tools"
)

// execute runs one call of the subagent tool. There are three kinds: a call with an
// id looks at, waits for or stops an earlier run; a call with a task starts a run and
// waits for it; a call with neither lists the agents and the retained runs.
func (s *subagentTool) execute(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
	callID := execution.CallID
	task, _ := arguments["task"].(string)
	task = strings.TrimSpace(task)
	id, _ := arguments["id"].(string)
	stop, _ := arguments["stop"].(bool)

	switch {
	case id != "":
		if task != "" || arguments["agent"] != nil {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "conflicting_arguments",
				"Give either id (to check a run) or task (to start one), not both"), nil
		}
		return s.followUp(ctx, callID, execution.SessionKey, id, stop, arguments)
	case stop:
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "missing_id", "stop needs the id of the run to cancel"), nil
	case task == "":
		// The caller wants the agent list. It needs no launcher, so it works even
		// where children cannot run.
		s.mu.RLock()
		defer s.mu.RUnlock()
		return renderSubagentList(callID, s.agents, s.manager.list(execution.SessionKey), s.settings.ResultMaxBytes)
	}
	return s.startRun(ctx, callID, task, arguments, execution)
}

func (s *subagentTool) startRun(ctx context.Context, callID, task string, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
	agent, problem := s.selectAgent(arguments)
	if problem != "" {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "unknown_agent", problem,
			tools.Information{Key: "available", Value: s.agentNames()}), nil
	}
	if s.launcher == nil {
		return tools.ExecutionError(callID, tools.StatusError, "subagent_unavailable", "Sub-agent runs are not available in this session"), nil
	}
	if agent.catalog == nil {
		return tools.ExecutionError(callID, tools.StatusError, "subagent_unavailable",
			fmt.Sprintf("The MCP tools of agent %s were not loaded", agent.config.Name)), nil
	}
	run, err := s.manager.start(subagentStart{
		launcher: s.launcher,
		owner:    execution.SessionKey,
		request: SubagentRequest{
			Agent:  agent.config,
			Task:   task,
			CallID: callID,
			Tools:  agent.catalog,
			Execution: tools.ExecutionContext{
				WorkspaceRoot: execution.WorkspaceRoot,
				SessionDir:    execution.SessionDir,
			},
		},
		limit:     int(s.settings.MaxConcurrent),
		maxQueued: int(s.settings.MaxQueued),
		keep:      int(s.settings.KeepFinished),
		resultMax: s.settings.ResultMaxBytes,
	})
	if err != nil {
		code := "subagent_unavailable"
		if err == errSubagentQueueFull {
			code = "subagent_busy"
		}
		return tools.ExecutionError(callID, tools.StatusError, code, err.Error()), nil
	}
	// The run is not detached yet, so the parent stopping ends it, as it always did.
	return s.await(ctx, callID, run, s.waitFor(arguments), true)
}

func (s *subagentTool) followUp(ctx context.Context, callID, owner, id string, stop bool, arguments map[string]any) (string, error) {
	run := s.manager.get(id, owner)
	if run == nil {
		return tools.ExecutionError(callID, tools.StatusNotFound, "unknown_run",
			fmt.Sprintf("No run with id %s; it may have been dropped. Call with no task to list the runs", id)), nil
	}
	if stop {
		s.manager.stop(run)
	}
	// A detached run goes on when this call is cancelled: the model can still ask for it.
	return s.await(ctx, callID, run, s.waitFor(arguments), false)
}

// waitFor is the wait the call asked for, or the default, within the policy cap.
func (s *subagentTool) waitFor(arguments map[string]any) time.Duration {
	wait := s.settings.WaitMS
	if asked, ok := safeInteger(arguments["wait_ms"]); ok {
		wait = asked
	}
	wait = min(max(wait, 0), s.settings.MaxWaitMS)
	return time.Duration(wait) * time.Millisecond
}

// await waits for a run to end, for at most wait, and renders where it stands. When
// ownsRun is set and ctx ends first, the run is cancelled and awaited, so the child
// has stopped when the call returns.
func (s *subagentTool) await(ctx context.Context, callID string, run *subagentRun, wait time.Duration, ownsRun bool) (string, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-run.done:
	case <-timer.C:
	case <-ctx.Done():
		if ownsRun {
			s.manager.stop(run)
			<-run.done
		}
		return "", ctx.Err()
	}
	return s.renderRun(callID, run.view())
}

func (s *subagentTool) renderRun(callID string, run subagentRunView) (string, error) {
	switch run.state {
	case subagentRunQueued, subagentRunRunning:
		return tools.RenderToolResult(tools.ResultOptions{
			ID:     callID,
			Status: tools.StatusOK,
			Summary: fmt.Sprintf("Agent %s is still %s (run %s). Do not start the task again: call subagent with id %s and wait_ms to collect the answer, or stop: true to cancel",
				run.agent, run.state, run.id, run.id),
			Information: []tools.Information{
				{Key: "run", Value: run.id},
				{Key: "state", Value: run.state},
				{Key: "elapsed_ms", Value: run.elapsed.Milliseconds()},
				{Key: "limit_ms", Value: run.timeoutMS},
			},
		})
	case subagentRunStopped:
		return tools.RenderToolResult(tools.ResultOptions{
			ID:          callID,
			Status:      tools.StatusOK,
			Summary:     fmt.Sprintf("Agent %s was stopped (run %s)", run.agent, run.id),
			Information: []tools.Information{{Key: "rounds", Value: run.result.Rounds}, {Key: "tokens", Value: run.result.Tokens}},
		})
	case subagentRunTimedOut:
		return tools.ExecutionError(callID, tools.StatusError, "subagent_timeout",
			fmt.Sprintf("Agent %s did not finish within %d ms", run.agent, run.timeoutMS),
			tools.Information{Key: "agent", Value: run.agent},
			tools.Information{Key: "rounds", Value: run.result.Rounds},
			tools.Information{Key: "tokens", Value: run.result.Tokens}), nil
	case subagentRunFailed:
		return tools.ExecutionError(callID, tools.StatusError, "subagent_failed",
			fmt.Sprintf("Agent %s failed: %s", run.agent, boundedReason(run.reason)),
			tools.Information{Key: "agent", Value: run.agent}), nil
	}
	text := strings.TrimSpace(run.result.Text)
	if text == "" {
		return tools.ExecutionError(callID, tools.StatusError, "subagent_empty_result",
			fmt.Sprintf("Agent %s finished without an answer", run.agent),
			tools.Information{Key: "agent", Value: run.agent},
			tools.Information{Key: "rounds", Value: run.result.Rounds},
			tools.Information{Key: "tokens", Value: run.result.Tokens}), nil
	}
	// The manager cut the answer to the limit when the run ended; this only reports it.
	information := []tools.Information{
		{Key: "agent", Value: run.agent},
		{Key: "rounds", Value: run.result.Rounds},
		{Key: "tokens", Value: run.result.Tokens},
	}
	if run.cut {
		information = append(information, tools.Information{Key: "truncated", Value: true})
	}
	return tools.RenderToolResult(tools.ResultOptions{
		ID:          callID,
		Status:      tools.StatusOK,
		Summary:     fmt.Sprintf("Agent %s finished", run.agent),
		Information: information,
		Content:     &tools.Content{Encoding: "utf8", Text: text},
	})
}
