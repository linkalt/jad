package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

const profileMessageSource = "profile_message"

// ProfileRunRequest is one message a tool sends to a profile of a session.
type ProfileRunRequest struct {
	Reference string
	Profile   string
	// Caller is the profile that sends the message; the run is listed with it.
	Caller  string
	Message string
	// BusyWait is how long to queue behind the run that holds the transcript, in the
	// order user messages queue; 0 fails at once with ErrSessionBusy.
	BusyWait time.Duration
	// OnCommitted sees every response the run commits, on the run's goroutine.
	OnCommitted func(event json.RawMessage)
}

// ProfileRunStarter starts a run of a profile for a tool, the way a user's message
// starts one: same locks, same registry entry, same stream and stop route.
type ProfileRunStarter interface {
	// StartProfileRun returns ErrSessionBusy when another run holds the transcript.
	StartProfileRun(ctx context.Context, request ProfileRunRequest) (*ProfileRun, error)
	// RunningProfileRunCaller reports the caller of the profile's running tool-started
	// run, if there is one.
	RunningProfileRunCaller(reference, profile string) (caller string, ok bool)
}

// ProfileRun is a started run. Final is only valid once Done is closed.
type ProfileRun struct {
	run   *activeRun
	final json.RawMessage
}

func (p *ProfileRun) Done() <-chan struct{}  { return p.run.done }
func (p *ProfileRun) Status() string         { return p.run.state() }
func (p *ProfileRun) Final() json.RawMessage { return p.final }
func (p *ProfileRun) Stop()                  { p.run.stop() }

// ProfileRuns returns the port that starts tool-started runs.
func (h *Handler) ProfileRuns() ProfileRunStarter { return profileRunStarter{handler: h} }

type profileRunStarter struct{ handler *Handler }

func (s profileRunStarter) StartProfileRun(ctx context.Context, request ProfileRunRequest) (*ProfileRun, error) {
	h := s.handler
	input, err := json.Marshal(request.Message)
	if err != nil {
		return nil, err
	}
	var started startedRun
	var busy bool
	if request.BusyWait > 0 {
		started, err = h.acquireProfileRun(ctx, request)
	} else {
		started, busy, err = h.tryStartRun(ctx, request.Reference, request.Profile, "", profileMessageSource, request.Caller)
	}
	if busy {
		return nil, ErrSessionBusy
	}
	if err != nil {
		var statusErr *StatusError
		if errors.As(err, &statusErr) && statusErr.Code == http.StatusConflict {
			return nil, ErrSessionBusy
		}
		return nil, err
	}
	run := started.run
	handle := &ProfileRun{run: run}
	run.mu.Lock()
	run.supportsInteraction = true
	run.mu.Unlock()
	run.onCommitted = request.OnCommitted
	run.onResult = func(event json.RawMessage) { handle.final = event }
	h.attachContentCache(run)
	go func() {
		defer started.processUnlock()
		h.executeRun(run, started.resolved.Runner, input)
	}()
	return handle, nil
}

func (s profileRunStarter) RunningProfileRunCaller(reference, profile string) (string, bool) {
	run := s.handler.runs.current(s.handler.runKey(ResolvedSession{Reference: reference, Profile: profile}))
	if run == nil || run.source != profileMessageSource || run.state() != "running" {
		return "", false
	}
	return run.caller, true
}

// acquireProfileRun queues for the transcript like a user message for at most BusyWait.
// The wait ending, while ctx is still alive, is ErrSessionBusy. The deadline only bounds
// the wait: the started run does not depend on it.
func (h *Handler) acquireProfileRun(ctx context.Context, request ProfileRunRequest) (startedRun, error) {
	waitCtx, cancel := context.WithTimeout(ctx, request.BusyWait)
	defer cancel()
	started, err := h.acquireRun(waitCtx, request.Reference, request.Profile, "", profileMessageSource, request.Caller)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return startedRun{}, ErrSessionBusy
	}
	return started, err
}
