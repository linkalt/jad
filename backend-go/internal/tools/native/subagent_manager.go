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
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

// subagentTaskPreviewBytes bounds the task text kept per run for the run list.
const subagentTaskPreviewBytes = 80

// closeGrace is how long Close waits for the children to return after cancelling them.
// A child still going after it is logged and left behind, so shutdown is bounded.
const closeGrace = 2 * time.Second

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
	errSubagentDepth         = errors.New("sub-agent nesting is as deep as tools.native.subagent.max_depth allows")
	errSubagentManagerClosed = errors.New("sub-agent runs are shutting down")
	errSubagentQueueFull     = errors.New("too many sub-agent runs are going or waiting")
	// A nested start (a child starting a child) is never queued: a parent that holds a slot
	// and waits for a queued child of its own could never be freed.
	errSubagentNoSlot   = errors.New("no free sub-agent slot: a run started from inside a sub-agent is not queued; try again later or do the work yourself")
	errSubagentRunLimit = errors.New("this sub-agent tree or session already has the most runs it may have going")
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
	// writers counts the runs of each owner that can change files and run as handles,
	// from reservation to end; see reserveWriter.
	writers map[string]int
	// byRoot and bySession count the runs going (queued or running) per top-level run and
	// per session, for the nested-start limits.
	byRoot    map[string]int
	bySession map[string]int
	queued    int // runs waiting for a slot
	freed     chan struct{}
	// store keeps a manifest per run so ids survive a restart; nil keeps runs in
	// memory only.
	store *subagentStore
	// removeManifest deletes the manifest of a pruned run; it is a field so a test can
	// hold the pruner inside the removal. The store sets it.
	removeManifest func(id string)
	// newRunID makes the id of a new run; it is a field so a test can force a collision.
	newRunID func() (string, error)
	// now reads the clock for the run times that the lists and the pruning order by; it is a
	// field so a test can give runs an exact order.
	now func() time.Time
	// transcriptRoot, when set, is the storage directory whose child folders are
	// deleted as runs are dropped; see DeleteTranscriptsUnder.
	transcriptRoot string
}

// NewSubagentManager returns an empty manager that keeps its runs in memory only.
func NewSubagentManager() *SubagentManager {
	ctx, cancel := context.WithCancel(context.Background())
	manager := &SubagentManager{ctx: ctx, cancel: cancel, runs: make(map[string]*subagentRun), writers: make(map[string]int), byRoot: make(map[string]int), bySession: make(map[string]int), freed: make(chan struct{})}
	// A nil store removes nothing, so a memory-only manager keeps no manifests.
	manager.removeManifest = manager.store.remove
	manager.newRunID = newSubagentRunID
	manager.now = time.Now
	return manager
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
	manager.removeManifest = manager.store.remove
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
	now := m.now()
	for _, manifest := range manifests {
		// A run of another server that is still going on this directory is not ours:
		// marking it lost or pruning it would corrupt that server's runs.
		if manifest.Server != nil && foreignServerAlive(m.store.identity, *manifest.Server) {
			m.logger().Info("leave the sub-agent runs of another running server alone", "run", manifest.ID, "pid", manifest.Server.PID)
			continue
		}
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
	var dropped []droppedRun
	for len(m.finished) > keep {
		dropped = append(dropped, m.dropOldestLocked())
	}
	m.removeDropped(dropped)
}

// droppedRun is a run that was forgotten under mu: its manifest and, when it is to be
// deleted, its transcript folder. Both are removed by removeDropped after mu is released.
type droppedRun struct {
	id  string
	dir string
}

// dropOldestLocked forgets the oldest ended run. The caller holds mu or is the only user of
// the manager, and must pass the result to removeDropped after releasing mu, so a slow disk
// never holds the lock.
func (m *SubagentManager) dropOldestLocked() droppedRun {
	id := m.finished[0]
	m.finished = m.finished[1:]
	run := m.runs[id]
	delete(m.runs, id)
	dropped := droppedRun{id: id}
	if run == nil {
		return dropped
	}
	run.mu.Lock()
	dir := run.dir
	run.mu.Unlock()
	if transcriptFolderUnder(m.transcriptRoot, dir) {
		dropped.dir = dir
	}
	return dropped
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

// removeDropped deletes the manifests and transcript folders chosen by dropOldestLocked. The
// caller must not hold mu: a manifest removal waits for the store, which a save holds across
// its fsync. A failure is logged and does not stop the others.
func (m *SubagentManager) removeDropped(dropped []droppedRun) {
	for _, run := range dropped {
		m.removeManifest(run.id)
		if run.dir == "" {
			continue
		}
		if err := os.RemoveAll(run.dir); err != nil {
			m.logger().Warn("delete sub-agent transcript folder", "dir", run.dir, "error", err)
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
	owner string
	// handleOwner owns the run's async handle: the session key, or the run id of the
	// parent run for a run a child starts (see handleOwner).
	handleOwner string
	// session is the session of the whole run tree and root the top-level run (its own id
	// for one); they key the nested-start limits.
	session   string
	root      string
	agent     string
	task      string
	timeoutMS int64
	// changes is set when the agent can change files: its result then warns that an
	// unfinished run may have left changes behind.
	changes   bool
	resultMax int64
	startedAt time.Time
	cancel    context.CancelFunc
	ctx       context.Context
	// done closes once, after the final state is set.
	done chan struct{}

	mu     sync.Mutex
	state  string
	result SubagentResult
	reason string
	// stopReason is why the host stopped the run; the final stopped state keeps it as reason.
	stopReason string
	endedAt    time.Time
	cut        bool   // the answer was longer than the limit and was cut
	dir        string // the child's transcript directory, once it exists
	// files are the files the child's write and edit changed, filesOmitted how many
	// more there were than the list keeps.
	files        []subagentFileChange
	filesOmitted int
	// processesStopped is how many background processes the child left running that
	// the run's end stopped. Kept in memory only.
	processesStopped int
	// countedWriter is set when the run holds one of its owner's background writer places,
	// which finish gives back.
	countedWriter bool
	// note says why a writer that could have been handed off was not; it rides on the result.
	note string
	// configNote says the agent definition changed since a resumed run's source; see configChangedNote.
	configNote string
	// config is the agent definition's fingerprint, written into the manifest; see subagentFingerprint.
	config string
	// resumedFrom is the run this one continues from; empty for a run that does not.
	resumedFrom string
	// caller is the remote caller the run was started for ("remote:<token id>"); "" for a
	// local run. A remote resume may use only a run whose caller is its own.
	caller string
	// resumedConfig is the source's fingerprint; see subagentStore's ResumedConfig.
	resumedConfig string
	// tracked is set when the run lists its changed files (changes and a non-zero
	// changed_files_max); without it the result says nothing about files.
	tracked bool
	// async reports the run to the async manager as a handle; nil for a run that is not one
	// (a writer, or a session without the async tool).
	async *subagentAsyncHandle

	store *subagentStore // nil when runs are not persisted
	// restored marks a run loaded from disk. Its text is not in memory: it is read
	// from the transcript the first time the run is looked at; a reader that arrives
	// during that read waits for it.
	restored bool
	textOnce sync.Once
}

// subagentRunView is a copy of a run's state taken under its lock.
type subagentRunView struct {
	id, agent, task, state, reason string
	result                         SubagentResult
	resultMax                      int64
	cut                            bool
	changes                        bool
	files                          []subagentFileChange
	filesOmitted                   int
	tracked                        bool
	note                           string
	configNote                     string
	resumedFrom                    string
	processesStopped               int
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
	return subagentRunView{id: r.id, agent: r.agent, task: r.task, state: r.state, reason: r.reason, result: r.result, resultMax: r.resultMax, cut: r.cut, changes: r.changes, files: append([]subagentFileChange(nil), r.files...), filesOmitted: r.filesOmitted, tracked: r.tracked, note: r.note, configNote: r.configNote, resumedFrom: r.resumedFrom, processesStopped: r.processesStopped, dir: r.dir, timeoutMS: r.timeoutMS, elapsed: end.Sub(r.startedAt), startedAt: r.startedAt, endedAt: r.endedAt}
}

// stopWith cancels the run and keeps why, so the stopped result can say it. A run that
// is already being stopped (by the model, a reset or shutdown) keeps no reason of ours.
func (r *subagentRun) stopWith(reason string) {
	r.mu.Lock()
	if r.stopReason == "" && r.ctx.Err() == nil {
		r.stopReason = reason
	}
	r.mu.Unlock()
	r.cancel()
}

func (r *subagentRun) takeStopReason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopReason
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

// setProcessesStopped records how many processes the run's end stopped.
func (r *subagentRun) setProcessesStopped(count int) {
	r.mu.Lock()
	r.processesStopped = count
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
		Version: subagentManifestVersion, ID: r.id, Owner: r.owner, Session: r.session, Root: r.root, Agent: r.agent, Task: r.task,
		TimeoutMS: r.timeoutMS, State: r.state, Reason: r.reason, StartedAt: r.startedAt, EndedAt: r.endedAt,
		Rounds: r.result.Rounds, Tokens: r.result.Tokens, Dir: r.dir, Cut: r.cut, Changes: r.changes, Files: append([]subagentFileChange(nil), r.files...), FilesOmitted: r.filesOmitted, Tracked: r.tracked, Config: r.config, ResumedFrom: r.resumedFrom, ResumedConfig: r.resumedConfig, Caller: r.caller,
	}
}

// restoredSubagentRun rebuilds an ended run from its manifest. Its done channel is
// closed, so a wait on it returns at once.
func restoredSubagentRun(manifest subagentManifest, store *subagentStore) *subagentRun {
	done := make(chan struct{})
	close(done)
	// The run has ended, so its context is already cancelled: a stop on it records no reason.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return &subagentRun{
		id: manifest.ID, owner: manifest.Owner, session: manifest.Session, root: manifest.Root, agent: manifest.Agent, task: manifest.Task,
		timeoutMS: manifest.TimeoutMS, startedAt: manifest.StartedAt, endedAt: manifest.EndedAt,
		cancel: cancel, ctx: ctx, done: done, state: manifest.State, reason: manifest.Reason, cut: manifest.Cut, changes: manifest.Changes, files: manifest.Files, filesOmitted: manifest.FilesOmitted, tracked: manifest.Tracked,
		dir: manifest.Dir, result: SubagentResult{Rounds: manifest.Rounds, Tokens: manifest.Tokens},
		store: store, restored: true, config: manifest.Config, resumedFrom: manifest.ResumedFrom, resumedConfig: manifest.ResumedConfig, caller: manifest.Caller,
		configNote: configNoteFor(resumeStart{from: manifest.ResumedFrom, config: manifest.ResumedConfig}, manifest.Config),
	}
}

// loadText reads the text of a restored run from its transcript, once. A finished run
// gets its final answer; a run that was cut off or lost gets the newest text it wrote.
// A missing or unreadable transcript leaves the text empty. The text is kept whole: a
// reader cuts what it returns, so the first reader's limit does not decide what later
// readers see.
func (r *subagentRun) loadText() {
	if r.restored {
		r.textOnce.Do(r.readText)
	}
}

func (r *subagentRun) readText() {
	r.mu.Lock()
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
	r.mu.Lock()
	r.result.Text = text
	r.mu.Unlock()
}

// subagentStart is what one new run needs from the tool that starts it.
type subagentStart struct {
	launcher SubagentLauncher
	request  SubagentRequest
	// owner is the session key of the starting call; see subagentRun.owner.
	owner string
	// handleOwner owns the async handle; see subagentRun.handleOwner.
	handleOwner string
	// session is the session the whole tree belongs to. depth is the AgentDepth of the
	// starting call (0 for the session's own call), rootRun the top-level run it runs
	// under and parentID the run it runs in; the last two are empty at depth 0.
	session  string
	depth    int
	rootRun  string
	parentID string
	// maxDepth, maxPerRoot and maxPerSession bound nested starts; zero means 1 level
	// deep and no count limit.
	maxDepth      int
	maxPerRoot    int
	maxPerSession int
	// changes is set when the agent can change files; see subagentRun.changes.
	changes bool
	// limit, maxQueued, keep and resultMax are the starting session's settings.
	limit     int
	maxQueued int
	// processes, when set, is the manager whose processes the run started; the run's end
	// stops them. See stopRunProcesses.
	processes ProcessManager
	// changedMax is how many changed files the run lists; see recordChange.
	changedMax int
	// processMax is how many processes one child may have running; see
	// ExecutionContext.ProcessOwnerLimit.
	processMax int
	keep       int
	resultMax  int64
	// async, when set, makes the run a handle of the async manager owned by owner.
	async *subagentAsyncHandle
	// asyncHost, when set, is where the child's own handles (owner = the run id) live; the
	// run's end stops and removes them. It is set for writers too, which are no handles.
	asyncHost *AsyncHost
	// childAsync is set when the child has the async tool, so its processes are handles.
	childAsync bool
	// writerReserved is set when the caller took one of the owner's background writer
	// places with reserveWriter; the run gives it back when it ends.
	writerReserved bool
	// note is carried on the run's result; see subagentRun.note.
	note string
	// configNote says the agent definition changed since a resumed run's source; see configChangedNote.
	configNote string
	// config is the agent definition's fingerprint; see subagentRun.config.
	config string
	// resumedFrom is the run this one continues from; see subagentRun.resumedFrom.
	resumedFrom string
	// caller is the remote caller of the run; see subagentRun.caller.
	caller string
	// resumedConfig is the source's fingerprint; see subagentRun.resumedConfig.
	resumedConfig string
	// seed is the source transcript the run's folder starts from; see SubagentRequest.Seed.
	seed string
}

// reserveWriter takes one of owner's background writer places and reports whether there
// was one: a limit of zero or less means no limit. The caller that gets true passes
// writerReserved to start, or gives the place back with releaseWriter when start fails.
// A closed manager reserves, so start reports the closure itself.
func (m *SubagentManager) reserveWriter(owner string, limit int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed && limit > 0 && m.writers[owner] >= limit {
		return false
	}
	m.writers[owner]++
	return true
}

func (m *SubagentManager) releaseWriter(owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writers[owner] > 1 {
		m.writers[owner]--
		return
	}
	delete(m.writers, owner)
}

// BackgroundWriters is how many runs of owner can change files and are going as handles,
// queued ones included. A checkpoint restore of the session is refused while it is not 0.
func (m *SubagentManager) BackgroundWriters(owner string) int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writers[owner]
}

// undoReservationLocked gives back the slot or queue place start took for a run that was
// not created.
func (m *SubagentManager) undoReservationLocked(holdsSlot bool, root, session string) {
	countDown(m.byRoot, root)
	countDown(m.bySession, session)
	if holdsSlot {
		m.running--
		close(m.freed)
		m.freed = make(chan struct{})
		return
	}
	m.queued--
}

// freeRunIDLocked draws a run id that no run of the manager holds. The ids are 32 bits, so a
// collision is rare but a repeat would overwrite a live run; the caller holds mu.
func (m *SubagentManager) freeRunIDLocked() (string, error) {
	for range runIDAttempts {
		id, err := m.newRunID()
		if err != nil {
			return "", err
		}
		if m.runs[id] == nil {
			return id, nil
		}
	}
	return "", fmt.Errorf("create sub-agent run id: every id drawn was already in use")
}

// runIDAttempts bounds the draws of freeRunIDLocked. With 2^32 ids and at most a few hundred
// runs held, one draw collides with probability far below 1e-6, so this bound is never hit
// in practice.
const runIDAttempts = 8

// admitNestedLocked is the gate a nested start passes, local or remote: the depth, then the run
// caps of its root and its session. The caller holds mu. A start at depth 0 is not nested and
// never reaches it.
func (m *SubagentManager) admitNestedLocked(depth, maxDepth, maxPerRoot, maxPerSession int, root, session string) error {
	if depth+1 > max(maxDepth, 1) {
		return errSubagentDepth
	}
	if (maxPerRoot > 0 && m.byRoot[root]-1 >= maxPerRoot) || (maxPerSession > 0 && m.bySession[session] >= maxPerSession) {
		return errSubagentRunLimit
	}
	return nil
}

// remoteReservation is one place a remote start takes in the root and session counts for the
// run's lifetime. It is released once: a second release does nothing.
type remoteReservation struct {
	manager       *SubagentManager
	root, session string
	releaseOnce   sync.Once
}

// reserveRemote takes a place in the counts of root and session for a remote run, after the
// same gate a local nested start passes (admitNestedLocked). The remote run has no local record,
// slot or queue place: only these counts, which this host's caps read.
func (m *SubagentManager) reserveRemote(root, session string, maxPerRoot, maxPerSession, depth, maxDepth int) (*remoteReservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errSubagentManagerClosed
	}
	if err := m.admitNestedLocked(depth, maxDepth, maxPerRoot, maxPerSession, root, session); err != nil {
		return nil, err
	}
	m.byRoot[root]++
	m.bySession[session]++
	return &remoteReservation{manager: m, root: root, session: session}, nil
}

// releaseRemote gives back a remote run's places. It is safe to call more than once.
func (m *SubagentManager) releaseRemote(reservation *remoteReservation) {
	reservation.releaseOnce.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		countDown(m.byRoot, reservation.root)
		countDown(m.bySession, reservation.session)
	})
}

// sessionRuns is how many runs of session are counted as going, local or remote.
func (m *SubagentManager) sessionRuns(session string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bySession[session]
}

// start registers a run and launches its goroutine. It fails when the manager is
// closed or when the run would have to wait and the queue is full.
func (m *SubagentManager) start(spec subagentStart) (*subagentRun, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errSubagentManagerClosed
	}
	id, err := m.freeRunIDLocked()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	root := spec.rootRun
	if root == "" {
		root = id
	}
	nested := spec.depth > 0
	if nested {
		if err := m.admitNestedLocked(spec.depth, spec.maxDepth, spec.maxPerRoot, spec.maxPerSession, root, spec.session); err != nil {
			m.mu.Unlock()
			return nil, err
		}
	}
	// The slot is taken here, not in the goroutine, so a free slot is never refused
	// because runs that were just started have not reached it yet.
	holdsSlot := m.running < spec.limit
	switch {
	case holdsSlot:
		m.running++
	case nested:
		m.mu.Unlock()
		return nil, errSubagentNoSlot
	case m.queued < spec.maxQueued:
		m.queued++
	default:
		m.mu.Unlock()
		return nil, errSubagentQueueFull
	}
	m.byRoot[root]++
	m.bySession[spec.session]++
	// A nested run ends with the run it belongs to: its context hangs off the parent's.
	parentContext := m.ctx
	if parent := m.runs[spec.parentID]; nested && parent != nil {
		parentContext = parent.ctx
	}
	ctx, cancel := context.WithCancel(parentContext)
	if spec.async != nil {
		m.mu.Unlock()
		if err := spec.async.begin(id, !holdsSlot); err != nil {
			cancel()
			m.mu.Lock()
			m.undoReservationLocked(holdsSlot, root, spec.session)
			m.mu.Unlock()
			return nil, err
		}
		m.mu.Lock()
		if m.closed {
			cancel()
			m.undoReservationLocked(holdsSlot, root, spec.session)
			m.mu.Unlock()
			spec.async.abort(id, errSubagentManagerClosed)
			return nil, errSubagentManagerClosed
		}
	}
	run := &subagentRun{
		async:         spec.async,
		store:         m.store,
		id:            id,
		owner:         spec.owner,
		handleOwner:   spec.handleOwner,
		session:       spec.session,
		root:          root,
		agent:         spec.request.Agent.Name,
		task:          subagentTaskPreview(spec.request.Task),
		timeoutMS:     spec.request.Agent.TimeoutMS,
		changes:       spec.changes,
		resultMax:     spec.resultMax,
		note:          spec.note,
		configNote:    spec.configNote,
		config:        spec.config,
		resumedFrom:   spec.resumedFrom,
		caller:        spec.caller,
		resumedConfig: spec.resumedConfig,
		startedAt:     m.now(),
		countedWriter: spec.writerReserved,
		cancel:        cancel,
		ctx:           ctx,
		done:          make(chan struct{}),
		state:         subagentRunQueued,
	}
	if holdsSlot {
		run.state = subagentRunRunning
	}
	// The run id is the owner of every process the child starts, so the child sees only
	// its own and the run's end can stop them.
	spec.request.Seed = spec.seed
	spec.request.Execution.ProcessOwner = id
	spec.request.Execution.AgentDepth = spec.depth + 1
	spec.request.Execution.RootRun = root
	spec.request.Execution.ProcessOwnerLimit = int64(spec.processMax)
	spec.request.Execution.ProcessAsync = spec.asyncHost != nil && spec.childAsync
	if spec.changes && spec.changedMax > 0 {
		run.tracked = true
		changedMax, changedBytes := spec.changedMax, spec.resultMax
		spec.request.Execution.OnFileChange = func(path, action string) { run.recordChange(path, action, changedMax, changedBytes) }
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
	defer m.recoverRun(run, spec)
	if !holdsSlot {
		if !m.acquire(ctx, spec.limit) {
			m.finish(run, subagentRunStopped, SubagentResult{}, "", spec)
			return
		}
		run.setState(subagentRunRunning)
		if run.async != nil {
			run.async.markRunning(run.id)
		}
	}
	defer m.release()

	childContext, cancel := context.WithTimeout(ctx, time.Duration(spec.request.Agent.TimeoutMS)*time.Millisecond)
	defer cancel()
	spec.request.OnTranscript = run.setDir
	result, err := m.runLauncher(childContext, spec)
	// The child is over, so nothing it started should go on: stop its processes before
	// the run is reported ended, and say how many there were.
	// The child's own async handles go first, so their records are removed with them; the
	// process stop below catches any process that had no handle.
	stoppedHandles := 0
	if spec.asyncHost != nil {
		stoppedHandles = spec.asyncHost.Manager().DeleteOwnersWithPrefix(run.id, "subagent_run_ended")
	}
	run.setProcessesStopped(max(stoppedHandles, stopRunProcesses(spec.processes, run.id)))
	switch {
	case err == nil:
		m.finish(run, subagentRunDone, result, "", spec)
	case ctx.Err() != nil:
		// Stopped by the model or by shutdown, not by the child's own limit.
		m.finish(run, subagentRunStopped, result, run.takeStopReason(), spec)
	case childContext.Err() != nil:
		m.finish(run, subagentRunTimedOut, result, "", spec)
	default:
		m.finish(run, subagentRunFailed, result, boundedReason(err.Error()), spec)
	}
}

// stopRunProcesses stops the processes a run's child started and returns how many it
// stopped. It does nothing without a process manager or for a manager that cannot tell
// owners apart.
func stopRunProcesses(processes ProcessManager, runID string) int {
	owners, ok := processes.(processOwnership)
	if !ok {
		return 0
	}
	return owners.stopOwned(runID, "subagent_run_ended")
}

// runLauncher turns a panic in the launcher into a failed run, because the goroutine
// has no caller that could recover it. The panic is logged; the run's reason does not
// carry its text.
func (m *SubagentManager) runLauncher(ctx context.Context, spec subagentStart) (result SubagentResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("sub-agent launcher panicked", "panic", recovered, "stack", string(debug.Stack()))
			err = errors.New("sub-agent launcher panicked")
		}
	}()
	return spec.launcher.Run(ctx, spec.request)
}

// recoverRun is the panic boundary of the rest of execute (the process sweep, finish and
// the bookkeeping in it): the goroutine has no caller that could recover it, and a panic
// would end the server and leave the run going. The run is finished as failed, unless
// finish had already begun, in which case only the waiters are released.
func (m *SubagentManager) recoverRun(run *subagentRun, spec subagentStart) {
	recovered := recover()
	if recovered == nil {
		return
	}
	slog.Error("sub-agent run failed inside the server", "run", run.id, "panic", recovered, "stack", string(debug.Stack()))
	run.mu.Lock()
	ended := !run.endedAt.IsZero()
	run.mu.Unlock()
	if !ended {
		m.finish(run, subagentRunFailed, SubagentResult{}, "internal error", spec)
		return
	}
	select {
	case <-run.done:
	default:
		close(run.done)
	}
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
	run.endedAt = m.now()
	run.mu.Unlock()
	// The manifest is written before the waiters wake, so a read of the file after a
	// wait returns sees the end.
	run.store.save(run)

	var dropped []droppedRun
	m.mu.Lock()
	countDown(m.byRoot, run.root)
	countDown(m.bySession, run.session)
	m.finished = append(m.finished, run.id)
	for len(m.finished) > spec.keep {
		dropped = append(dropped, m.dropOldestLocked())
	}
	m.mu.Unlock()
	m.removeDropped(dropped)
	if run.countedWriter {
		m.releaseWriter(run.owner)
	}
	// The async signal is queued before the waiters wake, so a caller released by done
	// finds it.
	if run.async != nil {
		run.async.end(run.view(), subagentRunRef(run))
	}
	// Waiters wake last, so a wait that returns sees the disk already settled.
	close(run.done)
}

func countDown(counts map[string]int, key string) {
	if counts[key] > 1 {
		counts[key]--
		return
	}
	delete(counts, key)
}

// byID returns the run with the given id whatever its owner. The async driver uses it:
// the async manager has already checked the owner.
func (m *SubagentManager) byID(id string) *subagentRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[id]
}

// RunTranscript is where a run's transcript folder is and the session key that started the
// run; dir is empty until the child has one. The transcript tool reads it.
func (m *SubagentManager) RunTranscript(id string) (dir, owner string, found bool) {
	run := m.byID(id)
	if run == nil {
		return "", "", false
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.dir, run.owner, true
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

// allRuns returns every retained run whatever its owner; a manager serves one storage directory.
func (m *SubagentManager) allRuns() []*subagentRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	runs := make([]*subagentRun, 0, len(m.runs))
	for _, run := range m.runs {
		runs = append(runs, run)
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
	return cancelActive(m.ownedRuns(owner), false, "")
}

// CancelUnmanagedOwner is CancelOwner for the runs that are not async handles. A handle is
// stopped and counted by the async manager, so a host that sums both counts each run once.
// The reason is kept the way a handle's stop keeps it, so the stopped result can say why.
func (m *SubagentManager) CancelUnmanagedOwner(owner, reason string) int {
	if m == nil {
		return 0
	}
	return cancelActive(m.ownedRuns(owner), true, reason)
}

// CancelUnmanagedOwnersWithPrefix is CancelUnmanagedOwner for every session key that starts
// with prefix.
func (m *SubagentManager) CancelUnmanagedOwnersWithPrefix(prefix, reason string) int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	var runs []*subagentRun
	for _, run := range m.runs {
		if run.async == nil && strings.HasPrefix(run.owner, prefix) {
			runs = append(runs, run)
		}
	}
	m.mu.Unlock()
	return cancelActive(runs, false, reason)
}

// cancelActive asks every run of runs that is still queued or running to stop and returns
// how many it asked. The runs are chosen before any is cancelled: a cancel can end a nested
// run of the same list during the call, and that run was still one the call asked to stop.
// unmanaged keeps only the runs that are not async handles. An empty reason keeps none.
func cancelActive(runs []*subagentRun, unmanaged bool, reason string) int {
	var active []*subagentRun
	for _, run := range runs {
		if (!unmanaged || run.async == nil) && run.active() {
			active = append(active, run)
		}
	}
	for _, run := range active {
		run.stopWith(reason)
	}
	return len(active)
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

// Close stops every run and waits up to closeGrace for the children to return. A child
// still going after that is logged and left behind. Starting a run after Close fails.
func (m *SubagentManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	returned := make(chan struct{})
	go func() {
		m.active.Wait()
		close(returned)
	}()
	timer := time.NewTimer(closeGrace)
	defer timer.Stop()
	select {
	case <-returned:
	case <-timer.C:
		m.mu.Lock()
		var going []string
		for id, run := range m.runs {
			if run.active() {
				going = append(going, id)
			}
		}
		m.mu.Unlock()
		sort.Strings(going)
		slog.Warn("sub-agent runs still going after Close; leaving them behind", "runs", going, "grace", closeGrace)
	}
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
