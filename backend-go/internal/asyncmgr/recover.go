package asyncmgr

import (
	"sort"
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

// endDeadForeign ends as lost (ReasonRestarted, unread) the handles kept live at Open for
// another server that has stopped since: that server can no longer report them, so they
// would show running and count against the caps until this server restarts. It runs at the
// start of the calls that read or add handles, checks each such server once, outside the
// locks, and costs nothing when no foreign handle is live.
func (m *Manager) endDeadForeign() {
	if m.foreignN.Load() == 0 || m.opts.ServerAlive == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	byServer := map[ServerID][]string{}
	for id, server := range m.foreign {
		r := m.rows[id]
		if r == nil || r.state.Terminal() {
			delete(m.foreign, id)
			continue
		}
		if !r.finishing {
			byServer[server] = append(byServer[server], id)
		}
	}
	m.foreignN.Store(int32(len(m.foreign)))
	m.mu.Unlock()
	for server, ids := range byServer {
		if m.opts.ServerAlive(server) {
			continue
		}
		sort.Strings(ids)
		for _, id := range ids {
			_, _ = m.Complete(id, Result{State: StateLost, Reason: ReasonRestarted})
		}
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
