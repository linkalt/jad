package native

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/tools"
)

// asyncKindRemoteSubagent is the handle kind of a sub-agent running on another JAD server.
const asyncKindRemoteSubagent asyncmgr.Kind = "remote_subagent"

// attachRemoteSubagent registers the driver of kind remote_subagent once. It shares the run
// table of profile_message: a remote run is the same object either way.
func (h *AsyncHost) attachRemoteSubagent() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.remoteSubagentSeen {
		return
	}
	h.remoteSubagentSeen = true
	_ = h.manager.AddDriver(&remoteSubagentDriver{host: h})
}

type remoteSubagentDriver struct{ host *AsyncHost }

func (d *remoteSubagentDriver) Kind() asyncmgr.Kind { return asyncKindRemoteSubagent }

func (d *remoteSubagentDriver) Stop(id, reason string) { d.host.stopProfileMessage(id, reason) }

func (d *remoteSubagentDriver) latest(id string, info latestInfo, prev any, cfg asyncmgr.Config, now time.Time) (string, any) {
	if remote, ok := d.host.profileMessageRun(id).(*remoteRun); ok {
		return remoteLatest(remote, info, now), prev
	}
	return "", prev
}

func (d *remoteSubagentDriver) Detail(id string, maxBytes int) string {
	run := d.host.profileMessageRun(id)
	if run == nil {
		return ""
	}
	text, _ := cutUTF8(run.Status(), int64(maxBytes))
	return text
}

// remoteAgentSettings is what the subagent tool knows about remotes.
type remoteAgentSettings struct {
	names     []string
	directory *remote.Directory
	timeout   time.Duration
}

func (r remoteAgentSettings) allowed(name string) bool {
	for _, allowed := range r.names {
		if allowed == name {
			return true
		}
	}
	return false
}

// remoteListText is appended to the tool description when the session enables remotes.
func remoteListText(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "\nRemote agents on other JAD servers are written agent: remote:<name>/<session>/<category>/<profile>/<agent> (remotes: " + strings.Join(names, ", ") + "); the session names the profile whose agent list applies, and the remote decides what you may reach."
}

// startRemote starts a sub-agent on a remote server and follows it as a handle of the async
// tool, or inline when the call cannot collect a handle later.
func (s *subagentTool) startRemote(ctx context.Context, callID, reference, task string, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
	name, _, _ := remote.Split(reference)
	if len(s.remotes.names) == 0 || s.remotes.directory == nil {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "remote_unavailable",
			"This session does not enable remote agents (tools.native.subagent.remotes)"), nil
	}
	if !s.remotes.allowed(name) {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "unknown_remote",
			fmt.Sprintf("Remote %s is not available for agents", name),
			tools.Information{Key: "available", Value: strings.Join(s.remotes.names, ", ")}), nil
	}
	target, err := remote.ParseSubagent(reference, s.remotes.directory.Entry)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "bad_remote_reference", err.Error()), nil
	}
	entry, err := s.remotes.directory.Entry(name)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "remote_unavailable", remoteProblem(name, err)), nil
	}
	if !entry.Allows(target.Scope()) {
		return tools.ExecutionError(callID, tools.StatusForbidden, "remote_not_allowed",
			fmt.Sprintf("Agent %s is not among the agents this session may reach on remote %s", target.Scope(), name)), nil
	}
	owner, canHandle := handleOwner(execution)
	handled := s.async != nil && canHandle
	var id string
	if handled {
		random := make([]byte, 4)
		if _, err := rand.Read(random); err != nil {
			return tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The run id could not be created"), nil
		}
		id = "rs_" + hex.EncodeToString(random)
		label, cut := cutUTF8(target.String()+": "+oneLine(task), profileMessageLabelBytes)
		if cut {
			label += "..."
		}
		s.async.beginProfileMessage(id)
		if err := s.async.Manager().Register(asyncmgr.Registration{ID: id, Owner: owner, Kind: asyncKindRemoteSubagent, Label: label}); err != nil {
			s.async.abandonProfileMessage(id)
			code := "spawn_failed"
			if errors.Is(err, asyncmgr.ErrCapacity) {
				code = "async_capacity_reached"
			}
			return tools.ExecutionError(callID, tools.StatusError, code, err.Error()), nil
		}
	}
	body := map[string]any{"session": target.Session, "profile": target.Profile, "agent": target.Agent, "task": task}
	run, err := startRemoteRun(ctx, s.remotes.directory, target, "subagents", body, execution.RemoteHops+1, s.remotes.timeout, "agent")
	if err != nil {
		if handled {
			s.async.abandonProfileMessage(id)
			_, _ = s.async.Manager().Complete(id, asyncmgr.Result{State: asyncmgr.StateFailed, Reason: boundedReason(err.Error())})
			s.async.Manager().MarkRead(owner, id)
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return s.renderRemoteFailure(callID, target, err), nil
	}
	if !handled {
		select {
		case <-run.Done():
		case <-ctx.Done():
			run.Stop("caller_cancelled")
			<-run.Done()
			return "", ctx.Err()
		}
		return s.renderRemoteOutcome(callID, target, run.Outcome())
	}
	if reason := s.async.setProfileMessageRun(id, run); reason != "" {
		run.Stop(reason)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		<-run.Done()
		outcome := run.Outcome()
		s.async.dropProfileMessageRun(id)
		_, _ = s.async.Manager().Complete(id, s.remoteAsyncResult(target, outcome))
	}()
	s.async.Manager().MarkRunning(id)
	timer := time.NewTimer(s.blockFor(arguments))
	defer timer.Stop()
	select {
	case <-ended:
		s.async.Manager().MarkRead(owner, id)
		return s.renderRemoteOutcome(callID, target, run.Outcome())
	case <-timer.C:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case <-ended:
		s.async.Manager().MarkRead(owner, id)
		return s.renderRemoteOutcome(callID, target, run.Outcome())
	default:
	}
	return tools.RenderToolResult(tools.ResultOptions{
		ID:     callID,
		Status: tools.StatusOK,
		Summary: fmt.Sprintf("Agent %s is still running (run %s). Do not start the task again: use the async tool with id %s to wait for the answer (action wait), look at it (status) or cancel it (stop)",
			target.String(), id, id),
		Information: append([]tools.Information{{Key: "run", Value: id}, {Key: "state", Value: "running"}}, s.async.latestInformation(owner, id)...),
	})
}

func (s *subagentTool) renderRemoteFailure(callID string, target remote.Target, err error) string {
	code, message := remoteSubagentFailure(target, err, s.remotes.timeout)
	return tools.ExecutionError(callID, tools.StatusError, code, message)
}

// remoteSubagentFailure names a remote sub-agent that did not end in an answer.
func remoteSubagentFailure(target remote.Target, err error, timeout time.Duration) (code, message string) {
	reference := target.String()
	var busy *ProfileBusyError
	var failed *RemoteFailedError
	var profileFailed *ProfileFailedError
	switch {
	case errors.Is(err, ErrProfileStopped):
		return "subagent_stopped", fmt.Sprintf("Agent %s was stopped", reference)
	case errors.Is(err, ErrProfileTimeout):
		return "subagent_timeout", fmt.Sprintf("Agent %s did not finish within %d ms", reference, timeout.Milliseconds())
	case errors.Is(err, ErrRemoteLost):
		return "remote_lost", fmt.Sprintf("Agent %s was lost: the remote could not be reached. It may still be running there; check the remote's transcripts before starting the task again", reference)
	case errors.Is(err, ErrProfileNoAnswer):
		return "subagent_empty_result", fmt.Sprintf("Agent %s finished without an answer", reference)
	case errors.As(err, &busy):
		return "subagent_busy", fmt.Sprintf("The remote has no free place for agent %s; try again later", reference)
	case errors.As(err, &failed):
		return failed.Code, failed.Message
	case errors.As(err, &profileFailed):
		message := fmt.Sprintf("Agent %s failed", reference)
		if profileFailed.Message != "" {
			message += ": " + profileFailed.Message
		}
		return "subagent_failed", message
	}
	return "subagent_failed", fmt.Sprintf("Agent %s failed", reference)
}

func (s *subagentTool) renderRemoteOutcome(callID string, target remote.Target, outcome ProfileMessageOutcome) (string, error) {
	text, cut := cutUTF8(strings.TrimSpace(outcome.Text), s.settings.ResultMaxBytes)
	if outcome.Err != nil {
		if errors.Is(outcome.Err, ErrProfileStopped) && text != "" {
			return tools.RenderToolResult(tools.ResultOptions{
				ID: callID, Status: tools.StatusOK,
				Summary:     fmt.Sprintf("Agent %s was stopped%s", target.String(), stopReasonNote(subagentRunView{reason: outcome.Reason})),
				Information: []tools.Information{{Key: "partial", Value: true}},
				Content:     &tools.Content{Encoding: "utf8", Text: text},
			})
		}
		return s.renderRemoteFailure(callID, target, outcome.Err), nil
	}
	information := []tools.Information{{Key: "agent", Value: target.String()}}
	if cut {
		information = append(information, tools.Information{Key: "truncated", Value: true})
	}
	return tools.RenderToolResult(tools.ResultOptions{
		ID: callID, Status: tools.StatusOK,
		Summary:     fmt.Sprintf("Agent %s finished", target.String()),
		Information: information,
		Content:     &tools.Content{Encoding: "utf8", Text: text},
	})
}

// remoteAsyncResult maps how a remote sub-agent ended to the unified async result.
func (s *subagentTool) remoteAsyncResult(target remote.Target, outcome ProfileMessageOutcome) asyncmgr.Result {
	text, cut := cutUTF8(strings.TrimSpace(outcome.Text), s.settings.ResultMaxBytes)
	result := asyncmgr.Result{Summary: text, Detail: text, Cut: cut, Bytes: int64(len(text)), Reason: outcome.Reason}
	if outcome.Err == nil {
		result.State, result.OK = asyncmgr.StateDone, true
		return result
	}
	code, message := remoteSubagentFailure(target, outcome.Err, s.remotes.timeout)
	result.Code = code
	switch {
	case errors.Is(outcome.Err, ErrProfileStopped):
		result.State = asyncmgr.StateStopped
	case errors.Is(outcome.Err, ErrProfileTimeout):
		result.State = asyncmgr.StateTimedOut
	case errors.Is(outcome.Err, ErrRemoteLost):
		result.State = asyncmgr.StateLost
	default:
		result.State = asyncmgr.StateFailed
	}
	if result.Reason == "" || result.State == asyncmgr.StateFailed {
		result.Reason = boundedReason(message)
	}
	if text == "" {
		result.Summary, result.Detail = message, message
	}
	return result
}
