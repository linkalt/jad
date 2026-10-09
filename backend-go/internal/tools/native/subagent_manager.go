package native

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// subagentTaskPreviewBytes bounds the task text kept per run for the run list.
const subagentTaskPreviewBytes = 80

// Run states. A run is queued until it holds a slot, running while its child works,
// and ends as exactly one of done, failed, timed_out or stopped.
const (
	subagentRunQueued   = "queued"
	subagentRunRunning  = "running"
	subagentRunDone     = "done"
	subagentRunFailed   = "failed"
	subagentRunTimedOut = "timed_out"
	subagentRunStopped  = "stopped"
	// subagentRunLost is a run that was going when the server died. Its child died
	// with it; only the transcript it had written remains.
	subagentRunLost = "lost"
)

var (
	errSubagentManagerClosed = errors.New("sub-agent runs are shutting down")
	errSubagentQueueFull     = errors.New("too many sub-agent runs are going or waiting")
)

// SubagentManager owns the child runs of one storage directory. A run belongs to the
// manager, not to the call that started it, so it can outlive that call: a call that
// stops waiting hands the model the run's id and the child keeps working. The
// resolver shares one manager per storage directory, as it does for processes, and
// closes it at shutdown.
type SubagentManager struct {
	ctx    context.Context
	cancel context.CancelFunc
	active sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	runs     map[string]*subagentRun
	finished []string // ids of ended runs, oldest first
	running  int      // slots held
	queued   int      // runs waiting for a slot
	freed    chan struct{}
	// store keeps a manifest per run so ids survive a restart; nil keeps runs in
	// memory only.
	store *subagentStore
	// transcriptRoot, when set, is the storage directory whose child folders are
	// deleted as runs are dropped; see DeleteTranscriptsUnder.
	transcriptRoot string
}

// NewSubagentManager returns an empty manager that keeps its runs in memory only.
func NewSubagentManager() *SubagentManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &SubagentManager{ctx: ctx, cancel: cancel, runs: make(map[string]*subagentRun), freed: make(chan struct{})}
}

// SubagentManagerOptions configures a manager that keeps run manifests on disk.
type SubagentManagerOptions struct {
	// StorageDir is the directory the children's transcripts live in; the manifests
	// go in a folder inside it.
	StorageDir string
	// Keep is how many ended runs stay, as in tools.native.subagent.keep_finished.
	Keep   int
	Logger *slog.Logger
	// DeleteTranscripts deletes a run's transcript folder when keep_finished drops the
	// run, including at startup. Off keeps every folder.
	DeleteTranscripts bool
}

// NewPersistentSubagentManager returns a manager that writes a manifest per run and
// loads the manifests already in the storage directory. A run that was queued or
// running when the last server stopped becomes lost: its child died with the server.
// Ended runs stay readable by id. A manifest that cannot be read is skipped.
func NewPersistentSubagentManager(options SubagentManagerOptions) *SubagentManager {
	manager := NewSubagentManager()
	manager.store = newSubagentStore(options.StorageDir, options.Logger)
	if options.DeleteTranscripts {
		manager.transcriptRoot = filepath.Clean(options.StorageDir)
	}
	manager.recover(options.Keep)
	return manager
}

// recover loads the stored runs, marks the ones a restart interrupted as lost, and
// drops the oldest ended runs beyond keep.
func (m *SubagentManager) recover(keep int) {
	manifests := m.store.load()
	now := time.Now()
	for _, manifest := range manifests {
		run := restoredSubagentRun(manifest, m.store)
		if manifest.State == subagentRunQueued || manifest.State == subagentRunRunning {
			run.state = subagentRunLost
			run.reason = "the server stopped while the run was going"
			run.endedAt = now
			m.store.save(run)
		}
		m.runs[run.id] = run
		m.finished = append(m.finished, run.id)
	}
	sort.SliceStable(m.finished, func(i, j int) bool {
		a, b := m.runs[m.finished[i]], m.runs[m.finished[j]]
		if !a.endedAt.Equal(b.endedAt) {
			return a.endedAt.Before(b.endedAt)
		}
		return a.id < b.id
	})
	var dropped []string
	for len(m.finished) > keep {
		if dir := m.dropOldestLocked(); dir != "" {
			dropped = append(dropped, dir)
		}
	}
	m.removeTranscripts(dropped)
}

// dropOldestLocked forgets the oldest ended run and its manifest; the caller holds mu or
// is the only user of the manager. It returns the transcript folder to delete, or "" when
// the folder stays; the caller deletes it after releasing mu, so a slow disk never holds
// the lock.
func (m *SubagentManager) dropOldestLocked() string {
	id := m.finished[0]
	m.finished = m.finished[1:]
	run := m.runs[id]
	delete(m.runs, id)
	m.store.remove(id)
	if run == nil {
		return ""
	}
	run.mu.Lock()
	dir := run.dir
	run.mu.Unlock()
	if !transcriptFolderUnder(m.transcriptRoot, dir) {
		return ""
	}
	return dir
}

// DeleteTranscriptsUnder makes the manager delete a run's transcript folder when
// keep_finished drops the run, which is tools.native.subagent.delete_transcripts. Only a
// folder directly under root is ever deleted. Without it the folders stay and grow.
func (m *SubagentManager) DeleteTranscriptsUnder(root string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if root == "" {
		m.transcriptRoot = ""
		return
	}
	m.transcriptRoot = filepath.Clean(root)
}

// removeTranscripts deletes transcript folders chosen by dropOldestLocked. A failure is
// logged and does not stop the others.
func (m *SubagentManager) removeTranscripts(dirs []string) {
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			m.logger().Warn("delete sub-agent transcript folder", "dir", dir, "error", err)
		}
	}
}

func (m *SubagentManager) logger() *slog.Logger {
	if m.store != nil && m.store.logger != nil {
		return m.store.logger
	}
	return slog.New(slog.DiscardHandler)
}

// subagentRun is one child, from start to end.
type subagentRun struct {
	id string
	// owner is the session key of the call that started the run. Only calls of the
	// same session can see it, and a stop or reset of that session cancels it.
	owner     string
	agent     string
	task      string
	timeoutMS int64
	// changes is set when the agent can change files: its result then warns that an
	// unfinished run may have left changes behind.
	changes   bool
	startedAt time.Time
	cancel    context.CancelFunc
	// done closes once, after the final state is set.
	done chan struct{}

	mu      sync.Mutex
	state   string
	result  SubagentResult
	reason  string
	endedAt time.Time
	cut     bool   // the answer was longer than the limit and was cut
	dir     string // the child's transcript directory, once it exists

	store *subagentStore // nil when runs are not persisted
	// restored marks a run loaded from disk. Its text is not in memory: it is read
	// from the transcript the first time the run is looked at.
	restored   bool
	textLoaded bool
}

// subagentRunView is a copy of a run's state taken under its lock.
type subagentRunView struct {
	id, agent, task, state, reason string
	result                         SubagentResult
	cut                            bool
	changes                        bool
	dir                            string
	timeoutMS                      int64
	elapsed                        time.Duration
	startedAt, endedAt             time.Time
}

func (r *subagentRun) view() subagentRunView {
	r.mu.Lock()
	defer r.mu.Unlock()
	end := r.endedAt
	if end.IsZero() {
		end = time.Now()
	}
	return subagentRunView{id: r.id, agent: r.agent, task: r.task, state: r.state, reason: r.reason, result: r.result, cut: r.cut, changes: r.changes, dir: r.dir, timeoutMS: r.timeoutMS, elapsed: end.Sub(r.startedAt), startedAt: r.startedAt, endedAt: r.endedAt}
}

// active reports whether the run is queued or running.
func (r *subagentRun) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == subagentRunQueued || r.state == subagentRunRunning
}

func (r *subagentRun) setState(state string) {
	r.mu.Lock()
	r.state = state
	r.mu.Unlock()
}

func (r *subagentRun) setDir(dir string) {
	r.mu.Lock()
	r.dir = dir
	r.mu.Unlock()
	r.store.save(r)
}

// manifest is the run's state in the form written to disk.
func (r *subagentRun) manifest() subagentManifest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return subagentManifest{
		Version: subagentManifestVersion, ID: r.id, Owner: r.owner, Agent: r.agent, Task: r.task,
		TimeoutMS: r.timeoutMS, State: r.state, Reason: r.reason, StartedAt: r.startedAt, EndedAt: r.endedAt,
		Rounds: r.result.Rounds, Tokens: r.result.Tokens, Dir: r.dir, Cut: r.cut, Changes: r.changes,
	}
}

// restoredSubagentRun rebuilds an ended run from its manifest. Its done channel is
// closed, so a wait on it returns at once.
func restoredSubagentRun(manifest subagentManifest, store *subagentStore) *subagentRun {
	done := make(chan struct{})
	close(done)
	return &subagentRun{
		id: manifest.ID, owner: manifest.Owner, agent: manifest.Agent, task: manifest.Task,
		timeoutMS: manifest.TimeoutMS, startedAt: manifest.StartedAt, endedAt: manifest.EndedAt,
		cancel: func() {}, done: done, state: manifest.State, reason: manifest.Reason, cut: manifest.Cut, changes: manifest.Changes,
		dir: manifest.Dir, result: SubagentResult{Rounds: manifest.Rounds, Tokens: manifest.Tokens},
		store: store, restored: true,
	}
}

// loadText reads the text of a restored run from its transcript, once. A finished run
// gets its final answer; a run that was cut off or lost gets the newest text it wrote.
// A missing or unreadable transcript leaves the text empty. The text is cut to limit.
func (r *subagentRun) loadText(limit int64) {
	r.mu.Lock()
	if !r.restored || r.textLoaded {
		r.mu.Unlock()
		return
	}
	r.textLoaded = true
	dir, state := r.dir, r.state
	r.mu.Unlock()
	if dir == "" {
		return
	}
	answers, err := readTranscriptAnswers(filepath.Join(dir, "messages.jsonl"))
	if err != nil && answers.lastNonEmpty == "" {
		return
	}
	text := answers.lastNonEmpty
	if state == subagentRunDone {
		text = answers.last
	}
	text, cut := cutUTF8(text, limit)
	r.mu.Lock()
	r.result.Text = text
	r.cut = r.cut || cut
	r.mu.Unlock()
}

// subagentStart is what one new run needs from the tool that starts it.
type subagentStart struct {
	launcher SubagentLauncher
	request  SubagentRequest
	// owner is the session key of the starting call; see subagentRun.owner.
	owner string
	// changes is set when the agent can change files; see subagentRun.changes.
	changes bool
	// limit, maxQueued, keep and resultMax are the starting session's settings.
	limit     int
	maxQueued int
	keep      int
	resultMax int64
}

// start registers a run and launches its goroutine. It fails when the manager is
// closed or when the run would have to wait and the queue is full.
func (m *SubagentManager) start(spec subagentStart) (*subagentRun, error) {
	id, err := newSubagentRunID()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errSubagentManagerClosed
	}
	// The slot is taken here, not in the goroutine, so a free slot is never refused
	// because runs that were just started have not reached it yet.
	holdsSlot := m.running < spec.limit
	switch {
	case holdsSlot:
		m.running++
	case m.queued < spec.maxQueued:
		m.queued++
	default:
		m.mu.Unlock()
		return nil, errSubagentQueueFull
	}
	ctx, cancel := context.WithCancel(m.ctx)
	run := &subagentRun{
		store:     m.store,
		id:        id,
		owner:     spec.owner,
		agent:     spec.request.Agent.Name,
		task:      subagentTaskPreview(spec.request.Task),
		timeoutMS: spec.request.Agent.TimeoutMS,
		changes:   spec.changes,
		startedAt: time.Now(),
		cancel:    cancel,
		done:      make(chan struct{}),
		state:     subagentRunQueued,
	}
	if holdsSlot {
		run.state = subagentRunRunning
	}
	m.runs[id] = run
	m.active.Add(1)
	m.mu.Unlock()
	run.store.save(run)
	go m.execute(ctx, run, spec, holdsSlot)
	return run, nil
}

func (m *SubagentManager) execute(ctx context.Context, run *subagentRun, spec subagentStart, holdsSlot bool) {
	defer m.active.Done()
	defer run.cancel()
	if !holdsSlot {
		if !m.acquire(ctx, spec.limit) {
			m.finish(run, subagentRunStopped, SubagentResult{}, "", spec)
			return
		}
		run.setState(subagentRunRunning)
	}
	defer m.release()

	childContext, cancel := context.WithTimeout(ctx, time.Duration(spec.request.Agent.TimeoutMS)*time.Millisecond)
	defer cancel()
	spec.request.OnTranscript = run.setDir
	result, err := m.runLauncher(childContext, spec)
	switch {
	case err == nil:
		m.finish(run, subagentRunDone, result, "", spec)
	case ctx.Err() != nil:
		// Stopped by the model or by shutdown, not by the child's own limit.
		m.finish(run, subagentRunStopped, result, "", spec)
	case childContext.Err() != nil:
		m.finish(run, subagentRunTimedOut, result, "", spec)
	default:
		m.finish(run, subagentRunFailed, result, err.Error(), spec)
	}
}

// runLauncher turns a panic in the launcher into a failed run, because the goroutine
// has no caller that could recover it.
func (m *SubagentManager) runLauncher(ctx context.Context, spec subagentStart) (result SubagentResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("sub-agent launcher panicked: %v", recovered)
		}
	}()
	return spec.launcher.Run(ctx, spec.request)
}

// acquire waits for a free slot. It reports false when ctx ends first.
func (m *SubagentManager) acquire(ctx context.Context, limit int) bool {
	for {
		m.mu.Lock()
		if m.running < limit {
			m.running++
			m.queued--
			m.mu.Unlock()
			return true
		}
		freed := m.freed
		m.mu.Unlock()
		select {
		case <-freed:
		case <-ctx.Done():
			m.mu.Lock()
			m.queued--
			m.mu.Unlock()
			return false
		}
	}
}

func (m *SubagentManager) release() {
	m.mu.Lock()
	m.running--
	close(m.freed)
	m.freed = make(chan struct{})
	m.mu.Unlock()
}

// finish records the end of a run, wakes its waiters and drops the oldest ended runs
// beyond keep. A run that is still going is never dropped.
func (m *SubagentManager) finish(run *subagentRun, state string, result SubagentResult, reason string, spec subagentStart) {
	// The answer is cut once here, so a retained run never holds more than the limit.
	var cut bool
	result.Text, cut = cutUTF8(result.Text, spec.resultMax)
	run.mu.Lock()
	run.cut = cut
	run.state = state
	run.result = result
	run.reason = reason
	run.endedAt = time.Now()
	run.mu.Unlock()
	// The manifest is written before the waiters wake, so a read of the file after a
	// wait returns sees the end.
	run.store.save(run)

	var dropped []string
	m.mu.Lock()
	m.finished = append(m.finished, run.id)
	for len(m.finished) > spec.keep {
		if dir := m.dropOldestLocked(); dir != "" {
			dropped = append(dropped, dir)
		}
	}
	m.mu.Unlock()
	m.removeTranscripts(dropped)
	// Waiters wake last, so a wait that returns sees the disk already settled.
	close(run.done)
}

// get returns the run with the given id, or nil. A run of another session is nil too,
// so one session cannot read or stop the runs of another that shares the directory.
func (m *SubagentManager) get(id, owner string) *subagentRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	if run := m.runs[id]; run != nil && run.owner == owner {
		return run
	}
	return nil
}

// stop asks a run to end. It does not wait; the caller waits on run.done.
func (m *SubagentManager) stop(run *subagentRun) { run.cancel() }

// ownedRuns returns the retained runs of one session.
func (m *SubagentManager) ownedRuns(owner string) []*subagentRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	runs := make([]*subagentRun, 0, len(m.runs))
	for _, run := range m.runs {
		if run.owner == owner {
			runs = append(runs, run)
		}
	}
	return runs
}

// CancelOwner asks every queued or running child of one session to stop and reports
// how many it asked. It does not wait: each run ends as stopped once its child
// returns, and a later call by id reads that. The host calls it when the user stops
// the session or resets it, because a child handed off earlier would otherwise go on
// spending after the user asked for everything to stop. Runs already ended and runs
// of other sessions are left alone.
func (m *SubagentManager) CancelOwner(owner string) int {
	if m == nil {
		return 0
	}
	cancelled := 0
	for _, run := range m.ownedRuns(owner) {
		if run.active() {
			run.cancel()
			cancelled++
		}
	}
	return cancelled
}

// list returns the retained runs of one session, newest first.
func (m *SubagentManager) list(owner string) []subagentRunView {
	runs := m.ownedRuns(owner)
	views := make([]subagentRunView, 0, len(runs))
	for _, run := range runs {
		views = append(views, run.view())
	}
	sort.Slice(views, func(i, j int) bool {
		if !views[i].startedAt.Equal(views[j].startedAt) {
			return views[i].startedAt.After(views[j].startedAt)
		}
		return views[i].id < views[j].id
	})
	return views
}

// Closed reports whether Close has been called.
func (m *SubagentManager) Closed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// Close stops every run and waits for the children to return. Starting a run after
// Close fails.
func (m *SubagentManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.active.Wait()
	return nil
}

func newSubagentRunID() (string, error) {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("create sub-agent run id: %w", err)
	}
	return "sub_" + hex.EncodeToString(random[:]), nil
}

func subagentTaskPreview(task string) string {
	preview, cut := cutUTF8(oneLine(task), subagentTaskPreviewBytes)
	if cut {
		return preview + "..."
	}
	return preview
}
