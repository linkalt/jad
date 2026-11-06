package asyncmgr

import (
	"math"
	"sort"
	"time"
)

// DaysDuration converts days to a Duration, saturating instead of wrapping so a huge
// value can never turn a prune cutoff into a time in the future.
func DaysDuration(days int64) time.Duration {
	const day = 24 * time.Hour
	if days > int64(math.MaxInt64/day) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(days) * day
}

// flip is a record whose unread flag must be cleared on disk.
type flip struct {
	id       string
	overflow bool
}

// pruneWork is file work decided under the manager lock and done after releasing it.
type pruneWork struct {
	flips   []flip
	removes []string // handle ids whose record file is removed
}

func (w *pruneWork) merge(o pruneWork) {
	w.flips = append(w.flips, o.flips...)
	w.removes = append(w.removes, o.removes...)
}

func (w pruneWork) empty() bool { return len(w.flips) == 0 && len(w.removes) == 0 }

// applyWork does the file work and, when anything changed or index is true, rewrites the
// index. It must be called without m.mu or m.ioMu held.
func (m *Manager) applyWork(w pruneWork, index bool) {
	m.ioMu.Lock()
	m.applyWorkIOLocked(w)
	m.ioMu.Unlock()
	if index || !w.empty() {
		m.writeIndex()
	}
}

// applyWorkIOLocked does the record work of w for a caller that holds ioMu (not mu); the
// caller rewrites the index once it has released ioMu.
func (m *Manager) applyWorkIOLocked(w pruneWork) {
	for _, f := range w.flips {
		m.updateRecordIOLocked(f.id, func(rec *Record) {
			rec.Unread = false
			if f.overflow {
				rec.Overflow = true
			}
		})
	}
	for _, id := range w.removes {
		m.removeRecordIOLocked(id)
	}
}

// commit makes an in-memory change and writes its files under ioMu, which Close takes to
// write the final index and shut file writes: a change is then either on disk and in that
// index, or not made before Close. change runs with mu held and returns the file work.
// The index is rewritten afterwards when index is true or the change had file work.
func (m *Manager) commit(index bool, change func() pruneWork) {
	m.ioMu.Lock()
	m.mu.Lock()
	w := change()
	m.mu.Unlock()
	if m.beforeCommitWrite != nil {
		m.beforeCommitWrite()
	}
	m.applyWorkIOLocked(w)
	m.ioMu.Unlock()
	if index || !w.empty() {
		m.writeIndex()
	}
}

// markReadLocked clears the unread flag of r and drops its in-memory record.
func (m *Manager) markReadLocked(r *row, overflow bool) flip {
	m.mutateLocked(r, func(r *row) {
		r.unread = false
		if overflow {
			r.overflow = true
		}
	})
	r.rec = nil
	return flip{id: r.id, overflow: overflow}
}

// enforceUnreadLocked keeps at most unread_max unread signals for owner: the oldest ones
// are marked read-by-overflow and counted in dropped.
func (m *Manager) enforceUnreadLocked(owner string) pruneWork {
	var w pruneWork
	s := m.owners[owner]
	if s == nil || int64(len(s.unread)) <= m.cfg.UnreadMax {
		return w
	}
	rows := sortedRows(s.unread)
	for _, r := range rows[:int64(len(rows))-m.cfg.UnreadMax] {
		w.flips = append(w.flips, m.markReadLocked(r, true))
		s.dropped++
	}
	return w
}

// pruneLocked expires old unread signals and removes terminal, read records beyond
// keep_finished or older than retention_days. Live handles and unread signals younger
// than the TTL are never touched by the count rule.
func (m *Manager) pruneLocked(now time.Time) pruneWork {
	var w pruneWork
	if m.finished <= int(m.cfg.KeepFinished) && (m.nextExpiry.IsZero() || now.Before(m.nextExpiry)) {
		return w
	}
	defer m.recomputeExpiryLocked()
	if m.cfg.UnreadTTLDays > 0 {
		cutoff := now.Add(-DaysDuration(m.cfg.UnreadTTLDays))
		for _, s := range m.owners {
			for _, r := range sortedRows(s.unread) {
				if r.changed.Before(cutoff) {
					w.flips = append(w.flips, m.markReadLocked(r, true))
					s.dropped++
				}
			}
		}
	}
	var finished []*row
	for _, r := range m.rows {
		if r.state.Terminal() && !r.unread && !r.finishing {
			finished = append(finished, r)
		}
	}
	if len(finished) == 0 {
		return w
	}
	sort.Slice(finished, func(i, j int) bool {
		if !finished[i].changed.Equal(finished[j].changed) {
			return finished[i].changed.Before(finished[j].changed)
		}
		return finished[i].id < finished[j].id
	})
	drop := 0
	if excess := int64(len(finished)) - m.cfg.KeepFinished; excess > 0 {
		drop = int(excess)
	}
	if m.cfg.RetentionDays > 0 {
		cutoff := now.Add(-DaysDuration(m.cfg.RetentionDays))
		for drop < len(finished) && finished[drop].changed.Before(cutoff) {
			drop++
		}
	}
	for _, r := range finished[:drop] {
		w.removes = append(w.removes, r.id)
		m.forgetLocked(r)
	}
	return w
}

// noteExpiryLocked lowers nextExpiry to the moment r may expire.
func (m *Manager) noteExpiryLocked(r *row) {
	var days int64
	switch {
	case r.unread:
		days = m.cfg.UnreadTTLDays
	case r.state.Terminal():
		days = m.cfg.RetentionDays
	}
	if days <= 0 {
		return
	}
	at := r.changed.Add(DaysDuration(days))
	if m.nextExpiry.IsZero() || at.Before(m.nextExpiry) {
		m.nextExpiry = at
	}
}

// recomputeExpiryLocked sets nextExpiry exactly from the rows left after a scan.
func (m *Manager) recomputeExpiryLocked() {
	m.nextExpiry = time.Time{}
	for _, r := range m.rows {
		m.noteExpiryLocked(r)
	}
}

// Prune applies the retention rules now.
func (m *Manager) Prune() {
	m.commit(false, func() pruneWork { return m.pruneLocked(m.opts.Now()) })
}
