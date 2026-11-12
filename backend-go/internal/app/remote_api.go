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
	// runs maps the remote run ids of the sub-agent runs this backend started ("rr_…") to the
	// local runs ("sub_…"). It lives in memory with the backend, so a restarted server does not
	// know the runs of the one before (see DECISIONS.md, remote resume).
	runs *remoteRunIndex
}

// remoteRunIndex is the map from the remote run id to the local run it started. The map is
// bounded by the manager's keep_finished: a run dropped from its manager is forgotten here too.
type remoteRunIndex struct {
	mu   sync.Mutex
	runs map[string]remoteRunRef
	// byLocal is the remote id of each local run in runs, keyed by the run's identity, which is never
	// drawn again. A local id can be, so a forget keyed by the id could remove a newer run's entry.
	byLocal map[any]string
	// byID is the remote id of each local run in runs, keyed by its local id, so a resume's source is
	// found without a scan. An id can be drawn again, so an entry is kept only while its identity is.
	byID map[string]remoteIDEntry
	// byManager holds the identities of the runs each manager started through this index, so a
	// manager that closes forgets only its own entries.
	byManager map[*native.SubagentManager]map[any]struct{}
}

// remoteIDEntry is the remote id of a local run and the identity it was recorded under.
type remoteIDEntry struct {
	remoteID string
	identity any
}

// remoteRunRef is the local run a remote run id names, and the session it belongs to. identity names
// the run for as long as its manager keeps it, and manager is the manager that holds it.
type remoteRunRef struct {
	local, session string
	identity       any
	manager        *native.SubagentManager
}

// newRemoteRunIndex returns an empty index.
func newRemoteRunIndex() *remoteRunIndex {
	return &remoteRunIndex{runs: make(map[string]remoteRunRef), byLocal: make(map[any]string), byID: make(map[string]remoteIDEntry), byManager: make(map[*native.SubagentManager]map[any]struct{})}
}

// record maps a remote id to the local run it started, when the manager still keeps that run. A run
// that was dropped before the hook went in is not recorded, so no entry outlives its run.
func (i *remoteRunIndex) record(remoteID string, ref remoteRunRef) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.runs[remoteID] = ref
	i.byLocal[ref.identity] = remoteID
	i.byID[ref.local] = remoteIDEntry{remoteID, ref.identity}
	if i.byManager[ref.manager] == nil {
		i.byManager[ref.manager] = make(map[any]struct{})
	}
	i.byManager[ref.manager][ref.identity] = struct{}{}
}

// forget drops the entry of a run its manager has dropped; a later resume of its remote id is unknown.
func (i *remoteRunIndex) forget(identity any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	remoteID, ok := i.byLocal[identity]
	if !ok {
		return
	}
	ref := i.runs[remoteID]
	i.removeLocked(remoteID, ref, identity)
}

// forgetManager drops every entry of a manager that closed. Only that manager's identities are visited.
func (i *remoteRunIndex) forgetManager(manager *native.SubagentManager) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for identity := range i.byManager[manager] {
		remoteID := i.byLocal[identity]
		i.removeLocked(remoteID, i.runs[remoteID], identity)
	}
}

// size is the number of remote ids the index holds.
func (i *remoteRunIndex) size() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.runs)
}

// removeLocked drops one entry from every map. The caller holds mu.
func (i *remoteRunIndex) removeLocked(remoteID string, ref remoteRunRef, identity any) {
	delete(i.byLocal, identity)
	delete(i.runs, remoteID)
	if entry, ok := i.byID[ref.local]; ok && entry.identity == identity {
		delete(i.byID, ref.local)
	}
	if identities := i.byManager[ref.manager]; identities != nil {
		delete(identities, identity)
		if len(identities) == 0 {
			delete(i.byManager, ref.manager)
		}
	}
}

// remoteIDOf is the remote id of the local run local, or local itself when this backend did not start it
// (a run the server started on its own, or a resume that has no remote name).
func (i *remoteRunIndex) remoteIDOf(local string) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	if entry, ok := i.byID[local]; ok {
		return entry.remoteID
	}
	return local
}

// lookup is the run a remote id names, only when the run belongs to session: the same id sent to
// another session or profile is not found.
func (i *remoteRunIndex) lookup(remoteID, session string) (remoteRunRef, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	ref, ok := i.runs[remoteID]
	return ref, ok && ref.session == session
}

// RemoteBackend returns the backend for the remote API of this resolver.
func (r *Resolver) RemoteBackend(runTimeout time.Duration) remoteapi.Backend {
	r.remoteRuns = newRemoteRunIndex()
	return remoteBackend{resolver: r, runTimeout: runTimeout, runs: r.remoteRuns}
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
	if b.resolver.profileRunStarter() == nil {
		return nil, &remoteapi.Refusal{Code: "unavailable", Message: "profile messages are unavailable on this server"}
	}
	resolved, err := b.resolveProfile(ctx, request.Session, request.Profile)
	if err != nil {
		return nil, err
	}
	run := &remoteMessageRun{done: make(chan struct{})}
	handle, err := b.resolver.StartRun(ctx, RunSpec{Kind: RunPersistent, Profile: httpapi.ProfileRunRequest{
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
		OnEnd: run.ended,
		// The target's own sandbox.isolation applies as for a local profile_message: an
		// isolated target works in a git worktree of its own and the result says where its
		// work is. The caller's sandbox is on its own server and does not narrow this one.
		// Nor does its tools.deny (U3): no deny crosses the remote hop, so the run has the
		// target's own tools only. Nor do its model keys (T-009): the run hides its own models'.
		Confine: targetRunner(nil, request.Caller, request.Message, run.setWorktree, nil, nil, nil),
	}})
	if errors.Is(err, httpapi.ErrSessionBusy) {
		return nil, remoteapi.ErrBusy
	}
	if err != nil {
		return nil, err
	}
	timeout := b.runTimeout
	if timeout <= 0 {
		timeout = time.Hour
	}
	run.arm(handle.ProfileRun(), timeout)
	return run, nil
}

// remoteMessageRun adapts a started profile run to remoteapi.Run.
type remoteMessageRun struct {
	started  *httpapi.ProfileRun
	lastText atomic.Value
	done     chan struct{}

	mu       sync.Mutex
	reason   string
	timed    bool
	finished bool
	timer    *time.Timer
	result   remoteapi.RunResult
	// worktree are the worktree_* lines of a target that worked in a worktree; set before
	// the run ends.
	worktree []remoteapi.WorktreeLine
}

// setWorktree keeps the end of the target's worktree for the run's result.
func (r *remoteMessageRun) setWorktree(result native.DelegatedWorktreeResult) {
	lines := remoteWorktreeLines(result.Information)
	r.mu.Lock()
	r.worktree = lines
	r.mu.Unlock()
}

// remoteWorktreeLines are worktree_* lines as the remote API carries them.
func remoteWorktreeLines(information []tools.Information) []remoteapi.WorktreeLine {
	var lines []remoteapi.WorktreeLine
	for _, item := range information {
		lines = append(lines, remoteapi.WorktreeLine{Key: item.Key, Value: fmt.Sprint(item.Value)})
	}
	return lines
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

// arm starts the run's timeout. It runs after StartProfileRun returns, so a run that ends first
// is already finished here and needs no timer.
func (r *remoteMessageRun) arm(started *httpapi.ProfileRun, timeout time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = started
	if !r.finished {
		r.timer = time.AfterFunc(timeout, r.expire)
	}
}

// expire is the timeout. It is a no-op once the run has ended: the timer is stopped on end, and
// the finished flag covers a callback that already started.
func (r *remoteMessageRun) expire() {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.timed = true
	r.mu.Unlock()
	r.started.Stop()
}

// ended is the run's OnEnd hook. httpapi calls it after the run has ended and its transcript lock
// is released, once. It computes the result and closes done.
func (r *remoteMessageRun) ended(started *httpapi.ProfileRun) {
	r.mu.Lock()
	r.finished = true
	if r.timer != nil {
		r.timer.Stop()
	}
	timed, reason, worktree := r.timed, r.reason, r.worktree
	r.mu.Unlock()
	partial, _ := r.lastText.Load().(string)
	result := remoteapi.RunResult{Reason: reason, Worktree: worktree}
	switch {
	case timed:
		result.State, result.Text = "timed_out", partial
	case started.Status() == "completed":
		answer := native.SubagentAnswerText(started.Final())
		if strings.TrimSpace(answer) == "" {
			answer = partial
		}
		result.State, result.Text = "done", answer
		if strings.TrimSpace(answer) == "" {
			result.State, result.Code, result.Message = "failed", "profile_no_answer", "the profile finished without an answer"
		}
	case started.Status() == "aborted":
		result.State, result.Text = "stopped", partial
		if result.Reason == "" {
			result.Reason = "target_stopped"
		}
	default:
		result.State, result.Text, result.Code, result.Message = "failed", partial, "profile_failed", started.Failure()
	}
	r.mu.Lock()
	r.result = result
	r.mu.Unlock()
	close(r.done)
}

// afterRemoteObserved is a test seam: nil in production, and read only by StartSubagent. A test sets
// it to pause a remote start after its run is observed and before it is recorded; it may block until
// the test releases it.
var afterRemoteObserved func()

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
	config, err := remoteSubagentBase(bound.config)
	if err != nil {
		return nil, mapSessionError(err)
	}
	catalog, _, _, err := b.resolver.buildRegisteredCatalog(ctx, config, nil)
	if err != nil {
		return nil, err
	}
	values := map[string]any{"task": request.Task, "agent": request.Agent}
	if request.Resume != "" {
		// The remote run id names a local run this backend started; the local resume checks
		// that the caller owns it. A run another session started is unknown, as it is locally.
		ref, ok := b.runs.lookup(request.Resume, sessionKey(resolved.Reference, resolved.Profile))
		if !ok {
			return nil, &remoteapi.Refusal{Code: "unknown_run", Message: "No sub-agent run " + request.Resume + " in this session"}
		}
		values["resume"] = ref.local
	}
	arguments, _ := json.Marshal(values)
	call, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "remote_" + request.Agent, "name": "subagent", "arguments": string(arguments)})

	runCtx, cancel := context.WithCancel(context.Background())
	observed := make(chan *native.ExternalRun, 1)
	finished := make(chan json.RawMessage, 1)
	runCtx = native.WithExternalRun(runCtx, func(run *native.ExternalRun) { observed <- run })
	// The caller is the token the request authenticated as (handleStart sets it), never a value
	// from the body; the resume branch reads it and nothing else does.
	runCtx = native.WithRemoteCaller(runCtx, request.Caller)
	execution := tools.ExecutionContext{
		WorkspaceRoot: config.WorkingDir,
		SessionDir:    config.StateDir,
		SessionKey:    sessionKey(resolved.Reference, resolved.Profile),
		RemoteHops:    request.Hops,
		Environment:   runEnvironment(config),
	}
	go func() {
		finished <- callSubagentTool(func() json.RawMessage {
			output, _ := catalog.ExecuteFunctionCall(runCtx, call, execution)
			return output
		})
	}()
	select {
	case run := <-observed:
		// The hook goes in before the run is recorded, so a prune after this point is forgotten by
		// the hook.
		manager := b.resolver.sharedSubagentManager(config)
		if manager != nil {
			manager.SetOnDropped(b.runs.forget)
			manager.SetOnClosed(func() { b.runs.forgetManager(manager) })
		}
		if afterRemoteObserved != nil {
			afterRemoteObserved()
		}
		// Record first, then check: a drop that lands before the record is seen by Retains, and one
		// that lands after it is forgotten by the hook. The index lock is never held across a
		// manager call, so the two locks never nest.
		b.runs.record(request.RunID, remoteRunRef{local: run.ID(), session: sessionKey(resolved.Reference, resolved.Profile), identity: run.Identity(), manager: manager})
		if manager != nil && !manager.Retains(run.Identity()) {
			b.runs.forget(run.Identity())
		}
		go func() { <-run.Done(); cancel() }()
		return &remoteSubagentRun{run: run, runs: b.runs}, nil
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

type remoteSubagentRun struct {
	run  *native.ExternalRun
	runs *remoteRunIndex
}

func (r *remoteSubagentRun) Done() <-chan struct{} { return r.run.Done() }
func (r *remoteSubagentRun) Stop(reason string)    { r.run.Stop(reason) }

func (r *remoteSubagentRun) Result() remoteapi.RunResult {
	state, text, reason := r.run.Outcome()
	resumedFrom, configNote := r.run.ResumeFacts()
	result := remoteapi.RunResult{State: state, Text: text, Reason: reason, ResumedFrom: r.runs.remoteIDOf(resumedFrom), ConfigNote: configNote,
		Worktree: remoteWorktreeLines(r.run.WorktreeInformation())}
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
	tokens, err := profileMessenger{resolver: b.resolver}.ProfileInfo(ctx, request.Session, request.Session, request.Profile)
	if err != nil {
		return "", err
	}
	return native.ProfileInfoText(request.Profile, tokens), nil
}

// count is how many remote runs the index holds; a test reads it.
func (i *remoteRunIndex) count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.runs)
}

// entry is the local run and session of a remote id; a test reads it.
func (i *remoteRunIndex) entry(remoteID string) (remoteRunRef, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	ref, ok := i.runs[remoteID]
	return ref, ok
}

// remoteSubagentBase is the configuration of a remote sub-agent run, which starts here: the
// profile's own models are read once, as for a run (decider 3c.2b), and the child's model loads
// from that copy (childConfig).
func remoteSubagentBase(config sessionfs.SessionConfig) (sessionfs.SessionConfig, error) {
	return config.PinOwnModels().WithModel()
}
