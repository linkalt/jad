package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// remoteBackend is what /api/remote/v1 does in this process. A message is an ordinary run of
// the target profile (same locks, registry entry and stop route as a user's message); a
// sub-agent is a call of the profile's own subagent tool.
type remoteBackend struct {
	resolver   *Resolver
	runTimeout time.Duration
}

// RemoteBackend returns the backend for the remote API of this resolver.
func (r *Resolver) RemoteBackend(runTimeout time.Duration) remoteapi.Backend {
	return remoteBackend{resolver: r, runTimeout: runTimeout}
}

func (b remoteBackend) location(session string) (sessionfs.Location, error) {
	location, err := sessionfs.Resolve(b.resolver.roots, session)
	var resolveErr *sessionfs.ResolveError
	if errors.As(err, &resolveErr) {
		return sessionfs.Location{}, remoteapi.ErrNoSession
	}
	if err != nil {
		return sessionfs.Location{}, err
	}
	if !location.UsesLayout() {
		return sessionfs.Location{}, remoteapi.ErrNoSession
	}
	return location, nil
}

func (b remoteBackend) Profiles(_ context.Context, session string) ([]string, error) {
	location, err := b.location(session)
	if err != nil {
		return nil, err
	}
	info, err := sessionfs.ReadLayoutProfileInfo(location)
	if err != nil {
		return nil, err
	}
	return info.Profiles, nil
}

func (b remoteBackend) resolveProfile(ctx context.Context, session, profile string) (httpapi.ResolvedSession, error) {
	location, err := b.location(session)
	if err != nil {
		return httpapi.ResolvedSession{}, err
	}
	info, err := sessionfs.ReadLayoutProfileInfo(location)
	if err != nil {
		return httpapi.ResolvedSession{}, err
	}
	found := false
	for _, name := range info.Profiles {
		found = found || name == profile
	}
	if !found {
		return httpapi.ResolvedSession{}, remoteapi.ErrUnknownTarget
	}
	return b.resolver.Resolve(ctx, location.Reference, profile, "")
}

func (b remoteBackend) SendMessage(ctx context.Context, request remoteapi.MessageRequest) (remoteapi.Run, error) {
	starter := b.resolver.profileRunStarter()
	if starter == nil {
		return nil, &remoteapi.Refusal{Code: "unavailable", Message: "profile messages are unavailable on this server"}
	}
	resolved, err := b.resolveProfile(ctx, request.Session, request.Profile)
	if err != nil {
		return nil, err
	}
	run := &remoteMessageRun{done: make(chan struct{})}
	started, err := starter.StartProfileRun(ctx, httpapi.ProfileRunRequest{
		Reference: resolved.Reference,
		Profile:   resolved.Profile,
		Caller:    request.Caller,
		Chain:     []string{request.Caller, chainNode(resolved.Reference, resolved.Profile)},
		Message:   request.Message,
		Hops:      request.Hops,
		OnCommitted: func(event json.RawMessage) {
			if text := native.SubagentAnswerText(event); strings.TrimSpace(text) != "" {
				run.lastText.Store(text)
			}
		},
	})
	if errors.Is(err, httpapi.ErrSessionBusy) {
		return nil, remoteapi.ErrBusy
	}
	if err != nil {
		return nil, err
	}
	run.started = started
	timeout := b.runTimeout
	if timeout <= 0 {
		timeout = time.Hour
	}
	go run.watch(timeout)
	return run, nil
}

// remoteMessageRun adapts a started profile run to remoteapi.Run.
type remoteMessageRun struct {
	started  *httpapi.ProfileRun
	lastText atomic.Value
	done     chan struct{}

	mu     sync.Mutex
	reason string
	timed  bool
	result remoteapi.RunResult
}

func (r *remoteMessageRun) Done() <-chan struct{} { return r.done }

func (r *remoteMessageRun) Result() remoteapi.RunResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result
}

func (r *remoteMessageRun) Stop(reason string) {
	r.mu.Lock()
	if r.reason == "" {
		r.reason = reason
	}
	r.mu.Unlock()
	r.started.Stop()
}

func (r *remoteMessageRun) watch(timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-r.started.Done():
	case <-timer.C:
		r.mu.Lock()
		r.timed = true
		r.mu.Unlock()
		r.started.Stop()
		<-r.started.Done()
	}
	partial, _ := r.lastText.Load().(string)
	r.mu.Lock()
	defer r.mu.Unlock()
	result := remoteapi.RunResult{Reason: r.reason}
	switch {
	case r.timed:
		result.State, result.Text = "timed_out", partial
	case r.started.Status() == "completed":
		answer := native.SubagentAnswerText(r.started.Final())
		if strings.TrimSpace(answer) == "" {
			answer = partial
		}
		result.State, result.Text = "done", answer
		if strings.TrimSpace(answer) == "" {
			result.State, result.Code, result.Message = "failed", "profile_no_answer", "the profile finished without an answer"
		}
	case r.started.Status() == "aborted":
		result.State, result.Text = "stopped", partial
		if result.Reason == "" {
			result.Reason = "target_stopped"
		}
	default:
		result.State, result.Text, result.Code, result.Message = "failed", partial, "profile_failed", r.started.Failure()
	}
	r.result = result
	close(r.done)
}

func (b remoteBackend) StartSubagent(ctx context.Context, request remoteapi.SubagentRequest) (remoteapi.Run, error) {
	resolved, err := b.resolveProfile(ctx, request.Session, request.Profile)
	if err != nil {
		return nil, err
	}
	bound, ok := resolved.Runner.(boundRunner)
	if !ok {
		return nil, errors.New("remote sub-agent needs a bound session")
	}
	if bound.config.NativeSubagent == nil {
		return nil, &remoteapi.Refusal{Code: "subagent_unavailable", Message: "that profile has no subagent tool"}
	}
	known := false
	for _, agent := range bound.config.NativeSubagent.Agents {
		known = known || agent.Name == request.Agent
	}
	if !known {
		return nil, remoteapi.ErrUnknownTarget
	}
	config, err := bound.config.WithModel()
	if err != nil {
		return nil, mapSessionError(err)
	}
	catalog, _, _, _, err := b.resolver.buildRegisteredCatalog(ctx, config)
	if err != nil {
		return nil, err
	}
	arguments, _ := json.Marshal(map[string]any{"task": request.Task, "agent": request.Agent})
	call, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "remote_" + request.Agent, "name": "subagent", "arguments": string(arguments)})

	runCtx, cancel := context.WithCancel(context.Background())
	observed := make(chan *native.ExternalRun, 1)
	finished := make(chan json.RawMessage, 1)
	runCtx = native.WithExternalRun(runCtx, func(run *native.ExternalRun) { observed <- run })
	execution := tools.ExecutionContext{
		WorkspaceRoot: config.WorkingDir,
		SessionDir:    config.StateDir,
		SessionKey:    sessionKey(resolved.Reference, resolved.Profile),
		RemoteHops:    request.Hops,
	}
	go func() {
		finished <- callSubagentTool(func() json.RawMessage {
			output, _ := catalog.ExecuteFunctionCall(runCtx, call, execution)
			return output
		})
	}()
	select {
	case run := <-observed:
		go func() { <-run.Done(); cancel() }()
		return &remoteSubagentRun{run: run}, nil
	case output := <-finished:
		cancel()
		return nil, subagentStartFailure(output)
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

// callSubagentTool runs the tool call on the goroutine that observes the run it starts.
// Nothing above that goroutine recovers, so the boundary is here: a panic must fail the
// call, as it does on the batch path (session/tool_batch.go), not end the process. On a
// panic the call returns no output, which subagentStartFailure reports as "did not start
// a run" instead of the remote API goroutine taking the whole process down.
func callSubagentTool(call func() json.RawMessage) (output json.RawMessage) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("remote sub-agent tool call panicked", "error", recovered)
		}
	}()
	return call()
}

// subagentStartFailure turns the error result of a subagent call that never started a run
// into a client-safe refusal.
func subagentStartFailure(output json.RawMessage) error {
	var item struct {
		Output string `json:"output"`
	}
	_ = json.Unmarshal(output, &item)
	code := ""
	for _, line := range strings.Split(item.Output, "\n") {
		if value, ok := strings.CutPrefix(line, "error_code: "); ok {
			code = strings.TrimSpace(value)
		}
	}
	switch code {
	case "subagent_busy":
		return remoteapi.ErrBusy
	case "unknown_agent":
		return remoteapi.ErrUnknownTarget
	case "":
		return fmt.Errorf("the subagent tool did not start a run")
	}
	return &remoteapi.Refusal{Code: code, Message: "the sub-agent could not be started (" + code + ")"}
}

type remoteSubagentRun struct{ run *native.ExternalRun }

func (r *remoteSubagentRun) Done() <-chan struct{} { return r.run.Done() }
func (r *remoteSubagentRun) Stop(reason string)    { r.run.Stop(reason) }

func (r *remoteSubagentRun) Result() remoteapi.RunResult {
	state, text, reason := r.run.Outcome()
	result := remoteapi.RunResult{State: state, Text: text, Reason: reason}
	if state == "failed" {
		result.Code, result.Message = "subagent_failed", reason
	}
	return result
}

// ProfileInfo implements remoteapi.ProfileBackend: the context tokens of one profile, for a token
// whose globs reach it. A profile outside the globs answers like one that does not exist, and the
// read is the local one (profile_actions.go), so a remote never compacts or resets.
func (b remoteBackend) ProfileInfo(ctx context.Context, request remoteapi.ProfileRequest) (string, error) {
	if !request.MayProfile(request.Session + "/" + request.Profile) {
		return "", remoteapi.ErrUnknownTarget
	}
	location, err := b.location(request.Session)
	if err != nil {
		return "", err
	}
	info, err := sessionfs.ReadLayoutProfileInfo(location)
	if err != nil {
		return "", err
	}
	if !slices.Contains(info.Profiles, request.Profile) {
		return "", remoteapi.ErrUnknownTarget
	}
	tokens, err := profileMessenger{resolver: b.resolver}.ProfileInfo(ctx, request.Session, request.Profile)
	if err != nil {
		return "", err
	}
	return native.ProfileInfoText(request.Profile, tokens), nil
}
