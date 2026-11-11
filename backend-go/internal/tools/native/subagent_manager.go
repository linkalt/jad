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
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/worktree"
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
	errSubagentRunLimit = ErrRunTreeLimit
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
	// remoteEnds counts the background goroutines of remote runs that have not returned yet. A
	// test waits on it so no remote goroutine outlives the test.
	remoteEnds sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	runs     map[string]*subagentRun
	finished []string // ids of ended runs, oldest first; keep_finished trims it per owner
	running  int      // slots held
	// tree is the run tree the nested-start limits and the background writer places count in
	// (RunTree): the manager's own until SetRunTree gives it the one every manager of the process
	// shares.
	tree   *RunTree
	queued int // runs waiting for a slot
	freed  chan struct{}
	// store keeps a manifest per run so ids survive a restart; nil keeps runs in
	// memory only.
	store *subagentStore
	// removeManifest deletes the manifest of a pruned run; it is a field so a test can
	// hold the pruner inside the removal. The store sets it.
	removeManifest func(id string)
	// onDropped, when set, is told the identity of each run keep_finished drops, after mu is released.
	// The app layer forgets the remote ids of dropped runs with it.
	onDropped func(identity any)
	// onClosed, when set, is called once by Close, after mu is released and the manager is marked
	// closed. The app layer forgets every remote id of the manager with it.
	onClosed func()
	// newRunID makes the id of a new run; it is a field so a test can force a collision.
	newRunID func() (string, error)
	// now reads the clock for the run times that the lists and the pruning order by; it is a
	// field so a test can give runs an exact order.
	now func() time.Time
	// afterLaunch, when set, runs in execute right after the launcher returned; it is a field
	// so a test can panic past the launcher's own recovery and reach recoverRun.
	afterLaunch func()
	// afterBegin, when set, runs in start right after a run's handle was registered and before
	// the run joins the manager; it is a field so a test can stop the handle in that window.
	afterBegin func(id string)
	// inFinish, when set, runs in finish after the run's end was saved and before its node leaves
	// the tree; it is a field so a test can panic inside finish and reach recoverRun's release.
	inFinish func()
	// transcriptRoot, when set, is the storage directory whose child folders are
	// deleted as runs are dropped; see DeleteTranscriptsUnder.
	transcriptRoot string
	// stopCascade, when set, is told the lineage key and stop reason of each run that ended stopped after
	// its launcher ran, or that a stop reached whatever its end (SetStopCascade, endLineage).
	stopCascade atomic.Pointer[func(key, reason string)]
	// runEnd, when set, is called outside every lock once a run that carries a lineage ended
	// (SetRunEnd).
	runEnd atomic.Pointer[func()]
}

// NewSubagentManager returns an empty manager that keeps its runs in memory only.
func NewSubagentManager() *SubagentManager {
	ctx, cancel := context.WithCancel(context.Background())
	manager := &SubagentManager{ctx: ctx, cancel: cancel, runs: make(map[string]*subagentRun), tree: &RunTree{}, freed: make(chan struct{})}
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
	// Worktree is tools.native.subagent.worktree of the session that built the manager, the
	// only one known at startup: it runs the git commands that finish the worktree of a run
	// the last server lost while it went.
	Worktree sessionfs.NativeSubagentWorktreeSettings
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
	manager.recover(options.Keep, worktreeOptions(options.Worktree))
	return manager
}

// recover loads the stored runs, marks the ones a restart interrupted as lost, and
// drops each owner's oldest ended runs beyond keep. keep is the setting of the session that
// built the manager, the only one known at startup; each session's own keep_finished applies
// again as its runs end. A lost run's worktree is finished with worktreeOptions: its work is
// committed to its branch and the folder removed, instead of being left where nobody looks.
func (m *SubagentManager) recover(keep int, worktreeOptions worktree.Options) {
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
			recoverWorktree(context.Background(), run, worktreeOptions)
		} else if run.worktree != nil && run.worktree.Path != "" {
			// A finished run whose collection failed: its folders were kept, and are tried again.
			recoverWorktree(context.Background(), run, worktreeOptions)
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
	owners := make(map[string]struct{})
	for _, id := range m.finished {
		if run := m.runs[id]; run != nil {
			owners[run.owner] = struct{}{}
		}
	}
	for owner := range owners {
		dropped = append(dropped, m.trimOwnerLocked(owner, keep)...)
	}
	m.removeDropped(dropped)
}

// droppedRun is a run that was forgotten under mu: its manifest and, when it is to be
// deleted, its transcript folder. Both are removed by removeDropped after mu is released.
type droppedRun struct {
	id  string
	run *subagentRun
	dir string
}

// trimOwnerLocked forgets owner's oldest ended runs beyond keep. keep_finished is a session
// setting, while sessions that share a storage directory share this manager: trimming the
// whole list by the keep of whichever run ended let one session's small keep_finished drop
// (and, with delete_transcripts, delete) another session's runs. An id whose run is gone is
// dropped as well. The caller holds mu or is the only user of the manager, and must pass the
// result to removeDropped after releasing mu, so a slow disk never holds the lock.
func (m *SubagentManager) trimOwnerLocked(owner string, keep int) []droppedRun {
	count := 0
	for _, id := range m.finished {
		if run := m.runs[id]; run != nil && run.owner == owner && !run.keepsWorktree() {
			count++
		}
	}
	var dropped []droppedRun
	for index := 0; index < len(m.finished); {
		run := m.runs[m.finished[index]]
		// A run whose worktree is kept (its collection failed) is the only record of those
		// folders: it stays until the work is collected, whatever the limit.
		if run != nil && (run.owner != owner || count <= keep || run.keepsWorktree()) {
			index++
			continue
		}
		if run != nil {
			count--
		}
		dropped = append(dropped, m.dropFinishedLocked(index))
	}
	return dropped
}

// keepsWorktree reports whether the run still has a worktree folder that was kept.
func (r *subagentRun) keepsWorktree() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.worktree != nil && r.worktree.Path != "" && r.state != subagentRunQueued && r.state != subagentRunRunning
}

// dropFinishedLocked forgets the ended run at index of m.finished; see trimOwnerLocked.
func (m *SubagentManager) dropFinishedLocked(index int) droppedRun {
	id := m.finished[index]
	m.finished = append(m.finished[:index], m.finished[index+1:]...)
	run := m.runs[id]
	delete(m.runs, id)
	dropped := droppedRun{id: id, run: run}
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

// Retains reports whether the manager still keeps the run with this identity. An id can be drawn
// again once its run is gone, so the identity, not the id, names the run. A closed manager keeps
// nothing: its runs are being forgotten by its close hook.
func (m *SubagentManager) Retains(identity any) bool {
	run, _ := identity.(*subagentRun)
	if run == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closed && m.runs[run.id] == run
}

// SetOnDropped makes the manager call fn with the identity of each run keep_finished drops, after
// the manager releases its lock. A later call replaces the earlier one.
func (m *SubagentManager) SetOnDropped(fn func(identity any)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onDropped = fn
}

// SetOnClosed makes Close call fn once, after the manager is marked closed and its lock is released.
func (m *SubagentManager) SetOnClosed(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onClosed = fn
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

// removeDropped deletes the manifests and transcript folders chosen by trimOwnerLocked. The
// caller must not hold mu: a manifest removal waits for the store, which a save holds across
// its fsync. A failure is logged and does not stop the others.
func (m *SubagentManager) removeDropped(dropped []droppedRun) {
	m.mu.Lock()
	onDropped := m.onDropped
	m.mu.Unlock()
	for _, run := range dropped {
		if onDropped != nil && run.run != nil {
			endStep(run.id, "dropped_callback", nil, func() { onDropped(run.run) })
		}
		endStep(run.id, "dropped_files", nil, func() {
			m.removeManifest(run.id)
			if run.dir == "" {
				return
			}
			if err := os.RemoveAll(run.dir); err != nil {
				m.logger().Warn("delete sub-agent transcript folder", "dir", run.dir, "error", err)
			}
		})
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
	// session is the session of the whole run tree and root the top-level run of this manager
	// (its own id for one); the manifest records them. node is the run's place in the run tree,
	// which the nested-start limits count; nil for a run loaded from disk, which is not going.
	session string
	root    string
	node    *RunNode
	// lineage is the run lineage of the call that started the run (RunLineageOf); the run's own
	// context carries it with lineageKey added. CancelUnmanagedLineage matches it.
	lineage []string
	// lineageKey is the run's own key in the lineage of what it starts (NewRunLineageKey), unique
	// in the process; it is what the run's stop cascades. Empty for a run loaded from disk.
	lineageKey string
	// callID is the parent's tool call that started the run (SubagentRequest.CallID); it
	// names the child's transcript folder and is how a reader finds the run of a call.
	callID    string
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

	mu    sync.Mutex
	state string
	// retrying is set while a failed collection of the run's worktree is tried again.
	retrying bool
	result   SubagentResult
	reason   string
	// stopReason is why the host stopped the run; the final stopped state keeps it as reason.
	stopReason string
	endedAt    time.Time
	cut        bool // the answer was longer than the limit and was cut
	// stopAsked is set by the first stop request (stopWith), whenever it came, and askedReason
	// keeps its reason. lineageEnded is set once the run starts nothing more (its launcher
	// returned), lineageCascaded once its lineageKey went to the stop cascade. Together they make a stop
	// that reached the run while it could still start work cascade exactly once, after that point,
	// whatever state the run ends in (endLineage, stopWith).
	stopAsked, lineageEnded, lineageCascaded bool
	askedReason                              string
	// external is set for a run the remote API serves; full then holds its whole answer, which
	// the remote API hands out in parts, as the cut result does not.
	external bool
	full     string
	dir      string // the child's transcript directory, once it exists
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
	// listed, writerReleased, asyncEnded and notified mark the steps of a run's end (endRun) that
	// have begun, so a second endRun (recoverRun after a panic inside finish) does exactly the ones
	// still to do, and a step that panicked is not tried again.
	listed, writerReleased, asyncEnded, notified atomic.Bool
	// note says why a writer that could have been handed off was not; it rides on the result.
	note string
	// configNote says the agent definition changed since a resumed run's source; see configChangedNote.
	configNote string
	// config is the agent definition's fingerprint, written into the manifest; see subagentFingerprint.
	config string
	// resumedFrom is the run this one continues from; empty for a run that does not.
	resumedFrom string
	// sandbox is the sandbox choice (the call's sandbox argument) the run was started in;
	// empty for a run in its agent's own environment.
	sandbox string
	// caller is the remote caller the run was started for ("remote:<token id>"); "" for a
	// local run. A remote resume may use only a run whose caller is its own.
	caller string
	// resumedConfig is the source's fingerprint; see subagentStore's ResumedConfig.
	resumedConfig string
	// tracked is set when the run lists its changed files (changes and a non-zero
	// changed_files_max); without it the result says nothing about files.
	tracked bool
	// isolated is set for a run of an agent with isolation worktree, and worktree is its
	// worktree once it has one; see subagentWorktree.
	isolated bool
	worktree *subagentWorktree
	// async reports the run to the async manager as a handle; nil for a run that is not one
	// (a writer, or a session without the async tool).
	async *subagentAsyncHandle

	store *subagentStore // nil when runs are not persisted
	// manager started the run and holds its stop cascade; nil for a run loaded from disk.
	manager *SubagentManager
	// restored marks a run loaded from disk. Its text is not in memory: it is read
	// from the transcript the first time the run is looked at; a reader that arrives
	// during that read waits for it.
	restored bool
	textOnce sync.Once
	doneOnce sync.Once
}

// subagentRunView is a copy of a run's state taken under its lock.
type subagentRunView struct {
	id, agent, task, state, reason string
	result                         SubagentResult
	resultMax                      int64
	cut                            bool
	full                           string
	changes                        bool
	files                          []subagentFileChange
	filesOmitted                   int
	tracked                        bool
	isolated                       bool
	worktree                       *subagentWorktree
	sandbox                        string
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
	return subagentRunView{id: r.id, agent: r.agent, task: r.task, state: r.state, reason: r.reason, result: r.result, resultMax: r.resultMax, cut: r.cut, full: r.full, changes: r.changes, files: append([]subagentFileChange(nil), r.files...), filesOmitted: r.filesOmitted, tracked: r.tracked, isolated: r.isolated, worktree: r.worktreeCopy(), sandbox: r.sandbox, note: r.note, configNote: r.configNote, resumedFrom: r.resumedFrom, processesStopped: r.processesStopped, dir: r.dir, timeoutMS: r.timeoutMS, elapsed: end.Sub(r.startedAt), startedAt: r.startedAt, endedAt: r.endedAt}
}

// stopWith cancels the run and keeps why, so the stopped result can say it. A run that
// is already being stopped (by the model, a reset or shutdown) keeps no reason of ours.
//
// A stop is chosen while the run is live and applied after, outside the chooser's lock, so it can
// land after the launcher returned, even after the run ended done or failed. The work the run
// started meanwhile carries its lineage key: once the run starts nothing more, a stop that reached
// it hands that key to the stop cascade, here when it came after that point (endLineage when before).
func (r *subagentRun) stopWith(reason string) {
	r.mu.Lock()
	if r.stopReason == "" && r.ctx.Err() == nil {
		r.stopReason = reason
	}
	if !r.stopAsked {
		r.stopAsked, r.askedReason = true, reason
	}
	late := r.lineageEnded && !r.lineageCascaded
	if late {
		r.lineageCascaded = true
	}
	r.mu.Unlock()
	r.cancel()
	if late {
		r.manager.cascadeStop(r.lineageKey, reason)
	}
}

// endLineage marks that the run starts nothing more, and reports whether its lineage key goes to the stop
// cascade now and with which reason: a run that ended stopped (reason), or one a stop reached
// before this point whatever its end (that stop's reason). A stop that comes later cascades from
// stopWith. A run that ended on its own or timed out, unasked, cascades nothing.
func (r *subagentRun) endLineage(stopped bool, reason string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lineageEnded = true
	if r.lineageCascaded || (!stopped && !r.stopAsked) {
		return "", false
	}
	r.lineageCascaded = true
	if !stopped {
		reason = r.askedReason
	}
	return reason, true
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
		Rounds: r.result.Rounds, Tokens: r.result.Tokens, Dir: r.dir, Cut: r.cut, Changes: r.changes, Files: append([]subagentFileChange(nil), r.files...), FilesOmitted: r.filesOmitted, Tracked: r.tracked, Worktree: r.worktreeCopy(), Sandbox: r.sandbox, Config: r.config, ResumedFrom: r.resumedFrom, ResumedConfig: r.resumedConfig, Caller: r.caller,
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
		isolated: manifest.Worktree != nil, worktree: manifest.Worktree,
		dir: manifest.Dir, result: SubagentResult{Rounds: manifest.Rounds, Tokens: manifest.Tokens},
		store: store, restored: true, config: manifest.Config, resumedFrom: manifest.ResumedFrom, sandbox: manifest.Sandbox, resumedConfig: manifest.ResumedConfig, caller: manifest.Caller,
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
	// admission is where the start sits in its tree and the limits it is admitted under (admitLocked).
	admission runAdmission
	// parentID is the run the starting call runs in; empty at depth 0.
	parentID string
	// lineage is the run lineage of the starting call (RunLineageOf); see subagentRun.lineage.
	lineage []string
	// changes is set when the agent can change files; see subagentRun.changes.
	changes bool
	// worktree, when set, runs the child in a git worktree of its own.
	worktree *subagentWorktreeStart
	// runMCP, when set, starts the child's stdio MCP servers in the worktree it works in
	// (startRunMCP); nil for an agent without MCP tools, or a session without the loader.
	runMCP *subagentRunMCP
	// changedMax is how many changed files the run lists (see recordChange); keep and resultMax
	// are the starting session's settings.
	changedMax int
	keep       int
	resultMax  int64
	// external is set for a run the remote API serves; see subagentRun.external.
	external bool
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
	// sandbox is the run's sandbox choice; see subagentRun.sandbox.
	sandbox string
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
// A closed manager reserves, so start reports the closure itself. The places are the owner's in
// the run tree, so every manager sharing it counts the same writers.
func (m *SubagentManager) reserveWriter(owner string, limit int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tree.ReserveWriter(owner, limit, m.closed)
}

func (m *SubagentManager) releaseWriter(owner string) {
	m.runTree().ReleaseWriter(owner)
}

// BackgroundWriters is how many runs of owner can change files and are going as handles,
// queued ones included, in every manager sharing the run tree. A checkpoint restore of the session is refused while it is not 0.
func (m *SubagentManager) BackgroundWriters(owner string) int {
	if m == nil {
		return 0
	}
	return m.runTree().Writers(owner)
}

// undoReservationLocked gives back the node, and the slot or queue place, start took for a run
// that was not created.
func (m *SubagentManager) undoReservationLocked(holdsSlot bool, node *RunNode) {
	node.Release()
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

// remoteReservation is one place a remote start takes in the root and session counts for the
// run's lifetime. It is released once: a second release does nothing.
type remoteReservation struct {
	node *RunNode
}

// reserveRemote takes a node in the run tree for a nested remote run, after the same admission a
// local nested start passes (admitLocked). The remote run has no local record, slot or queue
// place: only this node, counted in the calling session and under the caller's root, which this
// host's caps read.
func (m *SubagentManager) reserveRemote(admission runAdmission) (*remoteReservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errSubagentManagerClosed
	}
	_, node, err := m.admitLocked(admission, false)
	if err != nil {
		return nil, err
	}
	return &remoteReservation{node: node}, nil
}

// releaseRemote gives back a remote run's node. It is safe to call more than once.
func (m *SubagentManager) releaseRemote(reservation *remoteReservation) {
	reservation.node.Release()
}

// sessionRuns is how many runs session executes that the run tree counts as going, local or
// remote, of either kind and in every manager sharing the tree.
func (m *SubagentManager) sessionRuns(session string) int {
	return m.runTree().SessionRuns(session)
}

// runTree is the tree the manager admits into.
func (m *SubagentManager) runTree() *RunTree {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tree
}

// SetRunTree makes the manager admit into tree, the one the resolver shares between every manager
// and the profile_message runs. It must be called before the manager starts a run; nil keeps the
// manager's own.
func (m *SubagentManager) SetRunTree(tree *RunTree) {
	if m == nil || tree == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tree = tree
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
	admission := spec.admission
	root := admission.rootRun
	if root == "" {
		root = id
	}
	nested := admission.nested()
	holdsSlot, node, err := m.admitLocked(admission, true)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	// A nested run ends with the run it belongs to: its context hangs off the parent's.
	parentContext := m.ctx
	if parent := m.runs[spec.parentID]; nested && parent != nil {
		parentContext = parent.ctx
	}
	ctx, cancel := context.WithCancel(parentContext)
	// The child's calls run under the run's lineage: the caller's and the run's own key, which
	// is unique in the process where the id is unique only in this manager.
	lineageKey := NewRunLineageKey("subagent:" + id)
	ctx = WithRunLineage(ctx, ExtendRunLineage(spec.lineage, lineageKey))
	if spec.async != nil {
		m.mu.Unlock()
		err := spec.async.begin(id, lineageKey, !holdsSlot)
		if err == nil && m.afterBegin != nil {
			m.afterBegin(id)
		}
		if err != nil {
			cancel()
			m.mu.Lock()
			m.undoReservationLocked(holdsSlot, node)
			m.mu.Unlock()
			return nil, err
		}
		m.mu.Lock()
		if m.closed {
			cancel()
			m.undoReservationLocked(holdsSlot, node)
			m.mu.Unlock()
			spec.async.abort(id, errSubagentManagerClosed)
			return nil, errSubagentManagerClosed
		}
	}
	run := &subagentRun{
		async:         spec.async,
		store:         m.store,
		manager:       m,
		id:            id,
		owner:         spec.owner,
		handleOwner:   spec.handleOwner,
		session:       admission.session,
		root:          root,
		node:          node,
		lineage:       spec.lineage,
		lineageKey:    lineageKey,
		callID:        spec.request.CallID,
		agent:         spec.request.Agent.Name,
		task:          subagentTaskPreview(spec.request.Task),
		timeoutMS:     spec.request.Agent.TimeoutMS,
		changes:       spec.changes,
		isolated:      spec.worktree != nil,
		resultMax:     spec.resultMax,
		external:      spec.external,
		note:          spec.note,
		configNote:    spec.configNote,
		config:        spec.config,
		resumedFrom:   spec.resumedFrom,
		sandbox:       spec.sandbox,
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
	// The run id is the owner of every handle the child starts, so the child sees only
	// its own and the run's end can stop them.
	spec.request.Seed = spec.seed
	spec.request.Execution.ProcessOwner = id
	spec.request.Execution.AgentDepth = admission.depth + 1
	spec.request.Execution.ChainDepth = admission.runDepth() + 1
	spec.request.Execution.RootRun = root
	spec.request.Execution.RunNode = node.ID()
	spec.request.Execution.ProcessAsync = spec.asyncHost != nil && spec.childAsync
	if spec.changes && spec.changedMax > 0 {
		run.tracked = true
		changedMax, changedBytes := spec.changedMax, spec.resultMax
		// The caller's own listener (a run started by a child is also a change of that child) hears it too.
		outer := spec.request.Execution.OnFileChange
		spec.request.Execution.OnFileChange = func(path, action string) {
			run.recordChange(path, action, changedMax, changedBytes)
			if outer != nil {
				outer(path, action)
			}
		}
	}
	m.runs[id] = run
	m.active.Add(1)
	m.mu.Unlock()
	// A stop of the handle that came while the run was not in the manager yet applies now.
	if spec.async != nil {
		if reason, stopped := spec.async.started(id); stopped {
			run.stopWith(reason)
		}
	}
	run.store.save(run)
	go m.execute(ctx, run, spec, holdsSlot)
	return run, nil
}

func (m *SubagentManager) execute(ctx context.Context, run *subagentRun, spec subagentStart, holdsSlot bool) {
	defer m.active.Done()
	defer run.cancel()
	defer m.recoverRun(run, spec)
	if !holdsSlot {
		if !m.acquire(ctx, spec.admission.limit) {
			m.finish(run, subagentRunStopped, SubagentResult{}, "", spec)
			return
		}
		run.setState(subagentRunRunning)
		run.node.SetState(RunNodeRunning)
		if run.async != nil {
			run.async.markRunning(run.id)
		}
	}
	defer m.release()

	childContext, cancel := context.WithTimeout(ctx, time.Duration(spec.request.Agent.TimeoutMS)*time.Millisecond)
	defer cancel()
	var tree *worktree.Tree
	if spec.worktree != nil {
		var err error
		if tree, err = m.openWorktree(childContext, run, &spec); err != nil {
			m.finish(run, subagentRunFailed, SubagentResult{}, boundedReason("the worktree could not be created: "+err.Error()), spec)
			return
		}
	}
	if spec.runMCP != nil {
		// A run in a worktree (its own, or the one of the run it is nested in) calls its MCP
		// tools on servers started there. A server that does not start ends the run: it never
		// falls back to the session's, which work in the session's folder.
		if err := startRunMCP(childContext, spec.request.Execution, spec.runMCP.key, spec.runMCP.load); err != nil {
			state, reason := subagentRunFailed, boundedReason("the run's MCP servers could not be started: "+err.Error())
			switch {
			case ctx.Err() != nil:
				state, reason = subagentRunStopped, run.takeStopReason()
			case childContext.Err() != nil:
				state, reason = subagentRunTimedOut, ""
			}
			if tree != nil {
				m.closeWorktree(run, tree, spec.request.Execution)
			}
			m.finish(run, state, SubagentResult{}, reason, spec)
			return
		}
	}
	spec.request.OnTranscript = run.setDir
	result, err := m.runLauncher(childContext, spec)
	if m.afterLaunch != nil {
		m.afterLaunch()
	}
	// The child is over, so nothing it started should go on: stop its async handles (exec
	// runs, nested children) before the run is reported ended, and say how many there were.
	stoppedHandles := 0
	if spec.asyncHost != nil {
		stoppedHandles = spec.asyncHost.Manager().DeleteOwnersWithPrefix(run.id, "subagent_run_ended")
	}
	run.setProcessesStopped(stoppedHandles)
	state, reason := subagentRunDone, ""
	switch {
	case err == nil:
	case ctx.Err() != nil:
		// Stopped by the model or by shutdown, not by the child's own limit.
		state, reason = subagentRunStopped, run.takeStopReason()
	case childContext.Err() != nil:
		state = subagentRunTimedOut
	default:
		state, reason = subagentRunFailed, boundedReason(err.Error())
	}
	// A stopped run's own handles went with the sweep above; what it started in another profile
	// (the work of a run its profile_message started) is reached through the lineage. So is the
	// work of a run a stop reached after its launcher had returned, which can end done or failed.
	// A run that ended on its own or timed out, unasked, leaves that work alone, as before.
	if cascadeReason, cascade := run.endLineage(state == subagentRunStopped, reason); cascade {
		m.cascadeStop(run.lineageKey, cascadeReason)
	}
	// The worktree is committed once nothing of the child runs any more, whatever the
	// end, so what it did is on its branch before the run is reported ended.
	if tree != nil {
		m.closeWorktree(run, tree, spec.request.Execution)
	}
	m.finish(run, state, result, reason, spec)
}

func (r *subagentRun) worktreeCopy() *subagentWorktree {
	if r.worktree == nil {
		return nil
	}
	copied := *r.worktree
	return &copied
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

// recoverRun is the panic boundary of the rest of execute (the handle sweep, finish and
// the bookkeeping in it): the goroutine has no caller that could recover it, and a panic
// would end the server and leave the run going. The run is finished as failed, unless
// finish had already recorded its end, in which case the steps of endRun it had not begun are
// done (its node, its finished-list place, its writer place, its async handle, done).
func (m *SubagentManager) recoverRun(run *subagentRun, spec subagentStart) {
	recovered := recover()
	if recovered == nil {
		return
	}
	slog.Error("sub-agent run failed inside the server", "run", run.id, "panic", recovered, "stack", string(debug.Stack()))
	// Whatever finish got to, the run is over: its node leaves the tree (Release counts once).
	defer run.node.Release()
	run.mu.Lock()
	ended := !run.endedAt.IsZero()
	answer := run.result.Text
	if run.external {
		answer = run.full
	}
	run.mu.Unlock()
	if !ended {
		m.finish(run, subagentRunFailed, SubagentResult{}, "internal error", spec)
		return
	}
	// finish recorded the end and then panicked: the steps of endRun it had not begun are done
	// now (the async handle ends with the recorded state, the answer as the run keeps it).
	m.endRun(run, spec, answer)
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
	// The whole answer is kept for the run's async handle, which saves it before this cut.
	answer := result.Text
	var cut bool
	result.Text, cut = cutUTF8(result.Text, spec.resultMax)
	run.mu.Lock()
	run.cut = cut
	if run.external {
		run.full = answer
	}
	run.state = state
	run.result = result
	run.reason = reason
	run.endedAt = m.now()
	run.mu.Unlock()
	// The manifest is written before the waiters wake, so a read of the file after a
	// wait returns sees the end.
	run.store.save(run)
	if m.inFinish != nil {
		m.inFinish()
	}
	m.endRun(run, spec, answer)
}

// closeDone wakes the run's waiters once.
func (r *subagentRun) closeDone() { r.doneOnce.Do(func() { close(r.done) }) }

// endRun gives back what a run whose end finish recorded holds, and wakes its waiters: its node,
// its place in the finished list (with the trim to keep and the removal of what the trim dropped),
// its writer place, its async handle, done and the run-end notification. Each step runs at most once
// (the run's marks are taken before the step), so a second call, as recoverRun makes after a panic
// in finish before endRun, does only what is left. A panic inside a step is logged and ends that
// step only: the later steps still run, and the step that panicked is not tried again, so whatever
// it had not given back yet stays held (see DECISIONS.md, T-007 end steps).
func (m *SubagentManager) endRun(run *subagentRun, spec subagentStart, answer string) {
	var dropped []droppedRun
	endStep(run.id, "finished_list", nil, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		run.node.Release()
		if run.listed.CompareAndSwap(false, true) {
			m.finished = append(m.finished, run.id)
			dropped = m.trimOwnerLocked(run.owner, spec.keep)
		}
	})
	// Each dropped run is removed in its own steps (removeDropped), so a panic for one leaves the
	// others, and the files of the one, to go.
	m.removeDropped(dropped)
	if run.countedWriter {
		endStep(run.id, "writer_place", &run.writerReleased, func() { m.releaseWriter(run.owner) })
	}
	// The async signal is queued before the waiters wake, so a caller released by done
	// finds it.
	if run.async != nil {
		endStep(run.id, "async_handle", &run.asyncEnded, func() {
			run.async.end(run.view(), subagentTranscriptRef(run), answer)
		})
	}
	// Waiters wake last, so a wait that returns sees the disk already settled.
	endStep(run.id, "done", nil, run.closeDone)
	if notify := m.runEnd.Load(); notify != nil && len(run.lineage) > 0 {
		endStep(run.id, "run_end_notify", &run.notified, *notify)
	}
}

// endStep runs one step of a run's end. A non-nil mark that was already taken skips it; it is
// taken before the step, so a step that panicked is never tried again. A panic is logged with the
// run and the step and goes no further.
func endStep(runID, step string, mark *atomic.Bool, do func()) {
	if mark != nil && !mark.CompareAndSwap(false, true) {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("sub-agent run end step failed inside the server", "run", runID, "step", step, "panic", recovered, "stack", string(debug.Stack()))
		}
	}()
	do()
}

// SetRunEnd sets what the manager calls, outside every lock, each time a run that carries a
// lineage ended: the app layer then looks again whether work is still going under a run's
// lineage key (HasLiveLineage). Nil sets none.
func (m *SubagentManager) SetRunEnd(notify func()) {
	if m == nil {
		return
	}
	if notify == nil {
		m.runEnd.Store(nil)
		return
	}
	m.runEnd.Store(&notify)
}

// HasLiveLineage reports whether a run whose lineage has a key that match accepts is queued or
// running, a run with a handle or not.
func (m *SubagentManager) HasLiveLineage(match func(key string) bool) bool {
	if m == nil {
		return false
	}
	return len(m.liveRuns(func(run *subagentRun) bool { return slices.ContainsFunc(run.lineage, match) })) > 0
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

// CallRunning reports whether a run that the call callID of the session started is queued
// or running now. It reads the manager's own table, so it is exact for the moment it is
// asked and says nothing of a run the manager does not hold: one that ended and was pruned,
// one from before a restart (a run that was going then is loaded as lost, never as running),
// or a remote one (it has no run here).
func (m *SubagentManager) CallRunning(session, callID string) bool {
	if m == nil || callID == "" {
		return false
	}
	for _, run := range m.allRuns() {
		if run.session == session && run.callID == callID && run.active() {
			return true
		}
	}
	return false
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
	return stopRuns(m.liveRuns(func(run *subagentRun) bool { return run.owner == owner }), "")
}

// CancelUnmanagedOwner is CancelOwner for the runs that are not async handles. A handle is
// stopped and counted by the async manager, so a host that sums both counts each run once.
// The reason is kept the way a handle's stop keeps it, so the stopped result can say why.
func (m *SubagentManager) CancelUnmanagedOwner(owner, reason string) int {
	if m == nil {
		return 0
	}
	return stopRuns(m.liveRuns(func(run *subagentRun) bool { return run.owner == owner && run.async == nil }), reason)
}

// SetStopCascade sets what the manager tells, outside its lock, of each run that ended stopped
// after its launcher ran, or that a stop reached at any time, even after its launcher returned
// or the run ended: the run's lineage key (a key of its descendants' lineage, not its id) and
// the stop's reason, once per run and only once the run starts nothing more. The app layer stops the work under that key
// in every manager with it.
func (m *SubagentManager) SetStopCascade(cascade func(key, reason string)) {
	if m == nil {
		return
	}
	if cascade == nil {
		m.stopCascade.Store(nil)
		return
	}
	m.stopCascade.Store(&cascade)
}

// cascadeStop hands key and reason to the stop cascade, outside every lock, and reports whether
// there was one to hand them to. An empty key (a run loaded from disk) has nothing under it.
func (m *SubagentManager) cascadeStop(key, reason string) bool {
	if m == nil || key == "" {
		return false
	}
	cascade := m.stopCascade.Load()
	if cascade == nil {
		return false
	}
	(*cascade)(key, reason)
	return true
}

// CancelUnmanagedLineage is CancelUnmanagedOwner for the runs whose lineage has a key that match
// accepts: the runs started under a run or owner of another profile, whoever owns them.
func (m *SubagentManager) CancelUnmanagedLineage(match func(key string) bool, reason string) int {
	if m == nil {
		return 0
	}
	return stopRuns(m.liveRuns(func(run *subagentRun) bool {
		return run.async == nil && slices.ContainsFunc(run.lineage, match)
	}), reason)
}

// CancelUnmanagedOwnersWithPrefix is CancelUnmanagedOwner for every session key that starts
// with prefix.
func (m *SubagentManager) CancelUnmanagedOwnersWithPrefix(prefix, reason string) int {
	if m == nil {
		return 0
	}
	return stopRuns(m.liveRuns(func(run *subagentRun) bool {
		return run.async == nil && strings.HasPrefix(run.owner, prefix)
	}), reason)
}

// liveRuns returns the runs keep accepts that are queued or running, chosen under the manager's
// lock (each run's lock nests inside it) before any is stopped: a stop can end a nested run of
// the same list during the stops, and that run was still one the call asked to stop. A run chosen
// here that ends before its stop lands is still asked, so its stop cascade is not lost (stopWith).
func (m *SubagentManager) liveRuns(keep func(*subagentRun) bool) []*subagentRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	var runs []*subagentRun
	for _, run := range m.runs {
		if keep(run) && run.active() {
			runs = append(runs, run)
		}
	}
	return runs
}

// stopRuns asks every run of runs to stop, outside every lock, and returns how many it asked. An
// empty reason keeps none.
func stopRuns(runs []*subagentRun, reason string) int {
	for _, run := range runs {
		run.stopWith(reason)
	}
	return len(runs)
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
	// A start after this point is refused, so the hook sees every run the manager will ever hold.
	onClosed := m.onClosed
	m.mu.Unlock()
	if onClosed != nil {
		onClosed()
	}
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
