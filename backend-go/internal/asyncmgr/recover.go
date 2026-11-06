package asyncmgr

import (
	"errors"
	"io/fs"
	"sort"
	"time"
)

// recover loads the index and the records that are live or unread. Terminal read
// history is not opened. A missing or corrupt index is rebuilt once from the record
// files (names first, then the files).
func (m *Manager) recover() error {
	if err := mkdirAll(m.dir); err != nil {
		return err
	}
	now := m.opts.Now()
	indexed, ok := m.readIndex()
	var loaded []Record
	if ok {
		for _, ir := range indexed {
			r := &row{id: ir.ID, owner: ir.Owner, kind: ir.Kind, state: ir.State, ok: ir.OK, label: ir.Label,
				started: ir.Started, changed: ir.Changed, unread: ir.Unread, overflow: ir.Overflow}
			if r.state.Terminal() && !r.unread {
				m.addRowLocked(r)
				continue
			}
			rec, err := m.readRecord(ir.ID)
			if err != nil {
				m.opts.Logger.Warn("async record unreadable; dropped", "handle", ir.ID, "error", err)
				continue
			}
			r.rec = &rec
			loaded = append(loaded, rec)
			m.addRowLocked(r)
		}
		// Record files the index does not know (written after its last rewrite) are
		// read too: the names are cheap, and an unknown file is always opened.
		for _, id := range m.recordNames() {
			if m.rows[id] != nil {
				continue
			}
			if rec, err := m.readRecord(id); err == nil {
				loaded = append(loaded, m.adopt(rec))
			}
		}
	} else {
		names := m.recordNames()
		for _, id := range names {
			if rec, err := m.readRecord(id); err == nil {
				loaded = append(loaded, m.adopt(rec))
			}
		}
		m.opts.Logger.Info("async index rebuilt from records", "records", len(names))
	}

	sort.Slice(loaded, func(i, j int) bool { return loaded[i].ID < loaded[j].ID })
	for _, rec := range loaded {
		r := m.rows[rec.ID]
		if rec.State.Terminal() {
			// The record is written before the delayed index, so a crash in between leaves
			// the index row live for a handle that already finished.
			if r != nil && !r.state.Terminal() {
				m.mutateLocked(r, func(r *row) {
					r.state, r.ok, r.changed, r.unread = rec.State, rec.OK, rec.EndedAt, rec.Unread
				})
				if !rec.Unread {
					r.rec = nil
				}
			}
			continue
		}
		if r == nil {
			continue
		}
		if m.foreignLive(rec) {
			m.foreign[rec.ID] = *rec.Server
			continue
		}
		rec.State, rec.OK, rec.Reason = StateLost, false, ReasonRestarted
		rec.EndedAt, rec.Unread = now, true
		m.mutateLocked(r, func(r *row) { r.state, r.ok, r.changed, r.unread = StateLost, false, now, true })
		r.rec = &rec
		if err := m.writeRecord(rec); err != nil {
			m.opts.Logger.Warn("lost async record not saved", "handle", rec.ID, "error", err)
		}
	}
	work := pruneWork{}
	for owner := range m.owners {
		work.merge(m.enforceUnreadLocked(owner))
	}
	work.merge(m.pruneLocked(now))
	m.applyWork(work, false)
	m.foreignN.Store(int32(len(m.foreign)))
	m.flushIndexNow()
	return nil
}

// endDeadForeign ends the handles kept live at Open for another server that has stopped
// since, with the end that server recorded or else as lost (endForeign): it can no longer
// report them here, so they would show running and count against the caps until this
// server restarts. It runs at the start of the calls that read or add handles (Counts and
// the snapshots included) and in the background check (peerCheck), checks each such
// server once, outside the locks, and costs nothing when no foreign handle is live.
func (m *Manager) endDeadForeign() {
	m.checkForeign(false)
}

// checkForeign is endDeadForeign; with readLive it also reads again the records of the
// handles of a server still running and takes an end that server recorded since Open
// (endForeign with alive): it reports its own handles' ends only to itself, so without the
// read they would show running here, hold the caps and waits, and refuse a stop until it
// stops. Only the background check reads them, one record per such handle each
// peer_check_ms; the calls that read or add handles do not pay for it.
func (m *Manager) checkForeign(readLive bool) {
	if m.foreignN.Load() == 0 || m.opts.ServerAlive == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.pruneForeignLocked()
	byServer := map[ServerID][]string{}
	for id, server := range m.foreign {
		if !m.rows[id].finishing {
			byServer[server] = append(byServer[server], id)
		}
	}
	m.mu.Unlock()
	for server, ids := range byServer {
		alive := m.opts.ServerAlive(server)
		if alive && !readLive {
			continue
		}
		sort.Strings(ids)
		for _, id := range ids {
			m.endForeign(id, alive)
		}
	}
}

// pruneForeignLocked forgets the foreign handles that ended or are gone.
func (m *Manager) pruneForeignLocked() {
	for id := range m.foreign {
		if r := m.rows[id]; r == nil || r.state.Terminal() {
			delete(m.foreign, id)
		}
	}
	m.foreignN.Store(int32(len(m.foreign)))
}

// armPeerCheckLocked schedules the next background check of the servers whose handles are
// kept live (peer_check_ms), when there are any and none is scheduled, so a dead one's
// handles end even while no call reads them: a wait in progress is woken by their end and
// a watcher (the run stream's counts) is told. Called with mu held.
func (m *Manager) armPeerCheckLocked() {
	if m.closed || m.peerTimer != nil || m.cfg.PeerCheckMs <= 0 || m.opts.ServerAlive == nil {
		return
	}
	if m.pruneForeignLocked(); len(m.foreign) == 0 {
		return
	}
	m.peerChecks.Add(1)
	m.peerTimer = time.AfterFunc(time.Duration(m.cfg.PeerCheckMs)*time.Millisecond, m.peerCheck)
}

// peerCheck is one background check: it ends the handles of the servers that stopped, takes
// the ends a running one recorded (checkForeign) and schedules the next check while handles
// of a running one remain.
func (m *Manager) peerCheck() {
	defer m.peerChecks.Done()
	m.mu.Lock()
	m.peerTimer = nil
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return
	}
	m.checkForeign(true)
	m.mu.Lock()
	m.armPeerCheckLocked()
	m.mu.Unlock()
}

// endForeign ends one handle of a stopped server. Its record is read again first: that
// server may have ended the handle itself before it stopped (a result, a stop at its
// shutdown, a delete), while this server's row still holds what Open read. Only a record
// still live is ended as lost, as the next start would; a terminal record is taken as it
// is, and a missing one drops the row. An unreadable one (a newer build's version, say) also
// drops the row, as recover does, and is left as it is rather than overwritten from the
// stale row. A reset or delete this server asked for meanwhile (r.after) still applies.
// With alive the server still runs: only an end it recorded (a terminal or missing record)
// is taken, and a live or unreadable record leaves the row as it is, still that server's.
func (m *Manager) endForeign(id string, alive bool) {
	rec, err := m.readRecord(id)
	missing := errors.Is(err, fs.ErrNotExist)
	unreadable := err != nil && !missing
	if alive && (unreadable || (err == nil && !rec.State.Terminal())) {
		return
	}
	if err == nil && !rec.State.Terminal() {
		_, _ = m.Complete(id, Result{State: StateLost, Reason: ReasonRestarted})
		return
	}
	if unreadable {
		m.opts.Logger.Warn("async record of a stopped server unreadable; dropped", "handle", id, "error", err)
	}
	var w pruneWork
	changed := false
	m.ioMu.Lock()
	m.mu.Lock()
	if r := m.rows[id]; r != nil && !m.closed && !r.state.Terminal() && !r.finishing {
		changed = true
		if missing || unreadable || r.after == endRemove {
			m.forgetLocked(r)
			if !missing && r.after == endRemove {
				w.removes = append(w.removes, id)
			}
		} else {
			unread := rec.Unread && r.after == endKeep
			ended := rec.EndedAt
			if ended.IsZero() {
				ended = m.opts.Now()
			}
			m.mutateLocked(r, func(r *row) {
				r.state, r.ok, r.changed, r.unread, r.progress = rec.State, rec.OK, ended, unread, nil
				r.overflow = r.overflow || rec.Overflow
			})
			r.rec = nil
			if unread {
				r.rec = &rec
			} else if rec.Unread {
				w.flips = append(w.flips, flip{id: id})
			}
			w.merge(m.enforceUnreadLocked(r.owner))
		}
		w.merge(m.pruneLocked(m.opts.Now()))
		m.notifyLocked()
	}
	m.mu.Unlock()
	m.applyWorkIOLocked(w)
	m.ioMu.Unlock()
	if changed {
		m.writeIndex()
	}
}

// adopt adds a row built from a record found without an index entry.
func (m *Manager) adopt(rec Record) Record {
	r := &row{id: rec.ID, owner: rec.Owner, kind: rec.Kind, state: rec.State, ok: rec.OK, label: rec.Label,
		started: rec.StartedAt, unread: rec.Unread, overflow: rec.Overflow}
	r.changed = rec.EndedAt
	if r.changed.IsZero() {
		r.changed = rec.StartedAt
	}
	if r.unread || !r.state.Terminal() {
		r.rec = &rec
	}
	m.addRowLocked(r)
	return rec
}

// foreignLive reports a non-terminal record that another, still running server wrote:
// it is that server's to end, so it is neither lost nor counted... it stays tracked as
// it was.
func (m *Manager) foreignLive(rec Record) bool {
	if rec.Server == nil || m.opts.ServerAlive == nil {
		return false
	}
	if rec.Server.PID == m.opts.Server.PID && rec.Server.Start == m.opts.Server.Start {
		return false
	}
	return m.opts.ServerAlive(*rec.Server)
}
