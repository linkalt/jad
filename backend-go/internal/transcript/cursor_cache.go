package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	defaultCursorCacheCheckpointBytes int64 = 1 << 20
	maxCursorCacheCheckpointBytes     int64 = 64 << 20
	cursorCacheSchemaVersion                = 2
	maxSafeCursorInteger              int64 = 1<<53 - 1
)

// CursorRun is one completed provider response in transcript byte order.
type CursorRun struct {
	Offset int64 `json:"offset"`
	Turn   int64 `json:"turn"`
	Step   int64 `json:"step"`
}

// CursorIndex is a stable snapshot of completed response offsets at one file
// size. Runs are copied so callers can retain the index across later appends.
type CursorIndex struct {
	Size int64
	Runs []CursorRun
}

type cursorCacheState struct {
	offset           int64
	modifiedAtNS     int64
	checkpointStart  int64
	checkpointSHA256 string
	persistedOffset  int64
	runs             []CursorRun
}

type cursorCacheFile struct {
	SchemaVersion int                   `json:"schema_version"`
	Transcript    cursorCacheCheckpoint `json:"transcript"`
	Runs          []CursorRun           `json:"runs"`
}

type cursorCacheCheckpoint struct {
	Offset           int64  `json:"offset"`
	ModifiedAtNS     int64  `json:"modified_at_ns"`
	CheckpointStart  int64  `json:"checkpoint_start"`
	CheckpointSHA256 string `json:"checkpoint_sha256"`
}

// ReadCursorIndex returns response.completed offsets, using an append-aware
// in-memory and file-backed cache. A malformed or stale cache is discarded and
// rebuilt from complete transcript records.
func (s *TurnStore) ReadCursorIndex() (CursorIndex, error) {
	if s == nil || s.journal == nil {
		return CursorIndex{}, errors.New("transcript turn store is not initialized")
	}
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()

	info, err := os.Stat(s.journal.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CursorIndex{Runs: []CursorRun{}}, nil
		}
		return CursorIndex{}, fmt.Errorf("stat transcript for cursor index: %w", err)
	}
	currentSize := info.Size()
	currentModifiedAt := info.ModTime().UnixNano()

	state := s.cursorIndex
	if state == nil {
		state = s.readCursorCacheFile()
	}
	if state != nil && s.cursorCacheMatches(state, currentSize, currentModifiedAt) {
		if currentSize == state.offset {
			s.cursorIndex = state
			return CursorIndex{Size: currentSize, Runs: cloneCursorRuns(state.runs)}, nil
		}
		runs := cloneCursorRuns(state.runs)
		scannedEnd, err := s.scanCursorRuns(state.offset, currentSize, &runs)
		if err != nil {
			return CursorIndex{}, err
		}
		info, err = os.Stat(s.journal.path)
		if err != nil {
			return CursorIndex{}, fmt.Errorf("stat transcript after cursor scan: %w", err)
		}
		if scannedEnd > info.Size() {
			return s.rebuildCursorIndex(info)
		}
		updated, err := s.makeCursorCacheState(runs, scannedEnd, info.ModTime().UnixNano())
		if err != nil {
			return CursorIndex{}, err
		}
		updated.persistedOffset = state.persistedOffset
		if updated.offset-updated.persistedOffset >= s.options.CursorCacheCheckpointBytes {
			if err := s.persistCursorCache(updated); err != nil {
				return CursorIndex{}, err
			}
			updated.persistedOffset = updated.offset
		}
		s.cursorIndex = updated
		return CursorIndex{Size: info.Size(), Runs: cloneCursorRuns(updated.runs)}, nil
	}
	return s.rebuildCursorIndex(info)
}

func (s *TurnStore) rebuildCursorIndex(info os.FileInfo) (CursorIndex, error) {
	runs := make([]CursorRun, 0)
	scannedEnd, err := s.scanCursorRuns(0, info.Size(), &runs)
	if err != nil {
		return CursorIndex{}, err
	}
	current, err := os.Stat(s.journal.path)
	if err != nil {
		return CursorIndex{}, fmt.Errorf("stat transcript after cursor-index rebuild: %w", err)
	}
	if scannedEnd > current.Size() {
		return CursorIndex{}, errors.New("transcript shrank while rebuilding cursor index")
	}
	state, err := s.makeCursorCacheState(runs, scannedEnd, current.ModTime().UnixNano())
	if err != nil {
		return CursorIndex{}, err
	}
	state.persistedOffset = state.offset
	if err := s.persistCursorCache(state); err != nil {
		return CursorIndex{}, err
	}
	s.cursorIndex = state
	return CursorIndex{Size: current.Size(), Runs: cloneCursorRuns(state.runs)}, nil
}

func (s *TurnStore) scanCursorRuns(start, end int64, runs *[]CursorRun) (int64, error) {
	return s.journal.ReadCompleteRecordsRange(start, end, func(offset int64, line []byte) error {
		if !json.Valid(line) {
			return nil
		}
		var record struct {
			Type        string `json:"type"`
			SessionTurn int64  `json:"session_turn"`
			SessionStep int64  `json:"session_step"`
		}
		if err := json.Unmarshal(line, &record); err != nil || record.Type != "response.completed" {
			return nil
		}
		if record.SessionTurn < 1 || record.SessionStep < 1 || record.SessionTurn > maxSafeCursorInteger || record.SessionStep > maxSafeCursorInteger {
			return nil
		}
		*runs = append(*runs, CursorRun{Offset: offset, Turn: record.SessionTurn, Step: record.SessionStep})
		return nil
	})
}

func (s *TurnStore) cursorCacheMatches(state *cursorCacheState, size, modifiedAtNS int64) bool {
	if state == nil || size < state.offset || state.offset < 0 || state.checkpointStart < 0 || state.checkpointStart > state.offset {
		return false
	}
	if size == state.offset && modifiedAtNS != state.modifiedAtNS {
		return false
	}
	hash, err := hashTranscriptRange(s.journal.path, state.checkpointStart, state.offset)
	return err == nil && hash == state.checkpointSHA256
}

func (s *TurnStore) makeCursorCacheState(runs []CursorRun, offset, modifiedAtNS int64) (*cursorCacheState, error) {
	checkpointStart := offset - s.options.CursorCacheCheckpointBytes
	if checkpointStart < 0 {
		checkpointStart = 0
	}
	checkpointSHA256, err := hashTranscriptRange(s.journal.path, checkpointStart, offset)
	if err != nil {
		return nil, fmt.Errorf("hash transcript cursor checkpoint: %w", err)
	}
	return &cursorCacheState{
		offset:           offset,
		modifiedAtNS:     modifiedAtNS,
		checkpointStart:  checkpointStart,
		checkpointSHA256: checkpointSHA256,
		runs:             cloneCursorRuns(runs),
	}, nil
}

func (s *TurnStore) readCursorCacheFile() *cursorCacheState {
	path := s.options.CursorCachePath
	if path == "" {
		return nil
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cached cursorCacheFile
	if err := json.Unmarshal(contents, &cached); err != nil || cached.SchemaVersion != cursorCacheSchemaVersion || cached.Runs == nil {
		return nil
	}
	checkpoint := cached.Transcript
	if checkpoint.Offset < 0 || checkpoint.CheckpointStart < 0 || checkpoint.CheckpointStart > checkpoint.Offset {
		return nil
	}
	decodedHash, err := hex.DecodeString(checkpoint.CheckpointSHA256)
	if err != nil || len(decodedHash) != sha256.Size {
		return nil
	}
	previousOffset := int64(-1)
	for _, run := range cached.Runs {
		if run.Offset < 0 || run.Offset < previousOffset || run.Offset >= checkpoint.Offset || run.Turn < 1 || run.Step < 1 || run.Turn > maxSafeCursorInteger || run.Step > maxSafeCursorInteger {
			return nil
		}
		previousOffset = run.Offset
	}
	return &cursorCacheState{
		offset:           checkpoint.Offset,
		modifiedAtNS:     checkpoint.ModifiedAtNS,
		checkpointStart:  checkpoint.CheckpointStart,
		checkpointSHA256: checkpoint.CheckpointSHA256,
		persistedOffset:  checkpoint.Offset,
		runs:             append([]CursorRun(nil), cached.Runs...),
	}
}

func (s *TurnStore) persistCursorCache(state *cursorCacheState) error {
	path := s.options.CursorCachePath
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create cursor-cache directory: %w", err)
	}
	encoded, err := json.Marshal(cursorCacheFile{
		SchemaVersion: cursorCacheSchemaVersion,
		Transcript: cursorCacheCheckpoint{
			Offset:           state.offset,
			ModifiedAtNS:     state.modifiedAtNS,
			CheckpointStart:  state.checkpointStart,
			CheckpointSHA256: state.checkpointSHA256,
		},
		Runs: state.runs,
	})
	if err != nil {
		return fmt.Errorf("encode cursor cache: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".cursor-cache-*")
	if err != nil {
		return fmt.Errorf("create cursor-cache temporary file: %w", err)
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)
	if err := writeAll(file, encoded); err != nil {
		_ = file.Close()
		return fmt.Errorf("write cursor cache: %w", err)
	}
	if s.journal.durability != DurabilityProcess {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return fmt.Errorf("sync cursor cache: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close cursor cache: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace cursor cache: %w", err)
	}
	if s.journal.durability != DurabilityProcess {
		directory, err := os.Open(filepath.Dir(path))
		if err != nil {
			return fmt.Errorf("open cursor-cache directory for sync: %w", err)
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil {
			return fmt.Errorf("sync cursor-cache directory: %w", syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close cursor-cache directory: %w", closeErr)
		}
	}
	return nil
}

func hashTranscriptRange(path string, start, end int64) (string, error) {
	if start < 0 || end < start {
		return "", errors.New("transcript hash range is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	length := end - start
	if length > 0 {
		written, err := io.CopyN(hash, io.NewSectionReader(file, start, length), length)
		if err != nil {
			return "", err
		}
		if written != length {
			return "", io.ErrUnexpectedEOF
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *TurnStore) invalidateCursorCache() {
	if s == nil {
		return
	}
	s.cursorMu.Lock()
	s.cursorIndex = nil
	s.cursorMu.Unlock()
}

func cloneCursorRuns(runs []CursorRun) []CursorRun {
	return append([]CursorRun(nil), runs...)
}
