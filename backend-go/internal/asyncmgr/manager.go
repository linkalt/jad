package asyncmgr

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures Open.
type Options struct {
	// Dir is the .async folder: <id>.json records and index.json.
	Dir    string
	Config Config
	// Server identifies this process in the records it writes.
	Server ServerID
	// ServerAlive reports whether another server that wrote a record is still running,
	// which makes its handles not ours to mark lost. It must return false for this
	// process. Nil means no other server is ever alive.
	ServerAlive func(ServerID) bool
	// StopWait is how long Stop, Close and callers of Await-after-stop wait for a
	// driver to report the end of a handle it was told to stop. Zero does not wait.
	StopWait time.Duration
	Now      func() time.Time
	Logger   *slog.Logger
}

// row is the in-memory index entry of one handle. rec holds the full record only while
// the handle is live or its signal is unread; a read terminal handle is a compact row and
// its detail is read from its file on demand.
type row struct {
	id, owner        string
	kind             Kind
	state            State
	ok               bool
	label            string
	started, changed time.Time
	unread, overflow bool
	finishing        bool
	after            endAction
	rec              *Record
	// progress is the driver side cursor of what a "latest" view already showed; it lives
	// only while the handle is live.
	progress any
}

// endAction says what happens to a handle's signal when its end arrives after a user
// reset or delete already stopped it.
type endAction int

const (
	endKeep   endAction = iota // queue the signal as usual
	endRead                    // the signal is born read (reset: the history is gone)
	endRemove                  // the row and its record are removed (the session is gone)
)

type ownerStats struct {
	live    map[string]*row
	total   int
	unread  map[string]*row
	dropped int
}

// Manager owns every async handle of one storage root. It starts no goroutine: all work
// happens in the callers' goroutines, so it needs no lifecycle beyond Close.
type Manager struct {
	opts Options
	cfg  Config
	dir  string

	// ioMu serializes file writes. Lock order: indexMu, ioMu, then mu. Nothing takes ioMu
	// while holding mu, and nothing takes indexMu (other than with TryLock) while holding
	// ioMu. A change whose files must match it (commit, Complete, Register) holds ioMu from
	// the in-memory change until its files are written.
	ioMu sync.Mutex
	// ioClosed (guarded by ioMu) is set by Close once the final index is written; later
	// record, removal and index writes are skipped so nothing touches the folder after Close.
	ioClosed bool
	// beforeRunningWrite is a test-only hook. It runs in MarkRunning after the state check and
	// before the running record is written, with no lock held. Nil in production.
	beforeRunningWrite func(id string)
	// beforeWaitTimeout is a test-only hook. It runs in Wait on the timeout branch, after the
	// timer fires and before the context and the ready signals are checked, with no lock held.
	// Nil in production.
	beforeWaitTimeout func()
	// beforeCompleteCommit is a test-only hook. It runs in Complete after the result record
	// is written and before the end is committed under mu, with ioMu held (so it must not
	// write files itself). Nil in production.
	beforeCompleteCommit func(id string)
	// beforeCompleteWrite is a test-only hook. It runs in Complete after the handle is
	// claimed and before ioMu is taken, with no lock held. Nil in production.
	beforeCompleteWrite func(id string)
	// beforeCommitWrite is a test-only hook. It runs in commit and in a Wait delivery after
	// the in-memory change and before its files are written, with ioMu held (so it must not
	// write files). Nil in production.
	beforeCommitWrite func()
	// indexMu and indexDirty coalesce index.json rewrites (see writeIndex).
	indexMu    sync.Mutex
	indexDirty atomic.Bool
	// flushMu guards flushTimer, the pending delayed index write (nil when none).
	// indexClosed turns the delay off once Close has written the final index.
	flushMu     sync.Mutex
	flushTimer  *time.Timer
	indexClosed atomic.Bool

	mu        sync.Mutex
	rows      map[string]*row
	owners    map[string]*ownerStats
	liveTotal int
	// completing counts Complete calls past the claim that still write files; Close waits for them.
	completing int
	// finished counts terminal, read rows; nextExpiry is a lower bound of when an
	// unread signal or a finished row may expire (zero = none), so prune can skip its scan.
	finished   int
	nextExpiry time.Time
	drivers    map[Kind]Driver
	changed    chan struct{}
	closed     bool
	watchers   map[int]watcher
	watchSeq   int
	// foreign holds the live handles that another live server owned at Open, with that
	// server; foreignN is its size, read without mu, so the calls that check those servers
	// (endDeadForeign) cost nothing when there are none.
	foreign  map[string]ServerID
	foreignN atomic.Int32
}

// Open creates or recovers the manager in opts.Dir. Handles recorded as running by an
// earlier server become lost and queue a signal; only the index and those records are
// read, not the terminal history.
func Open(opts Options) (*Manager, error) {
	if err := opts.Config.Validate(); err != nil {
		return nil, fmt.Errorf("async config: %w", err)
	}
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, fmt.Errorf("async manager needs a directory")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	m := &Manager{
		opts: opts, cfg: opts.Config, dir: opts.Dir,
		rows: map[string]*row{}, owners: map[string]*ownerStats{},
		drivers: map[Kind]Driver{}, changed: make(chan struct{}), foreign: map[string]ServerID{},
	}
	if err := m.recover(); err != nil {
		return nil, err
	}
	return m, nil
}

// Config returns the validated configuration.
func (m *Manager) Config() Config { return m.cfg }

// AddDriver registers the driver of a kind. Register and Stop need it.
func (m *Manager) AddDriver(d Driver) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.drivers[d.Kind()]; exists {
		return fmt.Errorf("driver for kind %q already registered", d.Kind())
	}
	m.drivers[d.Kind()] = d
	return nil
}

func (m *Manager) stats(owner string) *ownerStats {
	s := m.owners[owner]
	if s == nil {
		s = &ownerStats{live: map[string]*row{}, unread: map[string]*row{}}
		m.owners[owner] = s
	}
	return s
}

type watcher struct {
	owner  string
	notify func()
}

// Watch calls notify whenever owner's live or unread counts may have changed, until the
// returned cancel runs. notify runs under the manager's lock: it must not block or call
// back into the Manager (signal a channel and return).
func (m *Manager) Watch(owner string, notify func()) (cancel func()) {
	m.mu.Lock()
	if m.watchers == nil {
		m.watchers = map[int]watcher{}
	}
	m.watchSeq++
	id := m.watchSeq
	m.watchers[id] = watcher{owner: owner, notify: notify}
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.watchers, id)
		m.mu.Unlock()
	}
}

func (m *Manager) changedLocked(owner string) {
	for _, w := range m.watchers {
		if w.owner == owner {
			w.notify()
		}
	}
}

func (m *Manager) track(r *row) {
	s := m.stats(r.owner)
	s.total++
	if !r.state.Terminal() {
		s.live[r.id] = r
		m.liveTotal++
	}
	if r.unread {
		s.unread[r.id] = r
	}
	if r.state.Terminal() && !r.unread {
		m.finished++
	}
	m.noteExpiryLocked(r)
}

func (m *Manager) untrack(r *row) {
	s := m.owners[r.owner]
	if s == nil {
		return
	}
	s.total--
	if _, ok := s.live[r.id]; ok {
		delete(s.live, r.id)
		m.liveTotal--
	}
	delete(s.unread, r.id)
	if r.state.Terminal() && !r.unread {
		m.finished--
	}
}

// mutateLocked changes a row while keeping the per-owner counters exact.
func (m *Manager) mutateLocked(r *row, change func(*row)) {
	m.untrack(r)
	change(r)
	m.track(r)
	m.changedLocked(r.owner)
}

func (m *Manager) addRowLocked(r *row) {
	m.rows[r.id] = r
	m.track(r)
	m.changedLocked(r.owner)
}

func (m *Manager) forgetLocked(r *row) {
	m.untrack(r)
	m.changedLocked(r.owner)
	delete(m.rows, r.id)
	if s := m.owners[r.owner]; s != nil && s.total == 0 && s.dropped == 0 {
		delete(m.owners, r.owner)
	}
}

func (m *Manager) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// Register starts tracking a handle. It fails with a *CapacityError (errors.Is
// ErrCapacity) when the owner or the server is at its live-handle cap; terminal and unread
// handles never count.
func (m *Manager) Register(reg Registration) error {
	m.endDeadForeign()
	if !idPattern.MatchString(reg.ID) || reg.ID == indexName {
		return fmt.Errorf("%w: %q", ErrInvalidID, reg.ID)
	}
	if reg.Owner == "" || reg.Kind == "" {
		return fmt.Errorf("async handle needs an owner and a kind")
	}
	now := m.opts.Now()
	state := StateRunning
	if reg.Queued {
		state = StateQueued
	}
	label, _ := cutUTF8(reg.Label, labelMaxBytes)
	rec := Record{Version: recordVersion, ID: reg.ID, Kind: reg.Kind, Owner: reg.Owner, State: state, Label: label, StartedAt: now}
	if m.opts.Server.PID > 0 {
		server := m.opts.Server
		rec.Server = &server
	}

	// ioMu is held from the row's creation until its record is written, so the final index
	// of Close never lists a handle without a record, and the record of a handle Close ended
	// cannot be overwritten by its running record.
	m.ioMu.Lock()
	m.mu.Lock()
	refuse := func(err error) error {
		m.mu.Unlock()
		m.ioMu.Unlock()
		return err
	}
	switch {
	case m.closed:
		return refuse(ErrClosed)
	case m.drivers[reg.Kind] == nil:
		return refuse(fmt.Errorf("%w %q", ErrNoDriver, reg.Kind))
	case m.rows[reg.ID] != nil:
		return refuse(fmt.Errorf("%w: %q", ErrDuplicate, reg.ID))
	}
	if s := m.owners[reg.Owner]; s != nil && int64(len(s.live)) >= m.cfg.MaxHandles {
		return refuse(&CapacityError{Scope: "owner", Limit: m.cfg.MaxHandles, Live: int64(len(s.live))})
	}
	if int64(m.liveTotal) >= m.cfg.MaxHandlesTotal {
		return refuse(&CapacityError{Scope: "total", Limit: m.cfg.MaxHandlesTotal, Live: int64(m.liveTotal)})
	}
	r := &row{id: reg.ID, owner: reg.Owner, kind: reg.Kind, state: state, label: label, started: now, changed: now, rec: &rec}
	m.addRowLocked(r)
	m.mu.Unlock()

	err := m.writeRecordIOLocked(rec)
	if err != nil {
		m.mu.Lock()
		m.forgetLocked(r)
		m.mu.Unlock()
	}
	m.ioMu.Unlock()
	if err != nil {
		return fmt.Errorf("save async record: %w", err)
	}
	m.writeIndex()
	return nil
}

// LiveInfo reports the kind, state and start time of a live handle of owner; ok is false for
// an unknown, foreign or ended handle.
func (m *Manager) LiveInfo(owner, id string) (kind Kind, state State, started time.Time, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[id]
	if r == nil || r.owner != owner || r.state.Terminal() {
		return "", "", time.Time{}, false
	}
	return r.kind, r.state, r.started, true
}

// Driver is the driver registered for kind, or nil.
func (m *Manager) Driver(kind Kind) Driver {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.drivers[kind]
}

// Progress is what SetProgress stored for a live handle, nil when nothing or the handle ended.
func (m *Manager) Progress(id string) any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rows[id]; r != nil {
		return r.progress
	}
	return nil
}

// SetProgress keeps a driver cursor on a live handle. It is in memory only and is dropped
// when the handle ends; an ended or unknown handle ignores it.
func (m *Manager) SetProgress(id string, progress any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rows[id]; r != nil && !r.state.Terminal() {
		r.progress = progress
	}
}

// MarkRunning moves a queued handle to running. Any other state is left alone.
func (m *Manager) MarkRunning(id string) {
	m.mu.Lock()
	r := m.rows[id]
	if r == nil || r.state != StateQueued || r.finishing {
		m.mu.Unlock()
		return
	}
	m.mutateLocked(r, func(r *row) { r.state = StateRunning })
	if r.rec != nil {
		r.rec.State = StateRunning
	}
	rec := *r.rec
	m.mu.Unlock()
	if m.beforeRunningWrite != nil {
		m.beforeRunningWrite(id)
	}
	// Complete may have run in the gap above and written a terminal record; the write is
	// skipped then. skip runs under ioMu, so Complete's write cannot land between the re-check and this write.
	_ = m.writeRecordUnless(rec, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		r := m.rows[id]
		return r == nil || r.state.Terminal() || r.finishing
	})
}

// Complete reports the end of a handle. The first call per handle wins and returns true;
// later ones return false. The result file is written before the signal is queued.
func (m *Manager) Complete(id string, res Result) (bool, error) {
	if !res.State.Terminal() {
		return false, ErrInvalidResult
	}
	now := m.opts.Now()
	m.mu.Lock()
	r := m.rows[id]
	if r == nil {
		m.mu.Unlock()
		return false, ErrUnknownHandle
	}
	if r.state.Terminal() || r.finishing {
		m.mu.Unlock()
		return false, nil
	}
	r.finishing = true
	m.completing++
	rec := *r.rec
	after := r.after
	m.mu.Unlock()
	if m.beforeCompleteWrite != nil {
		m.beforeCompleteWrite(id)
	}

	rec.State = res.State
	rec.OK = res.OK && res.State == StateDone
	rec.Code, _ = cutUTF8(res.Code, codeMaxBytes)
	rec.Reason, _ = cutUTF8(res.Reason, reasonMaxBytes)
	rec.Ref, _ = cutUTF8(res.Ref, refMaxBytes)
	rec.Summary, rec.Cut = cutUTF8(res.Summary, int(m.cfg.SummaryMaxBytes))
	rec.SrcCut = res.Cut
	rec.Detail, rec.DetailCut = cutUTF8(res.Detail, m.detailLimit())
	if rec.Detail != "" {
		// Entries render the detail, so the summary stays a short form instead of a second copy.
		rec.Summary, _ = cutUTF8(rec.Summary, shortSummaryBytes)
	}
	rec.ResumedFrom, _ = cutUTF8(res.ResumedFrom, refMaxBytes)
	rec.ConfigNote, _ = cutUTF8(res.ConfigNote, ConfigNoteMaxBytes)
	rec.Trace, _ = cutUTF8(res.Trace, refMaxBytes)
	rec.Cut = rec.Cut || res.Cut
	rec.Bytes = res.Bytes
	rec.EndedAt = now
	rec.Unread = after == endKeep
	// ioMu is held from the result write until the record matches the end that is
	// committed below, so Close, which shuts file writes under ioMu, cannot fall between
	// them and leave the record of a reset or deleted handle unread on disk.
	m.ioMu.Lock()
	if err := m.writeRecordIOLocked(rec); err != nil {
		m.opts.Logger.Warn("async result record not saved; the signal is kept in memory only", "handle", id, "error", err)
	}
	if m.beforeCompleteCommit != nil {
		m.beforeCompleteCommit(id)
	}

	m.mu.Lock()
	var work pruneWork
	// A reset or delete that came while the record was written raised r.after (cancel does
	// that for a finishing row too); its end wins over the one read above.
	late := false
	if r.after > after {
		after = r.after
		rec.Unread = false
		late = true
	}
	m.mutateLocked(r, func(r *row) {
		r.state, r.ok, r.changed, r.unread, r.finishing = rec.State, rec.OK, now, after == endKeep, false
		r.progress = nil
	})
	r.rec = &rec
	if after != endKeep {
		r.rec = nil
	}
	if after == endRemove {
		m.forgetLocked(r)
	}
	work.merge(m.enforceUnreadLocked(r.owner))
	work.merge(m.pruneLocked(now))
	m.notifyLocked()
	m.mu.Unlock()
	switch {
	case after == endRemove:
		m.removeRecordIOLocked(id)
	case late:
		// rec is what was just written, now read.
		if err := m.writeRecordIOLocked(rec); err != nil {
			m.opts.Logger.Warn("async record not updated", "handle", id, "error", err)
		}
	}
	m.applyWorkIOLocked(work)
	m.ioMu.Unlock()

	m.writeIndex()
	m.mu.Lock()
	m.completing--
	m.notifyLocked()
	m.mu.Unlock()
	return true, nil
}

// CancelOwner asks every live handle of owner to stop and returns how many it asked.
// It does not wait: drivers report the end through Complete.
func (m *Manager) CancelOwner(owner, reason string) int {
	return m.cancel(func(r *row) bool { return r.owner == owner }, reason, endKeep)
}

// CancelOwnersWithPrefix is CancelOwner for every owner that starts with prefix (all the
// profiles of one session).
func (m *Manager) CancelOwnersWithPrefix(prefix, reason string) int {
	return m.cancel(func(r *row) bool { return strings.HasPrefix(r.owner, prefix) }, reason, endKeep)
}

// ResetOwner is the user reset of one session: every live handle of owner is asked to stop
// and its end is recorded as already read, and the unread signals queued so far are
// dropped, because the history that gave them meaning is gone. It does not wait.
func (m *Manager) ResetOwner(owner, reason string) int {
	n := m.cancel(func(r *row) bool { return r.owner == owner }, reason, endRead)
	m.DropUnread(owner)
	return n
}

// DeleteOwnersWithPrefix is the delete of a session (every profile): live handles are asked
// to stop and are removed when they end, and the finished ones are removed now. Only the
// manager's records go; a driver's own result files follow its own retention. It does not wait.
func (m *Manager) DeleteOwnersWithPrefix(prefix, reason string) int {
	match := func(r *row) bool { return strings.HasPrefix(r.owner, prefix) }
	n := m.cancel(match, reason, endRemove)
	m.commit(false, func() pruneWork {
		var work pruneWork
		for _, r := range sortedRows(m.rows) {
			if r.state.Terminal() && !r.finishing && match(r) {
				work.removes = append(work.removes, r.id)
				m.forgetLocked(r)
			}
		}
		return work
	})
	return n
}

func (m *Manager) cancel(match func(*row) bool, reason string, after endAction) int {
	type target struct {
		driver Driver
		id     string
	}
	var targets []target
	m.mu.Lock()
	for _, r := range m.rows {
		if r.state.Terminal() || !match(r) {
			continue
		}
		d := m.drivers[r.kind]
		if d == nil && !r.finishing {
			continue
		}
		// The strongest end wins (endKeep < endRead < endRemove): a later cancel must
		// not undo a reset's born-read end or a delete's removal. A finishing row is not
		// stopped again, but Complete reads its end action once more before it commits,
		// so a reset or delete that arrives meanwhile still applies to it.
		if after > r.after {
			r.after = after
		}
		if !r.finishing {
			targets = append(targets, target{d, r.id})
		}
	}
	m.mu.Unlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].id < targets[j].id })
	for _, t := range targets {
		t.driver.Stop(t.id, reason)
	}
	return len(targets)
}

// DropUnread marks every unread signal of owner as read (a user reset: the history that
// gave them meaning is gone). The results stay readable until pruned.
func (m *Manager) DropUnread(owner string) int {
	n := 0
	m.commit(false, func() pruneWork {
		var work pruneWork
		if s := m.owners[owner]; s != nil {
			for _, r := range sortedRows(s.unread) {
				m.mutateLocked(r, func(r *row) { r.unread = false })
				r.rec = nil
				work.flips = append(work.flips, flip{id: r.id})
			}
			s.dropped = 0
		}
		n = len(work.flips)
		return work
	})
	return n
}

// Counts returns the cheap header numbers of one owner.
func (m *Manager) Counts(owner string) Counts {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.countsLocked(owner)
}

// Snapshot returns the owner's counts and the ids of its newest live handles, at most
// sse_max_ids of them.
func (m *Manager) Snapshot(owner string) (Counts, []string) {
	counts, live := m.SnapshotLive(owner)
	if live == nil {
		return counts, nil
	}
	ids := make([]string, len(live))
	for i, l := range live {
		ids[i] = l.ID
	}
	return counts, ids
}

// LiveID is a live handle id with the time it last changed, so ids of several managers can
// be merged newest first.
type LiveID struct {
	ID      string
	Changed time.Time
}

// SnapshotLive is Snapshot with the change time of each id.
func (m *Manager) SnapshotLive(owner string) (Counts, []LiveID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	counts := m.countsLocked(owner)
	s := m.owners[owner]
	if s == nil || m.cfg.SSEMaxIDs == 0 || len(s.live) == 0 {
		return counts, nil
	}
	rows := sortedRows(s.live)
	live := make([]LiveID, 0, min(len(rows), int(m.cfg.SSEMaxIDs)))
	for i := len(rows) - 1; i >= 0 && int64(len(live)) < m.cfg.SSEMaxIDs; i-- {
		live = append(live, LiveID{ID: rows[i].id, Changed: rows[i].changed})
	}
	return counts, live
}

func (m *Manager) countsLocked(owner string) Counts {
	if s := m.owners[owner]; s != nil {
		return Counts{Active: len(s.live), Unread: len(s.unread), Total: s.total}
	}
	return Counts{}
}

// Close asks every live handle of this server to stop (reason ReasonShutdown), waits up to
// StopWait for the drivers to report, wakes waiters with ErrClosed and writes the index. A
// handle of this server that has not reported by then ends as lost (ReasonShutdown),
// unread, so no record is left running; a later report of its driver is ignored. Handles
// another live server owns are left as they are: they get no Stop and are not waited for.
func (m *Manager) Close() { m.CloseWithin(0) }

// CloseWithin is Close for a caller that knows some drivers need longer than StopWait to
// report a stop (a child killed only when its kill grace ends): it waits up to the longer of
// StopWait and longest.
func (m *Manager) CloseWithin(longest time.Duration) {
	type target struct {
		driver Driver
		id     string
	}
	var targets []target
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	// Only this server's handles are stopped and waited for: a handle another live server
	// owns is never reported to this one, and waiting for it would hold Close for the whole
	// stop wait.
	own := m.ownLiveLocked()
	for _, id := range own {
		if d := m.drivers[m.rows[id].kind]; d != nil {
			targets = append(targets, target{d, id})
		}
	}
	m.notifyLocked()
	m.mu.Unlock()
	for _, t := range targets {
		t.driver.Stop(t.id, ReasonShutdown)
	}
	m.mu.Lock()
	writing := m.completing > 0
	m.mu.Unlock()
	if wait := max(m.opts.StopWait, longest); wait > 0 && (len(own) > 0 || writing) {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		for {
			m.mu.Lock()
			live, ch := m.completing, m.changed
			for _, id := range own {
				if r := m.rows[id]; r != nil && !r.state.Terminal() {
					live++
				}
			}
			m.mu.Unlock()
			if live == 0 {
				break
			}
			select {
			case <-ch:
				continue
			case <-timer.C:
			}
			break
		}
	}
	// Left running in its record, such a handle would be lost only at the next start, and a
	// server that starts while this process is still alive would take it for this one's
	// and keep it live. A handle being finished is skipped: its result is on its way.
	for _, id := range m.unreportedOwnLive() {
		_, _ = m.Complete(id, Result{State: StateLost, Reason: ReasonShutdown})
	}
	// A Complete that claimed its handle before the wait ran out has only its own files left
	// to write, no driver work, so it is waited for however long: shut first, its result
	// would be dropped and its record left running. No Complete can claim a handle of this
	// server after the loop above.
	m.mu.Lock()
	for m.completing > 0 {
		ch := m.changed
		m.mu.Unlock()
		<-ch
		m.mu.Lock()
	}
	m.mu.Unlock()
	m.shutFiles()
}

// shutFiles writes the final index and then stops every later write to the folder. The
// index is read and written under ioMu, which every change holds until its files are
// written (commit, Complete, Register), so it lists each change whose files are on disk and
// none whose files are not.
func (m *Manager) shutFiles() {
	m.indexClosed.Store(true)
	m.flushMu.Lock()
	if m.flushTimer != nil {
		m.flushTimer.Stop()
		m.flushTimer = nil
	}
	m.flushMu.Unlock()
	// A concurrent writer may own the coalesced write; wait for it. Later writers fail
	// TryLock while it is held, and find the folder shut once it is released.
	m.indexMu.Lock()
	m.ioMu.Lock()
	m.flushIndexIOLocked()
	m.ioClosed = true
	m.ioMu.Unlock()
	m.indexMu.Unlock()
}

// unreportedOwnLive lists, sorted, the live handles this server registered that are not
// being finished.
func (m *Manager) unreportedOwnLive() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ownLiveLocked()
}

// ownLiveLocked is unreportedOwnLive for a caller that holds mu.
func (m *Manager) ownLiveLocked() []string {
	var ids []string
	for _, r := range m.rows {
		if r.state.Terminal() || r.finishing || r.rec == nil {
			continue
		}
		if r.rec.Server != nil && *r.rec.Server != m.opts.Server {
			continue
		}
		ids = append(ids, r.id)
	}
	sort.Strings(ids)
	return ids
}

func sortedRows(set map[string]*row) []*row {
	rows := make([]*row, 0, len(set))
	for _, r := range set {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].changed.Equal(rows[j].changed) {
			return rows[i].changed.Before(rows[j].changed)
		}
		return rows[i].id < rows[j].id
	})
	return rows
}

// BlockDuration clamps a start tool's block_ms: nil means default_block_ms.
func (c Config) BlockDuration(ms *int64) time.Duration {
	value := c.DefaultBlockMs
	if ms != nil {
		value = min(max(*ms, 0), c.MaxBlockMs)
	}
	return time.Duration(value) * time.Millisecond
}

// WaitDuration clamps the async tool's wait_ms: nil means default_wait_ms.
func (c Config) WaitDuration(ms *int64) time.Duration {
	value := c.DefaultWaitMs
	if ms != nil {
		value = min(max(*ms, 0), c.MaxWaitMs)
	}
	return time.Duration(value) * time.Millisecond
}
