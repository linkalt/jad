package native

import (
	"context"
	"fmt"
	"path/filepath"
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
	changes := agent.config.ChangesFiles()
	childExecution := tools.ExecutionContext{
		WorkspaceRoot: execution.WorkspaceRoot,
		SessionDir:    execution.SessionDir,
	}
	if changes {
		// A child that can change files asks the same user through the same queue as
		// the parent, with its own name on the question. A read-only child asks nobody.
		childExecution.RequestApproval = childApproval(execution.RequestApproval, agent.config.Name, callID)
	}
	run, err := s.manager.start(subagentStart{
		launcher: s.launcher,
		owner:    execution.SessionKey,
		changes:  changes,
		request: SubagentRequest{
			Agent:     agent.config,
			Task:      task,
			CallID:    callID,
			Tools:     agent.catalog,
			Execution: childExecution,
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
	// A child that can change files is never handed off: its approvals and the
	// checkpoint belong to this run, so the call waits for it to end, whatever wait_ms
	// says. The agent's timeout_ms still bounds it.
	wait := s.waitFor(arguments)
	if changes {
		wait = -1
	}
	return s.await(ctx, callID, run, wait, true)
}

// childApproval wraps the parent's approval requester for a child's calls: the question
// names the agent, and the call id carries the parent's call id so it cannot be taken
// for one of the parent's own. A nil parent stays nil, so a run nobody can ask blocks
// the child's calls exactly as it blocks the parent's.
func childApproval(parent tools.ApprovalRequester, agent, parentCallID string) tools.ApprovalRequester {
	if parent == nil {
		return nil
	}
	return func(ctx context.Context, call tools.ApprovalCall) tools.ApprovalVerdict {
		call.Agent = agent
		call.CallID = parentCallID + "/" + call.CallID
		return parent(ctx, call)
	}
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
	// A run restored after a restart has its text in the transcript, not in memory.
	run.loadText(s.settings.ResultMaxBytes)
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

// await waits for a run to end, for at most wait (a negative wait has no limit), and
// renders where it stands. When ownsRun is set and ctx ends first, the run is cancelled
// and awaited, so the child has stopped when the call returns.
func (s *subagentTool) await(ctx context.Context, callID string, run *subagentRun, wait time.Duration, ownsRun bool) (string, error) {
	var expired <-chan time.Time
	if wait >= 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-run.done:
	case <-expired:
	case <-ctx.Done():
		if ownsRun {
			s.manager.stop(run)
			<-run.done
		}
		return "", ctx.Err()
	}
	return s.renderRun(callID, run.view())
}

// partialInformation adds what a cut-off run left behind to a result: a flag that the
// text is incomplete, and the child's transcript, where its tool results are.
func partialInformation(information []tools.Information, run subagentRunView) []tools.Information {
	if strings.TrimSpace(run.result.Text) != "" {
		information = append(information, tools.Information{Key: "partial", Value: true})
		if run.cut {
			information = append(information, tools.Information{Key: "truncated", Value: true})
		}
	}
	if run.dir != "" {
		information = append(information, tools.Information{Key: "transcript", Value: filepath.Join(run.dir, "messages.jsonl")})
	}
	return information
}

// partialContent is the last text the child wrote before it was cut off, or nil when
// it wrote none. It is not an answer: the child did not finish.
func partialContent(run subagentRunView) *tools.Content {
	text := strings.TrimSpace(run.result.Text)
	if text == "" {
		return nil
	}
	return &tools.Content{Encoding: "utf8", Text: text}
}

// changesNote is added to the result of a child that can change files and did not
// finish: what it did to the files is not rolled back, and its answer is not there to
// say how far it got.
func changesNote(run subagentRunView) string {
	if !run.changes {
		return ""
	}
	return ". It can change files, so some changes may already be made: check them before starting it again"
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
		information := []tools.Information{{Key: "rounds", Value: run.result.Rounds}, {Key: "tokens", Value: run.result.Tokens}}
		return tools.RenderToolResult(tools.ResultOptions{
			ID:          callID,
			Status:      tools.StatusOK,
			Summary:     fmt.Sprintf("Agent %s was stopped (run %s)%s", run.agent, run.id, changesNote(run)),
			Information: partialInformation(information, run),
			Content:     partialContent(run),
		})
	case subagentRunTimedOut:
		information := []tools.Information{
			{Key: "error_type", Value: "execution"},
			{Key: "error_code", Value: "subagent_timeout"},
			{Key: "run", Value: run.id},
			{Key: "agent", Value: run.agent},
			{Key: "rounds", Value: run.result.Rounds},
			{Key: "tokens", Value: run.result.Tokens},
		}
		return tools.RenderToolResult(tools.ResultOptions{
			ID:          callID,
			Status:      tools.StatusError,
			Summary:     fmt.Sprintf("Agent %s did not finish within %d ms%s", run.agent, run.timeoutMS, changesNote(run)),
			Information: partialInformation(information, run),
			Content:     partialContent(run),
		})
	case subagentRunLost:
		information := []tools.Information{
			{Key: "error_type", Value: "execution"},
			{Key: "error_code", Value: "subagent_lost"},
			{Key: "run", Value: run.id},
			{Key: "agent", Value: run.agent},
			{Key: "rounds", Value: run.result.Rounds},
			{Key: "tokens", Value: run.result.Tokens},
		}
		return tools.RenderToolResult(tools.ResultOptions{
			ID:     callID,
			Status: tools.StatusError,
			Summary: fmt.Sprintf("Agent %s was lost: the server stopped while it was going (run %s). The child did not survive. Start the task again only if the partial work below is not enough%s",
				run.agent, run.id, changesNote(run)),
			Information: partialInformation(information, run),
			Content:     partialContent(run),
		})
	case subagentRunFailed:
		return tools.ExecutionError(callID, tools.StatusError, "subagent_failed",
			fmt.Sprintf("Agent %s failed: %s%s", run.agent, boundedReason(run.reason), changesNote(run)),
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
