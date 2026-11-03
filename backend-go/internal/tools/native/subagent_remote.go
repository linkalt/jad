package native

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
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

func (r remoteAgentSettings) allowed(name string) bool { return slices.Contains(r.names, name) }

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
	if s.depthExceeded(execution.AgentDepth) {
		return depthExceededError(callID), nil
	}
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
	resume, hasResume := arguments["resume"].(string)
	if hasResume && resume != "" {
		remoteRun, problem := remoteResumeReference(resume, name)
		if problem != "" {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "bad_resume", problem), nil
		}
		arguments = withResume(arguments, remoteRun)
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
		if err := s.async.registerProfileMessage(id, asyncmgr.Registration{ID: id, Owner: owner, Kind: asyncKindRemoteSubagent, Label: handleLabel(target.String() + ": " + oneLine(task))}); err != nil {
			return tools.ExecutionError(callID, tools.StatusError, registerErrorCode(err), err.Error()), nil
		}
	}
	// A nested remote start takes its place in the run caps the way a local one does, and gives
	// it back when the run ends or when no run starts. A top-level start is not nested, so it
	// takes none.
	var reservation *remoteReservation
	if execution.AgentDepth > 0 {
		reservation, err = s.manager.reserveRemote(execution.RootRun, execution.SessionKey, int(s.settings.MaxRunsPerRoot), int(s.settings.MaxRunsPerSession), execution.AgentDepth, int(s.settings.MaxDepth))
		if err != nil {
			if handled {
				s.async.abandonProfileMessage(id)
				_, _ = s.async.Manager().Complete(id, asyncmgr.Result{State: asyncmgr.StateFailed, Reason: boundedReason(err.Error())})
				s.async.Manager().MarkRead(owner, id)
			}
			return refuseRemoteCap(callID, err), nil
		}
	}
	body := map[string]any{"session": target.Session, "profile": target.Profile, "agent": target.Agent, "task": task}
	if resume, _ := arguments["resume"].(string); resume != "" {
		body["resume"] = resume
	}
	// A panic before the run takes the reservation gives it back; once the run has it, the run's
	// end does.
	handedOver := false
	defer func() {
		if reservation != nil && !handedOver {
			s.manager.releaseRemote(reservation)
		}
	}()
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
	if reservation != nil {
		// The run's end gives the places back on every path: completion, failure, stop, timeout
		// and cancel all end in run.Done.
		handedOver = true
		s.manager.remoteEnds.Add(1)
		go func() {
			defer s.manager.remoteEnds.Done()
			<-run.Done()
			if f := beforeRemoteBackgroundRelease; f != nil {
				f()
			}
			s.manager.releaseRemote(reservation)
		}()
	}
	if !handled {
		select {
		case <-run.Done():
			// The places are given back before the reply, so a caller that reads the count
			// after the reply sees them free.
			if reservation != nil {
				s.manager.releaseRemote(reservation)
			}
		case <-ctx.Done():
			run.Stop("caller_cancelled")
			return "", ctx.Err()
		}
		return s.renderRemoteOutcome(callID, target, run.Reference(), run.Outcome())
	}
	if reason := s.async.setProfileMessageRun(id, run); reason != "" {
		run.Stop(reason)
	}
	ended := make(chan struct{})
	s.manager.remoteEnds.Add(1)
	go func() {
		defer close(ended)
		defer s.manager.remoteEnds.Done()
		<-run.Done()
		if reservation != nil {
			s.manager.releaseRemote(reservation)
		}
		if f := beforeRemoteAsyncComplete; f != nil {
			f()
		}
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
		return s.renderRemoteOutcome(callID, target, run.Reference(), run.Outcome())
	case <-timer.C:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case <-ended:
		s.async.Manager().MarkRead(owner, id)
		return s.renderRemoteOutcome(callID, target, run.Reference(), run.Outcome())
	default:
	}
	return tools.RenderToolResult(tools.ResultOptions{
		ID:     callID,
		Status: tools.StatusOK,
		Summary: fmt.Sprintf("Agent %s is still running (run %s). Do not start the task again: use the async tool with id %s to wait for the answer (action wait), look at it (status) or cancel it (stop)",
			target.String(), id, id),
		Information: append(append([]tools.Information{{Key: "run", Value: id}, {Key: "state", Value: "running"}}, remoteRunInformation(run.Reference())...), s.async.latestInformation(owner, id)...),
	})
}

// beforeRemoteBackgroundRelease and beforeRemoteAsyncComplete are test seams: nil in production. A
// test sets one to hold a remote run's release or completion at that point, to read the state the
// caller would see there. The background release is the one that runs when the run ends with no
// reply waiting on it; the completion runs once the async handle is about to complete.
var (
	beforeRemoteBackgroundRelease func()
	beforeRemoteAsyncComplete     func()
)

// remoteRunReference is the form remote_run and the resume argument take: "<name>/rr_…", the
// remote's own run id (16 lowercase hex digits).
var remoteRunReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}/rr_[0-9a-f]{16}$`)

// remoteResumeReference reads the resume argument of a remote call: "<name>/rr_…", the form
// remote_run shows, where name is the remote the call names. It returns the id the serving host
// knows the run by ("rr_…"), or the problem with the expected form. Nothing is sent before this.
func remoteResumeReference(resume, name string) (id, problem string) {
	expected := "the resume of a remote run is <name>/rr_… where name is the remote this agent is on (" + name + "), for example " + name + "/rr_0123456789abcdef"
	remoteName, runID, found := strings.Cut(resume, "/")
	if !found || remoteName != name || !remoteRunReference.MatchString(name+"/"+runID) {
		return "", expected
	}
	return runID, ""
}

// withResume returns the call's arguments with resume set to the id the serving host knows,
// so the body carries rr_… and the remote name is not sent twice.
func withResume(arguments map[string]any, id string) map[string]any {
	copied := make(map[string]any, len(arguments))
	for key, value := range arguments {
		copied[key] = value
	}
	copied["resume"] = id
	return copied
}

// refuseRemoteCap is the error a nested remote start gets when the run caps or the depth refuse
// it. The depth keeps its own code; a cap is subagent_limit_reached, as a local start's is.
func refuseRemoteCap(callID string, err error) string {
	if errors.Is(err, errSubagentDepth) {
		return depthExceededError(callID)
	}
	return tools.ExecutionError(callID, tools.StatusError, "subagent_limit_reached", err.Error())
}

// remoteRunInformation names the remote run a result is about, as "<name>/rr_…" under remote_run
// (design_remote.md 3.1). It is empty when no run was created: a refused or failed start has no
// trace, and a run's trace is set on every outcome of a run that existed.
func remoteRunInformation(reference string) []tools.Information {
	if reference == "" {
		return nil
	}
	return []tools.Information{{Key: "remote_run", Value: reference}}
}

// remoteResumeInformation names the run a remote resume continued and carries its config note, as
// a local resumed run does. It is empty for a run that did not resume another.
func remoteResumeInformation(resumedFrom, configNote string) []tools.Information {
	var information []tools.Information
	if resumedFrom != "" {
		information = append(information, tools.Information{Key: "resumed_from", Value: resumedFrom})
	}
	if configNote != "" {
		information = append(information, tools.Information{Key: "config_note", Value: configNote})
	}
	return information
}

func (s *subagentTool) renderRemoteFailure(callID string, target remote.Target, err error, information ...tools.Information) string {
	code, message := remoteSubagentFailure(target, err, s.remotes.timeout)
	return tools.ExecutionError(callID, tools.StatusError, code, message, information...)
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

func (s *subagentTool) renderRemoteOutcome(callID string, target remote.Target, reference string, outcome ProfileMessageOutcome) (string, error) {
	text, cut := cutUTF8(strings.TrimSpace(outcome.Text), s.settings.ResultMaxBytes)
	if outcome.Err != nil {
		if errors.Is(outcome.Err, ErrProfileStopped) && text != "" {
			return tools.RenderToolResult(tools.ResultOptions{
				ID: callID, Status: tools.StatusOK,
				Summary:     fmt.Sprintf("Agent %s was stopped%s", target.String(), stopReasonNote(subagentRunView{reason: outcome.Reason})),
				Information: append([]tools.Information{{Key: "partial", Value: true}}, remoteResumeInformation(outcome.ResumedFrom, outcome.ConfigNote)...),
				Content:     &tools.Content{Encoding: "utf8", Text: text},
			})
		}
		return s.renderRemoteFailure(callID, target, outcome.Err, remoteResumeInformation(outcome.ResumedFrom, outcome.ConfigNote)...), nil
	}
	information := append([]tools.Information{{Key: "agent", Value: target.String()}}, remoteRunInformation(reference)...)
	information = append(information, remoteResumeInformation(outcome.ResumedFrom, outcome.ConfigNote)...)
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
	result := asyncmgr.Result{Summary: text, Detail: text, Cut: cut, Bytes: int64(len(text)), Reason: outcome.Reason, ResumedFrom: outcome.ResumedFrom, ConfigNote: outcome.ConfigNote}
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
