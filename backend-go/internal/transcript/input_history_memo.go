package transcript

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// historyCheckpointBytes is how much of the transcript tail is hashed to prove
// that the bytes already expanded are still the bytes on disk. It matches the
// TypeScript store's history checkpoint.
const historyCheckpointBytes int64 = 64 * 1024

// InputHistory reads request history for one owner (a provider run) and keeps
// the expanded transcript items between reads, so each read expands only the
// bytes appended since the previous one. The retained items live exactly as
// long as the handle, which bounds memory without a configuration value.
//
// A handle is not safe for concurrent use. The slices it returns share their
// item bytes with the handle across reads, so callers must treat them as
// read-only.
type InputHistory struct {
	store      *TurnStore
	memo       transcriptMemo
	compaction compactionMemo
}

// NewInputHistory returns a history reader that reuses work across reads.
func (s *TurnStore) NewInputHistory() *InputHistory {
	return &InputHistory{store: s}
}

// Read returns the same items as TurnStore.ReadInputItems.
func (h *InputHistory) Read() ([][]byte, error) {
	if h == nil || h.store == nil || h.store.journal == nil {
		return nil, errors.New("turn store is not initialized")
	}
	return h.store.readInputItems(h)
}

// Release drops the retained items.
func (h *InputHistory) Release() {
	if h != nil {
		h.memo = transcriptMemo{}
		h.compaction = compactionMemo{}
	}
}

// transcriptMemo is the expansion of messages.jsonl[0:offset].
type transcriptMemo struct {
	valid            bool
	generation       uint64
	file             os.FileInfo
	modifiedAt       time.Time
	offset           int64
	checkpointStart  int64
	checkpointSHA256 string
	items            [][]byte
}

// readTranscriptItemsMemo expands the transcript, reusing memo for the prefix
// that is provably unchanged. The returned outer slice is a fresh copy; the
// item bytes are shared with the memo.
func (s *TurnStore) readTranscriptItemsMemo(memo *transcriptMemo) ([][]byte, error) {
	var result [][]byte
	err := s.journal.withRepairedFile(func(file *os.File, info os.FileInfo) error {
		if !memo.matches(s.journal.generation, file, info) {
			*memo = transcriptMemo{}
		}
		size := info.Size()
		var added [][]byte
		if size > memo.offset {
			visitErr := visitRecords(file, memo.offset, size, func(_ int64, line []byte) error {
				expanded, err := expandTranscriptRecord(line)
				if err != nil {
					return fmt.Errorf("expand transcript record: %w", err)
				}
				added = append(added, expanded...)
				return nil
			})
			if visitErr != nil {
				return visitErr
			}
		}
		checkpointStart := size - historyCheckpointBytes
		if checkpointStart < 0 {
			checkpointStart = 0
		}
		checkpoint, err := hashSection(file, checkpointStart, size)
		if err != nil {
			return fmt.Errorf("hash transcript history checkpoint: %w", err)
		}
		memo.items = append(memo.items, added...)
		memo.valid = true
		memo.generation = s.journal.generation
		memo.file = info
		memo.modifiedAt = info.ModTime()
		memo.offset = size
		memo.checkpointStart = checkpointStart
		memo.checkpointSHA256 = checkpoint
		result = make([][]byte, len(memo.items))
		copy(result, memo.items)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// matches reports whether the memo still describes the start of the file. The
// journal generation catches in-process rewrites (compaction, reset, front
// compaction); the file identity, size, modification time and a tail hash catch
// a transcript replaced or edited from outside this handle.
func (m *transcriptMemo) matches(generation uint64, file *os.File, info os.FileInfo) bool {
	if !m.valid || m.generation != generation || !os.SameFile(m.file, info) {
		return false
	}
	size := info.Size()
	if size < m.offset || m.checkpointStart < 0 || m.checkpointStart > m.offset {
		return false
	}
	if size == m.offset && !info.ModTime().Equal(m.modifiedAt) {
		return false
	}
	if m.offset > 0 {
		var previous [1]byte
		if n, err := file.ReadAt(previous[:], m.offset-1); err != nil || n != 1 || previous[0] != '\n' {
			return false
		}
	}
	hash, err := hashSection(file, m.checkpointStart, m.offset)
	return err == nil && hash == m.checkpointSHA256
}
