package asyncmgr

import (
	"context"
	"fmt"
	"time"
)

// WaitResult is the outcome of Wait. Signals is empty on timeout, cancellation-free
// polls and when nothing is pending.
type WaitResult struct {
	Signals []Entry
	// More is true when ready signals remain queued after the count or byte limit.
	More bool
	// Pending is the number of live handles in scope after the call.
	Pending int
	// Dropped counts unread signals lost to the unread cap or TTL since the last report.
	Dropped int
}

// Wait returns as soon as the owner has at least one unread signal, and then returns all
// signals ready at that moment, oldest first, up to wait_max_signals and the response
// byte budget; the rest stay queued (More). ids narrows the scope to those handles; an id
// the owner does not have is ErrUnknownHandle. With nothing live and nothing queued, or
// with wait <= 0, it returns at once. A cancelled ctx returns ctx.Err() and consumes
// nothing.
func (m *Manager) Wait(ctx context.Context, owner string, ids []string, wait time.Duration) (WaitResult, error) {
	var scope map[string]struct{}
	var unknown error
	m.commit(false, func() pruneWork {
		if len(ids) > 0 {
			scope = make(map[string]struct{}, len(ids))
			for _, id := range ids {
				r := m.rows[id]
				if r == nil || r.owner != owner {
					unknown = fmt.Errorf("%w: %q", ErrUnknownHandle, id)
					return pruneWork{}
				}
				scope[id] = struct{}{}
			}
		}
		return m.pruneLocked(m.opts.Now())
	})
	if unknown != nil {
		return WaitResult{}, unknown
	}

	var timer *time.Timer
	var timeout <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	// A delivery holds ioMu from the pop until the signals are marked read on disk, so Close
	// cannot shut the folder between them and deliver them again after a restart.
	for {
		m.ioMu.Lock()
		m.mu.Lock()
		if err := ctx.Err(); err != nil {
			m.mu.Unlock()
			m.ioMu.Unlock()
			return WaitResult{}, err
		}
		if m.closed {
			m.mu.Unlock()
			m.ioMu.Unlock()
			return WaitResult{}, ErrClosed
		}
		result, flips := m.popLocked(owner, scope)
		if len(result.Signals) > 0 || wait <= 0 || result.Pending == 0 {
			if s := m.owners[owner]; s != nil && (len(result.Signals) > 0 || result.Pending == 0) {
				result.Dropped = m.takeDroppedLocked(owner, s)
			}
			m.mu.Unlock()
			m.deliverIOLocked(flips)
			return result, nil
		}
		changed := m.changed
		m.mu.Unlock()
		m.ioMu.Unlock()
		if timer == nil {
			timer = time.NewTimer(wait)
			timeout = timer.C
		}
		select {
		case <-changed:
		case <-timeout:
			if m.beforeWaitTimeout != nil {
				m.beforeWaitTimeout()
			}
			m.ioMu.Lock()
			m.mu.Lock()
			if err := ctx.Err(); err != nil {
				m.mu.Unlock()
				m.ioMu.Unlock()
				return WaitResult{}, err
			}
			result, flips = m.popLocked(owner, scope)
			if s := m.owners[owner]; s != nil {
				result.Dropped = m.takeDroppedLocked(owner, s)
			}
			m.mu.Unlock()
			m.deliverIOLocked(flips)
			return result, nil
		case <-ctx.Done():
			return WaitResult{}, ctx.Err()
		}
	}
}

// takeDroppedLocked reports the dropped count once and clears it. An owner with no rows
// left has nothing to keep its entry for, so the entry goes with the count.
func (m *Manager) takeDroppedLocked(owner string, s *ownerStats) int {
	dropped := s.dropped
	s.dropped = 0
	if s.total == 0 {
		delete(m.owners, owner)
	}
	return dropped
}

// deliverIOLocked marks the popped signals read on disk, releases ioMu and rewrites the
// index when anything was marked.
func (m *Manager) deliverIOLocked(flips []flip) {
	w := pruneWork{flips: flips}
	if m.beforeCommitWrite != nil {
		m.beforeCommitWrite()
	}
	m.applyWorkIOLocked(w)
	m.ioMu.Unlock()
	if !w.empty() {
		m.writeIndex()
	}
}

// popLocked takes the ready signals of owner (inside scope when scope is set) within the
// count and byte limits and marks them read.
func (m *Manager) popLocked(owner string, scope map[string]struct{}) (WaitResult, []flip) {
	var result WaitResult
	s := m.owners[owner]
	if s == nil {
		return result, nil
	}
	if scope == nil {
		result.Pending = len(s.live)
	} else {
		for id := range scope {
			if _, ok := s.live[id]; ok {
				result.Pending++
			}
		}
	}
	ready := sortedRows(s.unread)
	if scope != nil {
		filtered := ready[:0]
		for _, r := range ready {
			if _, ok := scope[r.id]; ok {
				filtered = append(filtered, r)
			}
		}
		ready = filtered
	}
	budget := int(m.cfg.ResponseMaxBytes) - envelopeBytes
	used := 0
	var taken []*row
	for _, r := range ready {
		if int64(len(taken)) >= m.cfg.WaitMaxSignals {
			break
		}
		entry := entryOf(*r.rec, int(m.cfg.SummaryMaxBytes), m.detailLimit())
		if len(taken) == 0 {
			entry = fitEntry(entry, budget)
		}
		size := encodedSize(entry) + 1
		if used+size > budget {
			break
		}
		used += size
		result.Signals = append(result.Signals, entry)
		taken = append(taken, r)
	}
	result.More = len(taken) < len(ready)
	flips := make([]flip, 0, len(taken))
	for _, r := range taken {
		flips = append(flips, m.markReadLocked(r, false))
	}
	return result, flips
}
