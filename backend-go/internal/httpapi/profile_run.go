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
	// Model is the model ID (optionally @<effort>) the run uses; empty is the profile's default.
	Model string
	// Hops is the remote hop count of the request that caused this message (0 when none).
	Hops int
	// Depth is the run's depth across both kinds of run: its caller's RunDepth plus one, 0 for a
	// run no local run started. Its tools get it as ExecutionContext.ChainDepth, so the runs it
	// starts count max_depth from there; its AgentDepth stays 0, as it is a profile's own run.
	Depth int
	// RunNode is the run's node in the caller's run tree (native.RunTree); its tools get it as
	// ExecutionContext.RunNode, so the runs it starts are counted in that tree. Empty for a run no
	// local run started.
	RunNode string
	// Wait queues behind the run that holds the transcript, in the order user messages
	// queue, instead of failing at once with ErrSessionBusy. WaitLimit bounds that wait;
	// zero waits until ctx ends. The limit ending while ctx lives is ErrSessionBusy.
	Wait      bool
	WaitLimit time.Duration
	// OnCommitted sees every response the run commits, on the run's goroutine.
	OnCommitted func(event json.RawMessage)
	// BeforeRun, when set, runs once the transcript is held and the run is registered,
	// before the target run begins, so nothing can take the transcript in between. An error
	// fails the start like any that ends before the run: the run is finished as failed, the
	// transcript released, OnEnd called, and StartProfileRun returns the error.
	BeforeRun func(ctx context.Context) error
	// OnEnd runs once the run has ended and the transcript is released, also when the start fails.
	OnEnd func(*ProfileRun)
	// Confine, when set, turns the target's runner into the one the run uses: the target's
	// session narrowed by the caller's sandbox (profile_message, D20). An error fails the start.
	Confine func(RunSession) (RunSession, error)
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
		started, busy, err = h.tryStartRun(ctx, request.Reference, request.Profile, request.Model, profileMessageSource, request.Caller)
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
	launched := false
	defer func() {
		if !launched {
			run.finish("failed", failureEvents("The run failed before it started."))
			started.processUnlock()
			run.callOnEnd(handle)
		}
	}()
	run.mu.Lock()
	run.supportsInteraction = true
	run.chain = append([]string(nil), request.Chain...)
	run.hops = request.Hops
	run.depth = request.Depth
	run.runNode = request.RunNode
	run.onEnd = request.OnEnd
	run.mu.Unlock()
	runner := started.resolved.Runner
	if request.Confine != nil {
		// Fails like any start that ends before the run: the deferred cleanup runs OnEnd.
		if runner, err = request.Confine(runner); err != nil {
			return nil, err
		}
	}
	if request.BeforeRun != nil {
		if err := request.BeforeRun(ctx); err != nil {
			return nil, err
		}
	}
	run.onCommitted = request.OnCommitted
	run.onResult = func(event json.RawMessage) { handle.final = event }
	h.attachContentCache(run)
	launched = true
	go func() {
		// Deferred calls run last in, first out: the transcript lock is released before OnEnd runs.
		defer run.callOnEnd(handle)
		defer started.processUnlock()
		h.executeRun(run, runner, input)
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

// callOnEnd runs the run's OnEnd hook once the run has ended. Each caller runs it after it has
// released the transcript lock, so the hook never runs under that lock. The hook is taken, so
// it runs at most once.
func (r *activeRun) callOnEnd(handle *ProfileRun) {
	r.mu.Lock()
	if r.status == "running" || r.onEnd == nil {
		r.mu.Unlock()
		return
	}
	onEnd := r.onEnd
	r.onEnd = nil
	r.mu.Unlock()
	onEnd(handle)
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
	started, err := h.acquireRun(waitCtx, request.Reference, request.Profile, request.Model, profileMessageSource, request.Caller)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return startedRun{}, ErrSessionBusy
	}
	return started, err
}
