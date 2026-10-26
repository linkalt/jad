package httpapi

import (
	"bytes"
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
	Caller string
	// Chain is the profile names the message travelled through, the caller first and the
	// target last; the run keeps it so a message sent from inside it can be checked.
	Chain   []string
	Message string
	// Hops is the remote hop count of the request that caused this message (0 when none).
	Hops int
	// Wait queues behind the run that holds the transcript, in the order user messages
	// queue, instead of failing at once with ErrSessionBusy. WaitLimit bounds that wait;
	// zero waits until ctx ends. The limit ending while ctx lives is ErrSessionBusy.
	Wait      bool
	WaitLimit time.Duration
	// OnCommitted sees every response the run commits, on the run's goroutine.
	OnCommitted func(event json.RawMessage)
	// BeforeRun, when set, runs once the transcript is held and the run is registered,
	// before the target run begins, so nothing can take the transcript in between.
	BeforeRun func(ctx context.Context)
}

// ProfileRunStarter starts a run of a profile for a tool, the way a user's message
// starts one: same locks, same registry entry, same stream and stop route.
type ProfileRunStarter interface {
	// StartProfileRun returns ErrSessionBusy when another run holds the transcript.
	StartProfileRun(ctx context.Context, request ProfileRunRequest) (*ProfileRun, error)
	// RunningProfileRunCaller reports the caller of the profile's running tool-started
	// run, if there is one.
	RunningProfileRunCaller(reference, profile string) (caller string, ok bool)
	// RunningProfileRunChain reports the chain of the profile's running tool-started run.
	RunningProfileRunChain(reference, profile string) (chain []string, ok bool)
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

// TurnID is the id of the run's turn in the target's transcript.
func (p *ProfileRun) TurnID() string { return p.run.turnID }

// Failure is the client-safe message of a failed run, empty when there is none. Only
// valid once Done is closed.
func (p *ProfileRun) Failure() string {
	p.run.mu.Lock()
	defer p.run.mu.Unlock()
	for _, event := range p.run.terminal {
		if event.name != "error" {
			continue
		}
		_, data, _ := bytes.Cut(event.frame, []byte("data: "))
		var payload struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(bytes.TrimSpace(data), &payload) == nil {
			return payload.Message
		}
	}
	return ""
}

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
	if request.Wait {
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
	run.chain = append([]string(nil), request.Chain...)
	run.hops = request.Hops
	run.mu.Unlock()
	if request.BeforeRun != nil {
		request.BeforeRun(ctx)
	}
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

func (s profileRunStarter) RunningProfileRunChain(reference, profile string) ([]string, bool) {
	run := s.handler.runs.current(s.handler.runKey(ResolvedSession{Reference: reference, Profile: profile}))
	if run == nil || run.source != profileMessageSource || run.state() != "running" {
		return nil, false
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return append([]string(nil), run.chain...), true
}

// acquireProfileRun queues for the transcript like a user message, for at most WaitLimit
// when it is set. The limit ending, while ctx is still alive, is ErrSessionBusy. The
// deadline only bounds the wait: the started run does not depend on it.
func (h *Handler) acquireProfileRun(ctx context.Context, request ProfileRunRequest) (startedRun, error) {
	waitCtx, cancel := ctx, context.CancelFunc(func() {})
	if request.WaitLimit > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, request.WaitLimit)
	}
	defer cancel()
	started, err := h.acquireRun(waitCtx, request.Reference, request.Profile, "", profileMessageSource, request.Caller)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return startedRun{}, ErrSessionBusy
	}
	return started, err
}
