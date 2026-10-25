package asyncmgr

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ListQuery selects one page of an owner's handles. State is "active" (default: live
// handles and unread terminal ones), "all", or one terminal state name.
type ListQuery struct {
	Owner  string
	State  string
	Kind   Kind
	Limit  int64
	Cursor string
}

// ListResult is one page, newest first. Items are summaries only.
type ListResult struct {
	Counts Counts
	Items  []ListItem
	// Next is the opaque cursor of the next page; empty on the last page.
	Next string
	// Dropped counts unread signals lost to the unread cap or TTL.
	Dropped int
}

func sortIndexRows(rows []indexRow) {
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].Started.Equal(rows[j].Started) {
			return rows[i].Started.Before(rows[j].Started)
		}
		return rows[i].ID < rows[j].ID
	})
}

func encodeCursor(changed time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(changed.UnixNano(), 10) + "|" + id))
}

func decodeCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", ErrInvalidCursor
	}
	nanos, id, found := strings.Cut(string(raw), "|")
	value, parseErr := strconv.ParseInt(nanos, 10, 64)
	if !found || parseErr != nil || id == "" {
		return time.Time{}, "", ErrInvalidCursor
	}
	return time.Unix(0, value), id, nil
}

func stateMatches(r *row, state string) bool {
	switch state {
	case "", "active":
		return !r.state.Terminal() || r.unread
	case "all":
		return true
	case "done", "failed", "stopped", "lost", "timed_out":
		return string(r.state) == state
	}
	return false
}

// List returns one page. It works on the in-memory rows only (bounded by keep_finished
// plus the live and unread handles) and opens no file.
func (m *Manager) List(q ListQuery) (ListResult, error) {
	switch q.State {
	case "", "active", "all", "done", "failed", "stopped", "lost", "timed_out":
	default:
		return ListResult{}, fmt.Errorf("unknown state filter %q", q.State)
	}
	var after time.Time
	var afterID string
	if q.Cursor != "" {
		var err error
		if after, afterID, err = decodeCursor(q.Cursor); err != nil {
			return ListResult{}, err
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = m.cfg.ListDefaultLimit
	}
	limit = min(limit, m.cfg.ListMaxLimit)

	m.mu.Lock()
	var matched []*row
	collect := func(r *row) {
		if r.owner == q.Owner && (q.Kind == "" || r.kind == q.Kind) && stateMatches(r, q.State) {
			matched = append(matched, r)
		}
	}
	if q.State == "" || q.State == "active" {
		if s := m.owners[q.Owner]; s != nil {
			for _, r := range s.live {
				collect(r)
			}
			for _, r := range s.unread {
				collect(r)
			}
		}
	} else {
		for _, r := range m.rows {
			collect(r)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].changed.Equal(matched[j].changed) {
			return matched[i].changed.After(matched[j].changed)
		}
		return matched[i].id > matched[j].id
	})
	result := ListResult{Counts: m.countsLocked(q.Owner)}
	if s := m.owners[q.Owner]; s != nil {
		result.Dropped = s.dropped
	}
	budget := int(m.cfg.ResponseMaxBytes) - envelopeBytes
	used := 0
	for _, r := range matched {
		if q.Cursor != "" && (r.changed.After(after) || (r.changed.Equal(after) && r.id >= afterID)) {
			continue
		}
		item := ListItem{ID: r.id, Kind: r.kind, State: r.state, OK: r.ok, Label: r.label, Unread: r.unread, T: formatTime(r.changed)}
		size := encodedSize(item) + 1
		if int64(len(result.Items)) >= limit || (len(result.Items) > 0 && used+size > budget) {
			last := m.rows[result.Items[len(result.Items)-1].ID]
			result.Next = encodeCursor(last.changed, last.id)
			break
		}
		used += size
		result.Items = append(result.Items, item)
	}
	m.mu.Unlock()
	return result, nil
}

// Status returns the entry of one handle without consuming its signal. A driver's
// summary comes from its driver (Detailer) when it has one. The summary is cut to
// status_summary_max_bytes and to the response budget.
func (m *Manager) Status(owner, id string) (Entry, error) {
	m.mu.Lock()
	r := m.rows[id]
	if r == nil || r.owner != owner {
		m.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: %q", ErrUnknownHandle, id)
	}
	var rec Record
	have := r.rec != nil
	if have {
		rec = *r.rec
	}
	driver := m.drivers[r.kind]
	state, changed := r.state, r.changed
	m.mu.Unlock()

	if !have {
		var err error
		if rec, err = m.readRecord(id); err != nil {
			m.opts.Logger.Warn("async record unreadable", "handle", id, "error", err)
			return Entry{ID: id, Kind: r.kind, State: state, OK: r.ok, Label: r.label, T: formatTime(changed)}, nil
		}
	}
	limit := int(m.cfg.StatusSummaryBytes)
	if detailer, ok := driver.(Detailer); ok {
		if text := detailer.Detail(id, limit); text != "" {
			rec.Summary, rec.Detail = text, ""
		}
	}
	rec.Cut = rec.SrcCut
	entry := entryOf(rec, limit, m.detailLimit())
	return fitEntry(entry, int(m.cfg.ResponseMaxBytes)-envelopeBytes), nil
}

// Stop asks the driver to stop a live handle and waits up to StopWait for it to report.
// It is idempotent: an ended handle returns its terminal entry. Returning a terminal
// entry consumes that handle's unread signal. If the driver has not reported by then the
// entry is still live and the stop is in progress.
func (m *Manager) Stop(ctx context.Context, owner, id, reason string) (Entry, error) {
	m.mu.Lock()
	r := m.rows[id]
	if r == nil || r.owner != owner {
		m.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: %q", ErrUnknownHandle, id)
	}
	driver := m.drivers[r.kind]
	live := !r.state.Terminal()
	m.mu.Unlock()

	if live && driver != nil {
		driver.Stop(id, reason)
		m.awaitTerminal(ctx, id)
	}
	entry, err := m.Status(owner, id)
	if err != nil {
		return Entry{}, err
	}
	if entry.State.Terminal() {
		m.consume(id)
	}
	return entry, nil
}

func (m *Manager) awaitTerminal(ctx context.Context, id string) {
	if m.opts.StopWait <= 0 {
		return
	}
	timer := time.NewTimer(m.opts.StopWait)
	defer timer.Stop()
	for {
		m.mu.Lock()
		r := m.rows[id]
		done := r == nil || r.state.Terminal()
		ch := m.changed
		m.mu.Unlock()
		if done {
			return
		}
		select {
		case <-ch:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// MarkRead consumes the unread signal of a handle whose result the caller already
// returned to the model (a start tool that finished within block_ms), so a later wait
// does not deliver it again. An unknown id, another owner's handle or a read one is a no-op.
func (m *Manager) MarkRead(owner, id string) {
	m.mu.Lock()
	r := m.rows[id]
	mine := r != nil && r.owner == owner
	m.mu.Unlock()
	if mine {
		m.consume(id)
	}
}

func (m *Manager) consume(id string) {
	m.mu.Lock()
	r := m.rows[id]
	if r == nil || !r.unread {
		m.mu.Unlock()
		return
	}
	f := m.markReadLocked(r, false)
	m.mu.Unlock()
	m.applyWork(pruneWork{flips: []flip{f}}, false)
}
