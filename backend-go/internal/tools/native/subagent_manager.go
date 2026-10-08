package native

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
}

// NewSubagentManager returns an empty manager.
func NewSubagentManager() *SubagentManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &SubagentManager{ctx: ctx, cancel: cancel, runs: make(map[string]*subagentRun), freed: make(chan struct{})}
}

// subagentRun is one child, from start to end.
type subagentRun struct {
	id        string
	agent     string
	task      string
	timeoutMS int64
	startedAt time.Time
	cancel    context.CancelFunc
	// done closes once, after the final state is set.
	done chan struct{}

	mu      sync.Mutex
	state   string
	result  SubagentResult
	reason  string
	endedAt time.Time
	cut     bool // the answer was longer than the limit and was cut
}

// subagentRunView is a copy of a run's state taken under its lock.
type subagentRunView struct {
	id, agent, task, state, reason string
	result                         SubagentResult
	cut                            bool
	timeoutMS                      int64
	elapsed                        time.Duration
}

func (r *subagentRun) view() subagentRunView {
	r.mu.Lock()
	defer r.mu.Unlock()
	end := r.endedAt
	if end.IsZero() {
		end = time.Now()
	}
	return subagentRunView{id: r.id, agent: r.agent, task: r.task, state: r.state, reason: r.reason, result: r.result, cut: r.cut, timeoutMS: r.timeoutMS, elapsed: end.Sub(r.startedAt)}
}

func (r *subagentRun) setState(state string) {
	r.mu.Lock()
	r.state = state
	r.mu.Unlock()
}

// subagentStart is what one new run needs from the tool that starts it.
type subagentStart struct {
	launcher SubagentLauncher
	request  SubagentRequest
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
		id:        id,
		agent:     spec.request.Agent.Name,
		task:      subagentTaskPreview(spec.request.Task),
		timeoutMS: spec.request.Agent.TimeoutMS,
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
	close(run.done)

	m.mu.Lock()
	m.finished = append(m.finished, run.id)
	for len(m.finished) > spec.keep {
		delete(m.runs, m.finished[0])
		m.finished = m.finished[1:]
	}
	m.mu.Unlock()
}

// get returns the run with the given id, or nil.
func (m *SubagentManager) get(id string) *subagentRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[id]
}

// stop asks a run to end. It does not wait; the caller waits on run.done.
func (m *SubagentManager) stop(run *subagentRun) { run.cancel() }

// list returns the retained runs, newest first.
func (m *SubagentManager) list() []subagentRunView {
	m.mu.Lock()
	runs := make([]*subagentRun, 0, len(m.runs))
	for _, run := range m.runs {
		runs = append(runs, run)
	}
	m.mu.Unlock()
	views := make([]subagentRunView, 0, len(runs))
	for _, run := range runs {
		views = append(views, run.view())
	}
	sort.Slice(views, func(i, j int) bool { return views[i].elapsed < views[j].elapsed })
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
