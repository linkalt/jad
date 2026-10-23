package native

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

// execute runs one call of the subagent tool: a call with a task starts a run and blocks up
// to block_ms for it; a call without a task lists the agents. Waiting for, inspecting and
// stopping a run that outlived its call belong to the async tool.
func (s *subagentTool) execute(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
	task, _ := arguments["task"].(string)
	task = strings.TrimSpace(task)
	if task == "" {
		// The caller wants the agent list. It needs no launcher, so it works even
		// where children cannot run.
		s.mu.RLock()
		defer s.mu.RUnlock()
		return renderSubagentList(execution.CallID, s.agents, s.settings.ResultMaxBytes, s.settings)
	}
	return s.startRun(ctx, execution.CallID, task, arguments, execution)
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
	depth := execution.AgentDepth
	if depth+1 > max(int(s.settings.MaxDepth), 1) {
		return tools.ExecutionError(callID, tools.StatusError, "subagent_depth_exceeded",
			"A sub-agent cannot start another one from this depth (tools.native.subagent.max_depth)"), nil
	}
	childExecution := tools.ExecutionContext{
		WorkspaceRoot: execution.WorkspaceRoot,
		SessionDir:    execution.SessionDir,
		SessionKey:    execution.SessionKey,
		AgentName:     agent.config.Name,
	}
	var ownScope *tools.PathScope
	if agent.config.Paths.Set() {
		ownScope = &tools.PathScope{Allow: agent.config.Paths.Allow, Deny: agent.config.Paths.Deny}
	}
	childExecution.PathScope = narrowScope(execution.WorkspaceRoot, execution.PathScope, ownScope)
	// A writer becomes a handle only when it is allowed to (background_writers) and its
	// session has a place left; otherwise it runs to its end inside this call, where the
	// checkpoint belongs.
	background, note := false, ""
	if changes && agent.background && s.async != nil && execution.SessionKey != "" && depth == 0 {
		if background = s.manager.reserveWriter(execution.SessionKey, int(s.settings.MaxBackgroundWriters)); !background {
			note = fmt.Sprintf("Not handed off: the session already has %d background writer(s) going (tools.native.subagent.max_background_writers), so this call ran to its end", s.settings.MaxBackgroundWriters)
		}
	}
	// A run started inside a child belongs to that child's run: its handle is the child's
	// own, and exists only when the child has the async tool to collect it.
	owner, canHandle := handleOwner(execution)
	var handle *subagentAsyncHandle
	if s.async != nil && (!changes || background) && canHandle {
		handle = newSubagentAsyncHandle(s.async, owner, agent.config.Name, subagentTaskPreview(task))
	}
	run, err := s.manager.start(subagentStart{
		async:          handle,
		asyncHost:      s.async,
		childAsync:     agent.hasNativeTool(asyncToolID),
		launcher:       s.launcher,
		owner:          owner,
		session:        execution.SessionKey,
		depth:          depth,
		rootRun:        execution.RootRun,
		parentID:       execution.ProcessOwner,
		maxDepth:       int(s.settings.MaxDepth),
		maxPerRoot:     int(s.settings.MaxRunsPerRoot),
		maxPerSession:  int(s.settings.MaxRunsPerSession),
		changes:        changes,
		note:           note,
		writerReserved: background,
		request: SubagentRequest{
			Agent:     agent.config,
			Task:      task,
			CallID:    callID,
			Tools:     agent.catalog,
			Execution: childExecution,
		},
		processes:  agent.processes,
		limit:      int(s.settings.MaxConcurrent),
		maxQueued:  int(s.settings.MaxQueued),
		changedMax: int(s.settings.ChangedFilesMax),
		processMax: int(s.settings.ProcessMaxRunning),
		keep:       int(s.settings.KeepFinished),
		resultMax:  s.settings.ResultMaxBytes,
	})
	if err != nil {
		if background {
			s.manager.releaseWriter(execution.SessionKey)
		}
		code := "subagent_unavailable"
		switch {
		case errors.Is(err, errSubagentQueueFull), errors.Is(err, errSubagentNoSlot):
			code = "subagent_busy"
		case errors.Is(err, errSubagentDepth):
			code = "subagent_depth_exceeded"
		case errors.Is(err, errSubagentRunLimit):
			code = "subagent_limit_reached"
		}
		if errors.Is(err, asyncmgr.ErrCapacity) {
			code = "async_capacity_reached"
		}
		return tools.ExecutionError(callID, tools.StatusError, code, err.Error()), nil
	}
	// A run without a handle (a writer, or a session without the async tool) has nobody to
	// collect it later: the checkpoint belongs to this call, which waits
	// for its end whatever block_ms says, and cancelling the call cancels the run. The
	// agent's timeout_ms still bounds it. A handle outlives the call: the call's end stops
	// the wait, never the run.
	wait := -time.Millisecond
	if handle != nil {
		wait = s.blockFor(arguments)
	}
	return s.await(ctx, callID, run, wait, handle == nil)
}

// handleOwner is the owner under which a call registers a handle of the async manager and
// whether it may: the session in the session's own run, the run's id inside a child, and
// there only when the child has the async tool (ProcessAsync).
func handleOwner(execution tools.ExecutionContext) (string, bool) {
	if execution.ProcessOwner != "" {
		return execution.ProcessOwner, execution.ProcessAsync
	}
	return execution.SessionKey, execution.SessionKey != ""
}

// blockFor is the block the call asked for, or the default, within the async policy cap.
func (s *subagentTool) blockFor(arguments map[string]any) time.Duration {
	var asked *int64
	if value, ok := safeInteger(arguments["block_ms"]); ok {
		asked = &value
	}
	return s.async.Config().BlockDuration(asked)
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
	cancelled := func() (string, error) {
		if ownsRun {
			s.manager.stop(run)
			<-run.done
		}
		return "", ctx.Err()
	}
	select {
	case <-run.done:
	case <-expired:
	case <-ctx.Done():
		return cancelled()
	}
	view := run.view()
	if run.async != nil && view.state != subagentRunQueued && view.state != subagentRunRunning {
		run.async.markRead(run.id)
	}
	return s.renderRun(callID, view)
}

// partialInformation adds what a cut-off run left behind to a result: a flag that the
// text is incomplete, and the child's transcript, where its tool results are.
func partialInformation(information []tools.Information, run subagentRunView) []tools.Information {
	information = append(information, changedInformation(run)...)
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

// backgroundWriterNote is added to the running result of a writer: it keeps changing files
// after the turn ends, and the checkpoint of this turn does not cover that.
func backgroundWriterNote(run subagentRunView) string {
	if !run.changes {
		return ""
	}
	return ". It can change files and goes on after this turn ends: do not edit the paths it works on until it ends, and what it changes after this turn is not in this turn's checkpoint"
}

func stopReasonNote(run subagentRunView) string {
	if run.reason == "" {
		return ""
	}
	return ": " + run.reason
}

func (s *subagentTool) renderRun(callID string, run subagentRunView) (string, error) {
	switch run.state {
	case subagentRunQueued, subagentRunRunning:
		return tools.RenderToolResult(tools.ResultOptions{
			ID:     callID,
			Status: tools.StatusOK,
			Summary: fmt.Sprintf("Agent %s is still %s (run %s). Do not start the task again: use the async tool with id %s to wait for the answer (action wait), look at it (status) or cancel it (stop)%s",
				run.agent, run.state, run.id, run.id, backgroundWriterNote(run)),
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
			Summary:     fmt.Sprintf("Agent %s was stopped (run %s)%s%s", run.agent, run.id, stopReasonNote(run), changesNote(run)),
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
			append([]tools.Information{{Key: "agent", Value: run.agent}}, changedInformation(run)...)...), nil
	}
	text := strings.TrimSpace(run.result.Text)
	if text == "" {
		return tools.ExecutionError(callID, tools.StatusError, "subagent_empty_result",
			fmt.Sprintf("Agent %s finished without an answer", run.agent),
			append([]tools.Information{
				{Key: "agent", Value: run.agent},
				{Key: "rounds", Value: run.result.Rounds},
				{Key: "tokens", Value: run.result.Tokens},
			}, changedInformation(run)...)...), nil
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
	information = append(information, changedInformation(run)...)
	return tools.RenderToolResult(tools.ResultOptions{
		ID:          callID,
		Status:      tools.StatusOK,
		Summary:     fmt.Sprintf("Agent %s finished", run.agent),
		Information: information,
		Content:     &tools.Content{Encoding: "utf8", Text: text},
	})
}
