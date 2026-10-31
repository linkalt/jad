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

	// ioMu serializes file writes. Lock order: ioMu, then mu. Nothing takes ioMu while
	// holding mu.
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
		drivers: map[Kind]Driver{}, changed: make(chan struct{}),
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

	m.mu.Lock()
	switch {
	case m.closed:
		m.mu.Unlock()
		return ErrClosed
	case m.drivers[reg.Kind] == nil:
		m.mu.Unlock()
		return fmt.Errorf("%w %q", ErrNoDriver, reg.Kind)
	case m.rows[reg.ID] != nil:
		m.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrDuplicate, reg.ID)
	}
	if s := m.owners[reg.Owner]; s != nil && int64(len(s.live)) >= m.cfg.MaxHandles {
		live := len(s.live)
		m.mu.Unlock()
		return &CapacityError{Scope: "owner", Limit: m.cfg.MaxHandles, Live: int64(live)}
	}
	if int64(m.liveTotal) >= m.cfg.MaxHandlesTotal {
		live := m.liveTotal
		m.mu.Unlock()
		return &CapacityError{Scope: "total", Limit: m.cfg.MaxHandlesTotal, Live: int64(live)}
	}
	r := &row{id: reg.ID, owner: reg.Owner, kind: reg.Kind, state: state, label: label, started: now, changed: now, rec: &rec}
	m.addRowLocked(r)
	m.mu.Unlock()

	if err := m.writeRecord(rec); err != nil {
		m.mu.Lock()
		m.forgetLocked(r)
		m.mu.Unlock()
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
	rec.Trace, _ = cutUTF8(res.Trace, refMaxBytes)
	rec.Cut = rec.Cut || res.Cut
	rec.Bytes = res.Bytes
	rec.EndedAt = now
	rec.Unread = after == endKeep
	if err := m.writeRecord(rec); err != nil {
		m.opts.Logger.Warn("async result record not saved; the signal is kept in memory only", "handle", id, "error", err)
	}

	m.mu.Lock()
	m.mutateLocked(r, func(r *row) {
		r.state, r.ok, r.changed, r.unread, r.finishing = rec.State, rec.OK, now, after == endKeep, false
		r.progress = nil
	})
	r.rec = &rec
	if after != endKeep {
		r.rec = nil
	}
	var work pruneWork
	if after == endRemove {
		work.removes = append(work.removes, r.id)
		m.forgetLocked(r)
	}
	work.merge(m.enforceUnreadLocked(r.owner))
	work.merge(m.pruneLocked(now))
	m.notifyLocked()
	m.mu.Unlock()

	m.applyWork(work, true)
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
	var work pruneWork
	m.mu.Lock()
	for _, r := range sortedRows(m.rows) {
		if r.state.Terminal() && !r.finishing && match(r) {
			work.removes = append(work.removes, r.id)
			m.forgetLocked(r)
		}
	}
	m.mu.Unlock()
	m.applyWork(work, false)
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
		if !r.state.Terminal() && !r.finishing && match(r) {
			if d := m.drivers[r.kind]; d != nil {
				// The strongest end wins (endKeep < endRead < endRemove): a later cancel must
				// not undo a reset's born-read end or a delete's removal.
				if after > r.after {
					r.after = after
				}
				targets = append(targets, target{d, r.id})
			}
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
	var work pruneWork
	m.mu.Lock()
	if s := m.owners[owner]; s != nil {
		for _, r := range sortedRows(s.unread) {
			m.mutateLocked(r, func(r *row) { r.unread = false })
			r.rec = nil
			work.flips = append(work.flips, flip{id: r.id})
		}
		s.dropped = 0
	}
	m.mu.Unlock()
	m.applyWork(work, false)
	return len(work.flips)
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

// Close asks every live handle to stop (reason ReasonShutdown), waits up to StopWait for
// the drivers to report, wakes waiters with ErrClosed and writes the index. Handles that
// have not reported by then stay running in their record and are lost at the next start.
func (m *Manager) Close() {
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
	for _, r := range m.rows {
		if !r.state.Terminal() && !r.finishing {
			if d := m.drivers[r.kind]; d != nil {
				targets = append(targets, target{d, r.id})
			}
		}
	}
	m.notifyLocked()
	m.mu.Unlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].id < targets[j].id })
	for _, t := range targets {
		t.driver.Stop(t.id, ReasonShutdown)
	}
	m.mu.Lock()
	writing := m.completing > 0
	m.mu.Unlock()
	if m.opts.StopWait > 0 && (len(targets) > 0 || writing) {
		timer := time.NewTimer(m.opts.StopWait)
		defer timer.Stop()
		for {
			m.mu.Lock()
			live, ch := m.liveTotal+m.completing, m.changed
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
	m.indexClosed.Store(true)
	m.flushMu.Lock()
	if m.flushTimer != nil {
		m.flushTimer.Stop()
		m.flushTimer = nil
	}
	m.flushMu.Unlock()
	m.flushIndexNow()
	// A concurrent writer may own the coalesced write; wait for it, then shut the folder.
	m.indexMu.Lock()
	m.indexMu.Unlock()
	m.ioMu.Lock()
	m.ioClosed = true
	m.ioMu.Unlock()
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
