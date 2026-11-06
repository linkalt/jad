package asyncmgr

import (
	"encoding/json"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	indexName    = "index"
	indexFile    = "index.json"
	indexVersion = 1
	tempPrefix   = ".tmp-"
)

// indexRow is the compact form of a handle kept in index.json: enough for list and for
// startup to decide what to open. Detail lives in the record file.
type indexRow struct {
	ID       string    `json:"id"`
	Owner    string    `json:"owner"`
	Kind     Kind      `json:"kind"`
	State    State     `json:"state"`
	OK       bool      `json:"ok,omitempty"`
	Label    string    `json:"label,omitempty"`
	Started  time.Time `json:"started"`
	Changed  time.Time `json:"changed"`
	Unread   bool      `json:"unread,omitempty"`
	Overflow bool      `json:"overflow,omitempty"`
}

type indexFileBody struct {
	Version int             `json:"version"`
	Sum     string          `json:"sum"`
	Rows    json.RawMessage `json:"rows"`
}

func (m *Manager) recordPath(id string) string { return filepath.Join(m.dir, id+".json") }

// writeFileAtomic writes through a temp file in the same folder, then renames it over
// the target, as the process and sub-agent records do.
func writeFileAtomic(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), tempPrefix+"*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	_, writeErr := temp.Write(data)
	if writeErr == nil {
		writeErr = temp.Sync()
	}
	closeErr := temp.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tempPath, path)
}

func (m *Manager) writeRecord(rec Record) error {
	return m.writeRecordUnless(rec, nil)
}

// writeRecordUnless writes rec under ioMu unless skip reports true. skip runs with ioMu
// held, so it may take mu (the lock order is ioMu, then mu) but must not write a record.
func (m *Manager) writeRecordUnless(rec Record, skip func() bool) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	m.ioMu.Lock()
	defer m.ioMu.Unlock()
	if m.ioClosed || (skip != nil && skip()) {
		return nil
	}
	return writeFileAtomic(m.recordPath(rec.ID), append(data, '\n'))
}

// writeRecordIOLocked is writeRecord for a caller that holds ioMu.
func (m *Manager) writeRecordIOLocked(rec Record) error {
	if m.ioClosed {
		return nil
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writeFileAtomic(m.recordPath(rec.ID), append(data, '\n'))
}

func (m *Manager) readRecord(id string) (Record, error) {
	data, err := os.ReadFile(m.recordPath(id))
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, err
	}
	if rec.Version != recordVersion || rec.ID != id || rec.Owner == "" || rec.Kind == "" {
		return Record{}, errors.New("unrecognized async record")
	}
	return rec, nil
}

// updateRecordIOLocked rewrites one record in place for a caller that holds ioMu; a
// missing or unreadable file is skipped (the in-memory row stays authoritative until the
// next start).
func (m *Manager) updateRecordIOLocked(id string, change func(*Record)) {
	m.updateRecordIfIOLocked(id, func(rec *Record) bool {
		change(rec)
		return true
	})
}

// updateRecordIfIOLocked is updateRecordIOLocked for a change that may decide, from the
// record as it is on disk, to leave it unchanged (change returns false: nothing is written).
func (m *Manager) updateRecordIfIOLocked(id string, change func(*Record) bool) {
	if m.ioClosed {
		return
	}
	data, err := os.ReadFile(m.recordPath(id))
	if err != nil {
		return
	}
	var rec Record
	if json.Unmarshal(data, &rec) != nil || rec.ID != id {
		return
	}
	if !change(&rec) {
		return
	}
	out, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := writeFileAtomic(m.recordPath(id), append(out, '\n')); err != nil {
		m.opts.Logger.Warn("async record not updated", "handle", id, "error", err)
	}
}

// removeRecordIOLocked removes one record for a caller that holds ioMu.
func (m *Manager) removeRecordIOLocked(id string) {
	if m.ioClosed {
		return
	}
	if err := os.Remove(m.recordPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.opts.Logger.Warn("async record not removed", "handle", id, "error", err)
	}
}

func rowIndex(r *row) indexRow {
	return indexRow{ID: r.id, Owner: r.owner, Kind: r.kind, State: r.state, OK: r.ok, Label: r.label,
		Started: r.started, Changed: r.changed, Unread: r.unread, Overflow: r.overflow}
}

// writeIndex schedules a rewrite of index.json: after index_flush_ms (one write for a
// burst of changes), or at once when the delay is 0 or the manager is closed.
func (m *Manager) writeIndex() {
	if m.cfg.IndexFlushMs <= 0 || m.indexClosed.Load() {
		m.flushIndexNow()
		return
	}
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	if m.flushTimer != nil {
		return
	}
	m.flushTimer = time.AfterFunc(time.Duration(m.cfg.IndexFlushMs)*time.Millisecond, func() {
		m.flushMu.Lock()
		m.flushTimer = nil
		m.flushMu.Unlock()
		if !m.indexClosed.Load() {
			m.flushIndexNow()
		}
	})
}

// flushIndexNow rewrites index.json from the current rows. Concurrent calls coalesce: while
// one goroutine writes, others only mark the index dirty and return, and the writer
// loops until no request is left. The file is a cache; a failed write is logged only.
func (m *Manager) flushIndexNow() {
	for {
		m.indexDirty.Store(true)
		if !m.indexMu.TryLock() {
			return
		}
		for m.indexDirty.Swap(false) {
			m.flushIndex()
		}
		m.indexMu.Unlock()
		if !m.indexDirty.Load() {
			return
		}
	}
}

func (m *Manager) flushIndex() {
	body, ok := m.indexBody()
	if !ok {
		return
	}
	m.ioMu.Lock()
	m.writeIndexBodyIOLocked(body)
	m.ioMu.Unlock()
}

// flushIndexIOLocked reads the rows and writes the index for a caller that holds ioMu
// (Close's final index), so no change can fall between the read and the write.
func (m *Manager) flushIndexIOLocked() {
	if body, ok := m.indexBody(); ok {
		m.writeIndexBodyIOLocked(body)
	}
}

// indexBody is the content of index.json for the current rows.
func (m *Manager) indexBody() ([]byte, bool) {
	m.mu.Lock()
	rows := make([]indexRow, 0, len(m.rows))
	for _, r := range m.rows {
		rows = append(rows, rowIndex(r))
	}
	m.mu.Unlock()
	sortIndexRows(rows)
	raw, err := json.Marshal(rows)
	if err != nil {
		return nil, false
	}
	body, err := json.Marshal(indexFileBody{Version: indexVersion, Sum: checksum(raw), Rows: raw})
	if err != nil {
		return nil, false
	}
	return append(body, '\n'), true
}

func (m *Manager) writeIndexBodyIOLocked(body []byte) {
	if m.ioClosed {
		return
	}
	if err := writeFileAtomic(filepath.Join(m.dir, indexFile), body); err != nil {
		m.opts.Logger.Warn("async index not saved; it is rebuilt at the next start", "error", err)
	}
}

func checksum(raw []byte) string {
	return strconv.FormatUint(uint64(crc32.ChecksumIEEE(raw)), 16)
}

// readIndex returns the rows of index.json, or ok=false when it is missing, unreadable
// or fails its checksum.
func (m *Manager) readIndex() ([]indexRow, bool) {
	data, err := os.ReadFile(filepath.Join(m.dir, indexFile))
	if err != nil {
		return nil, false
	}
	var body indexFileBody
	if json.Unmarshal(data, &body) != nil || body.Version != indexVersion || body.Sum != checksum(body.Rows) {
		return nil, false
	}
	var rows []indexRow
	if json.Unmarshal(body.Rows, &rows) != nil {
		return nil, false
	}
	return rows, true
}

// recordNames lists the ids that have a record file (names only, no file is opened) and
// removes temp files an earlier crash left behind.
func (m *Manager) recordNames() []string {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case strings.HasPrefix(name, tempPrefix):
			_ = os.Remove(filepath.Join(m.dir, name))
		case name == indexFile || entry.IsDir() || !strings.HasSuffix(name, ".json"):
		default:
			if id := strings.TrimSuffix(name, ".json"); idPattern.MatchString(id) {
				ids = append(ids, id)
			}
		}
	}
	return ids
}
