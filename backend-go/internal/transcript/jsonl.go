// Package transcript contains low-level JSON Lines persistence primitives.
// Higher-level session behavior and record shaping remain separate.
package transcript

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// DurabilityMode mirrors the persistence durability choices in the TypeScript
// backend. Process mode awaits writes; turn and strict modes also call Sync.
type DurabilityMode string

const (
	DurabilityProcess DurabilityMode = "process"
	DurabilityTurn    DurabilityMode = "turn"
	DurabilityStrict  DurabilityMode = "strict"
)

// Journal appends compact JSON object records to one JSONL file. Use one
// Journal instance per transcript; appends through that instance are serialized.
// Cross-process locking and response-level idempotency are not provided here.
type Journal struct {
	path       string
	durability DurabilityMode
	mu         sync.Mutex
	// generation counts in-process rewrites of the whole file (compaction,
	// reset, front compaction). Readers that cache parsed content use it to
	// drop that cache; it is only read and written while mu is held.
	generation uint64
	// index remembers the line hashes of the transcript for idempotent appends;
	// guarded by mu.
	index *lineIndex
	// dirSyncHook replaces syncDirectory in tests.
	dirSyncHook func(path string) error
	// appendWriteHook replaces the batch write of AppendAt in tests.
	appendWriteHook func(file *os.File, data []byte) error
}

func (j *Journal) syncDirectory(path string) error {
	if j.dirSyncHook != nil {
		return j.dirSyncHook(path)
	}
	return syncDirectory(path)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// NewJournal constructs a JSONL writer for path using the requested durability.
func NewJournal(path string, durability DurabilityMode) (*Journal, error) {
	if path == "" {
		return nil, errors.New("transcript path is required")
	}
	switch durability {
	case DurabilityProcess, DurabilityTurn, DurabilityStrict:
	default:
		return nil, fmt.Errorf("unsupported transcript durability mode %q", durability)
	}
	return &Journal{path: filepath.Clean(path), durability: durability}, nil
}

// ReplaceRecords atomically replaces the transcript with compact JSONL records.
func (j *Journal) ReplaceRecords(records ...[]byte) error {
	if j == nil {
		return errors.New("transcript journal is not initialized")
	}
	lines := make([][]byte, len(records))
	for index, record := range records {
		line, err := compactObject(record)
		if err != nil {
			return fmt.Errorf("transcript record %d: %w", index, err)
		}
		lines[index] = line
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(j.path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(j.path), ".messages-replace-*")
	if err != nil {
		return err
	}
	tempPath := temporary.Name()
	defer os.Remove(tempPath)
	for _, line := range lines {
		if err := writeAll(temporary, line); err != nil {
			_ = temporary.Close()
			return err
		}
		if err := writeAll(temporary, []byte{'\n'}); err != nil {
			_ = temporary.Close()
			return err
		}
	}
	if j.durability != DurabilityProcess {
		if err := temporary.Sync(); err != nil {
			_ = temporary.Close()
			return err
		}
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, j.path); err != nil {
		return err
	}
	j.generation++
	if j.durability != DurabilityProcess {
		if err := j.syncDirectory(filepath.Dir(j.path)); err != nil {
			return fmt.Errorf("sync transcript directory: %w", err)
		}
	}
	return nil
}

// byteSize is the current size of the transcript file, 0 when it does not exist.
func (j *Journal) byteSize() (int64, error) {
	info, err := os.Stat(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// Append validates and compacts all records before touching the transcript,
// repairs any incomplete trailing line, then appends each record with a newline.
// The records land together or not at all (see AppendAt). An empty append is a
// no-op and does not create the file.
func (j *Journal) Append(records ...[]byte) error {
	_, err := j.AppendAt(records...)
	return err
}

// AppendAt appends like Append and returns the byte offset each record starts
// at in the transcript, in order. The offsets are what the web-ui projection
// uses in its message ids. The batch is written in one write; if it fails, the
// file is cut back to its size before the batch, so a failed append never
// leaves a complete prefix of the batch behind for readers to count.
func (j *Journal) AppendAt(records ...[]byte) (offsets []int64, result error) {
	if j == nil {
		return nil, errors.New("transcript journal is not initialized")
	}
	if len(records) == 0 {
		return nil, nil
	}

	lines := make([][]byte, len(records))
	for index, record := range records {
		line, err := compactObject(record)
		if err != nil {
			return nil, fmt.Errorf("transcript record %d: %w", index, err)
		}
		lines[index] = line
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	file, err := os.OpenFile(j.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o666)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); result == nil && closeErr != nil {
			result = fmt.Errorf("close transcript: %w", closeErr)
		}
	}()

	if _, err := repairTrailingPartialLine(file); err != nil {
		return nil, fmt.Errorf("repair transcript tail: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat transcript: %w", err)
	}
	offset := info.Size()
	start := offset
	created := offset == 0
	offsets = make([]int64, len(lines))
	batchBytes := 0
	for _, line := range lines {
		batchBytes += len(line) + 1
	}
	batch := make([]byte, 0, batchBytes)
	for index, line := range lines {
		offsets[index] = offset
		batch = append(batch, line...)
		batch = append(batch, '\n')
		offset += int64(len(line)) + 1
	}
	write := writeAll
	if j.appendWriteHook != nil {
		write = j.appendWriteHook
	}
	if err := write(file, batch); err != nil {
		cause := fmt.Errorf("append transcript record: %w", err)
		if truncateErr := file.Truncate(start); truncateErr != nil {
			return nil, errors.Join(cause, fmt.Errorf("cut transcript back after a failed append: %w", truncateErr))
		}
		return nil, cause
	}
	if j.durability != DurabilityProcess {
		if err := file.Sync(); err != nil {
			return nil, fmt.Errorf("sync transcript: %w", err)
		}
		if created {
			if err := j.syncDirectory(filepath.Dir(j.path)); err != nil {
				return nil, fmt.Errorf("sync transcript directory: %w", err)
			}
		}
	}
	return offsets, nil
}

// RepairTail removes any bytes after the last complete newline-terminated
// record, matching the TypeScript store's startup/recovery behavior.
func (j *Journal) RepairTail() (removed int64, result error) {
	if j == nil {
		return 0, errors.New("transcript journal is not initialized")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	file, err := os.OpenFile(j.path, os.O_RDWR|os.O_APPEND, 0o666)
	if errors.Is(err, os.ErrNotExist) {
		// A missing transcript has no tail to repair, and is not created here.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("open transcript for tail repair: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); result == nil && closeErr != nil {
			result = fmt.Errorf("close transcript after tail repair: %w", closeErr)
		}
	}()
	removed, err = repairTrailingPartialLine(file)
	if err != nil {
		return 0, fmt.Errorf("repair transcript tail: %w", err)
	}
	return removed, nil
}

// ReadLines repairs the tail and visits complete physical lines while holding
// the journal lock, preventing reads from observing a partial append.
func (j *Journal) ReadLines(visit func([]byte) error) error {
	if visit == nil {
		return errors.New("transcript line visitor is required")
	}
	return j.ReadRecords(func(_ int64, line []byte) error { return visit(line) })
}

// ReadRecords visits LF-terminated JSONL records and their physical byte offsets.
// U+2028 and U+2029 inside JSON strings are record content, not delimiters.
func (j *Journal) ReadRecords(visit func(offset int64, line []byte) error) error {
	return j.ReadRecordsRange(0, 1<<63-1, visit)
}

// ReadRecordsRange reads complete records in the half-open byte interval
// [start, end). Bounds must fall on record boundaries. A range read repairs an
// incomplete final line before observing the transcript.
func (j *Journal) ReadRecordsRange(start, end int64, visit func(offset int64, line []byte) error) (result error) {
	if j == nil {
		return errors.New("transcript journal is not initialized")
	}
	if visit == nil {
		return errors.New("transcript record visitor is required")
	}
	if start < 0 || end < start {
		return errors.New("transcript record range is invalid")
	}
	return j.withRepairedFile(func(file *os.File, info os.FileInfo) error {
		limit := end
		if limit > info.Size() {
			limit = info.Size()
		}
		if start > limit {
			return nil
		}
		if start > 0 {
			var previous [1]byte
			if n, readErr := file.ReadAt(previous[:], start-1); readErr != nil || n != 1 || previous[0] != '\n' {
				return errors.New("transcript range start is not a record boundary")
			}
		}
		return visitRecords(file, start, limit, visit)
	})
}

// ErrTranscriptChanged reports that the transcript was replaced or rewritten while a
// snapshot read ran, so the records visited by that read must be discarded.
var ErrTranscriptChanged = errors.New("transcript changed during snapshot read")

// ReadRecordsSnapshot visits the complete records in [start, end) of the transcript as it
// was when the call took the journal lock. The lock is held only to repair the tail and
// to take the snapshot, so appends made during the read wait for at most that repair. The
// read stops at the last newline before the snapshot end, so it never visits a torn record.
// If the file was replaced or rewritten before the read finished, the records visited are
// stale and ErrTranscriptChanged is returned; the caller must discard them. Use this only
// when the visitor can start over.
func (j *Journal) ReadRecordsSnapshot(start, end int64, visit func(offset int64, line []byte) error) (result error) {
	if j == nil {
		return errors.New("transcript journal is not initialized")
	}
	if visit == nil {
		return errors.New("transcript record visitor is required")
	}
	if start < 0 || end < start {
		return errors.New("transcript record range is invalid")
	}
	snapshotEnd, snapshot, identity, generation, err := j.takeSnapshot()
	if err != nil {
		return err
	}
	if snapshot == nil {
		return nil
	}
	defer func() {
		if closeErr := snapshot.Close(); result == nil && closeErr != nil {
			result = fmt.Errorf("close transcript after snapshot read: %w", closeErr)
		}
	}()
	limit := end
	if limit > snapshotEnd {
		limit = snapshotEnd
	}
	if start > limit {
		return nil
	}
	if start > 0 {
		var previous [1]byte
		if n, readErr := snapshot.ReadAt(previous[:], start-1); readErr != nil || n != 1 || previous[0] != '\n' {
			return errors.New("transcript range start is not a record boundary")
		}
	}
	if err := visitRecords(snapshot, start, limit, visit); err != nil {
		return err
	}
	return j.checkSnapshot(identity, generation)
}

// takeSnapshot repairs the tail under the lock, then returns a read-only handle together
// with the file identity, size and generation the snapshot was taken at. The handle is nil
// when the transcript does not exist.
func (j *Journal) takeSnapshot() (size int64, file *os.File, identity os.FileInfo, generation uint64, result error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	writable, err := os.OpenFile(j.path, os.O_RDWR|os.O_APPEND, 0o666)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil, nil, j.generation, nil
	}
	if err != nil {
		return 0, nil, nil, 0, fmt.Errorf("open transcript for snapshot: %w", err)
	}
	defer func() {
		if closeErr := writable.Close(); result == nil && closeErr != nil {
			result = fmt.Errorf("close transcript after snapshot repair: %w", closeErr)
		}
	}()
	if _, err := repairTrailingPartialLine(writable); err != nil {
		return 0, nil, nil, 0, fmt.Errorf("repair transcript tail before snapshot: %w", err)
	}
	info, err := writable.Stat()
	if err != nil {
		return 0, nil, nil, 0, fmt.Errorf("stat transcript before snapshot: %w", err)
	}
	reader, err := os.Open(j.path)
	if err != nil {
		return 0, nil, nil, 0, fmt.Errorf("open transcript snapshot for reading: %w", err)
	}
	snapshotInfo, err := reader.Stat()
	if err != nil || !os.SameFile(info, snapshotInfo) {
		_ = reader.Close()
		if err == nil {
			err = ErrTranscriptChanged
		}
		return 0, nil, nil, 0, fmt.Errorf("stat transcript snapshot: %w", err)
	}
	return info.Size(), reader, info, j.generation, nil
}

// checkSnapshot reports ErrTranscriptChanged when the transcript was rewritten, replaced or
// removed since takeSnapshot. Appends only write past the snapshot end, so they do not
// invalidate it.
func (j *Journal) checkSnapshot(identity os.FileInfo, generation uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.generation != generation {
		return ErrTranscriptChanged
	}
	current, err := os.Stat(j.path)
	if err != nil || !os.SameFile(identity, current) {
		return ErrTranscriptChanged
	}
	return nil
}

// withRepairedFile runs fn while holding the journal lock, on the transcript
// opened for reading after any incomplete final record was removed. info
// describes the repaired file. fn must not call other Journal methods.
func (j *Journal) withRepairedFile(fn func(file *os.File, info os.FileInfo) error) (result error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	file, err := os.OpenFile(j.path, os.O_RDWR|os.O_APPEND, 0o666)
	if errors.Is(err, os.ErrNotExist) {
		// A missing transcript is read as an empty one, and is not created here.
		return nil
	}
	if err != nil {
		return fmt.Errorf("open transcript for reading: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); result == nil && closeErr != nil {
			result = fmt.Errorf("close transcript after reading: %w", closeErr)
		}
	}()
	if _, err := repairTrailingPartialLine(file); err != nil {
		return fmt.Errorf("repair transcript tail before reading: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat transcript before reading: %w", err)
	}
	return fn(file, info)
}

// visitRecords visits the LF-terminated records in [start, limit) of an open
// transcript whose tail is already repaired. start must be a record boundary.
func visitRecords(file *os.File, start, limit int64, visit func(offset int64, line []byte) error) error {
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return fmt.Errorf("seek transcript: %w", err)
	}
	reader := bufio.NewReader(file)
	offset := start
	for offset < limit {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			recordOffset := offset
			offset += int64(len(line))
			if offset > limit {
				return errors.New("transcript range end is not a record boundary")
			}
			line = line[:len(line)-1]
			if len(line) > 0 {
				if err := visit(recordOffset, line); err != nil {
					return err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read transcript line: %w", readErr)
		}
	}
	return nil
}

// ReadCompleteRecordsRange visits LF-terminated records in [start,end) without
// repairing an incomplete trailing record. It returns the first byte offset not
// fully consumed, allowing incremental indexes to resume after a concurrent
// writer finishes its append.
func (j *Journal) ReadCompleteRecordsRange(start, end int64, visit func(offset int64, line []byte) error) (scannedEnd int64, result error) {
	if j == nil {
		return start, errors.New("transcript journal is not initialized")
	}
	if visit == nil {
		return start, errors.New("transcript record visitor is required")
	}
	if start < 0 || end < start {
		return start, errors.New("transcript record range is invalid")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	file, err := os.Open(j.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return start, nil
		}
		return start, fmt.Errorf("open transcript for indexed reading: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); result == nil && closeErr != nil {
			result = fmt.Errorf("close transcript after indexed reading: %w", closeErr)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return start, fmt.Errorf("stat transcript for indexed reading: %w", err)
	}
	limit := end
	if limit > info.Size() {
		limit = info.Size()
	}
	if start > limit {
		return start, nil
	}
	if start > 0 {
		var previous [1]byte
		if n, readErr := file.ReadAt(previous[:], start-1); readErr != nil || n != 1 || previous[0] != '\n' {
			return start, errors.New("transcript range start is not a record boundary")
		}
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return start, fmt.Errorf("seek transcript for indexed reading: %w", err)
	}
	reader := bufio.NewReader(file)
	offset := start
	for offset < limit {
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return offset, fmt.Errorf("read transcript record: %w", readErr)
		}
		if len(line) == 0 || line[len(line)-1] != '\n' {
			break
		}
		recordOffset := offset
		offset += int64(len(line))
		if offset > limit {
			return recordOffset, errors.New("transcript range end is not a record boundary")
		}
		line = line[:len(line)-1]
		if len(line) > 0 {
			if err := visit(recordOffset, line); err != nil {
				return recordOffset, err
			}
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return offset, fmt.Errorf("read transcript record: %w", readErr)
		}
	}
	return offset, nil
}

func (j *Journal) containsHashText(hashText string) (bool, error) {
	if j == nil {
		return false, errors.New("transcript journal is not initialized")
	}
	rawHash, err := hex.DecodeString(hashText)
	if err != nil || len(rawHash) != sha256.Size {
		return false, errors.New("invalid canonical transcript hash")
	}
	var target [sha256.Size]byte
	copy(target[:], rawHash)
	j.mu.Lock()
	defer j.mu.Unlock()
	file, err := os.Open(j.path)
	if err != nil {
		return false, fmt.Errorf("open transcript to verify committed line: %w", err)
	}
	defer file.Close()
	return containsLineHash(file, target)
}

func (j *Journal) appendFileIfAbsent(sourcePath string) (inserted bool, result error) {
	if j == nil {
		return false, errors.New("transcript journal is not initialized")
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return false, fmt.Errorf("open rendered transcript line: %w", err)
	}
	defer func() {
		if closeErr := source.Close(); result == nil && closeErr != nil {
			result = fmt.Errorf("close rendered transcript line: %w", closeErr)
		}
	}()
	info, err := source.Stat()
	if err != nil {
		return false, fmt.Errorf("stat rendered transcript line: %w", err)
	}
	if info.Size() == 0 {
		return false, errors.New("rendered transcript line is empty")
	}
	var lastByte [1]byte
	if _, err := source.ReadAt(lastByte[:], info.Size()-1); err != nil {
		return false, fmt.Errorf("read rendered transcript line terminator: %w", err)
	}
	if lastByte[0] != '\n' {
		return false, errors.New("rendered transcript line is not newline-terminated")
	}
	digest := sha256.New()
	if _, err := io.CopyBuffer(digest, source, make([]byte, 64*1024)); err != nil {
		return false, fmt.Errorf("hash rendered transcript line: %w", err)
	}
	var targetHash [sha256.Size]byte
	copy(targetHash[:], digest.Sum(nil))
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("rewind rendered transcript line: %w", err)
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	transcript, err := os.OpenFile(j.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o666)
	if err != nil {
		return false, fmt.Errorf("open transcript: %w", err)
	}
	defer func() {
		if closeErr := transcript.Close(); result == nil && closeErr != nil {
			result = fmt.Errorf("close transcript: %w", closeErr)
		}
	}()
	if _, err := repairTrailingPartialLine(transcript); err != nil {
		return false, fmt.Errorf("repair transcript tail: %w", err)
	}
	present, err := j.containsLine(transcript, targetHash)
	if err != nil {
		return false, fmt.Errorf("check transcript duplicate: %w", err)
	}
	if present {
		return false, nil
	}
	created := false
	if j.durability != DurabilityProcess {
		transcriptInfo, err := transcript.Stat()
		if err != nil {
			return false, fmt.Errorf("stat transcript: %w", err)
		}
		created = transcriptInfo.Size() == 0
	}
	buffer := make([]byte, 64*1024)
	for {
		readCount, readErr := source.Read(buffer)
		if readCount > 0 {
			if err := writeAll(transcript, buffer[:readCount]); err != nil {
				return false, fmt.Errorf("append rendered transcript line: %w", err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return false, fmt.Errorf("read rendered transcript line: %w", readErr)
		}
	}
	if j.durability != DurabilityProcess {
		if err := transcript.Sync(); err != nil {
			return false, fmt.Errorf("sync transcript: %w", err)
		}
		if created {
			if err := j.syncDirectory(filepath.Dir(j.path)); err != nil {
				return false, fmt.Errorf("sync transcript directory: %w", err)
			}
		}
	}
	return true, nil
}

func containsLineHash(file *os.File, target [sha256.Size]byte) (bool, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	reader := bufio.NewReader(file)
	lineHash := sha256.New()
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(chunk) > 0 {
			if _, writeErr := lineHash.Write(chunk); writeErr != nil {
				return false, writeErr
			}
		}
		if err == nil {
			if bytes.Equal(lineHash.Sum(nil), target[:]) {
				return true, nil
			}
			lineHash.Reset()
			continue
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
}

func compactObject(record []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(record)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("record must be a JSON object")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return nil, fmt.Errorf("record must be valid JSON: %w", err)
	}
	return compact.Bytes(), nil
}

func repairTrailingPartialLine(file *os.File) (int64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	originalSize := info.Size()
	if originalSize == 0 {
		return 0, nil
	}

	const scanChunkBytes = 64 * 1024
	var scanBuffer [scanChunkBytes]byte
	cursor := originalSize
	keepBytes := int64(0)
	for cursor > 0 {
		chunkSize := int64(scanChunkBytes)
		if cursor < chunkSize {
			chunkSize = cursor
		}
		start := cursor - chunkSize
		buffer := scanBuffer[:int(chunkSize)]
		n, readErr := file.ReadAt(buffer, start)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return 0, readErr
		}
		if n != len(buffer) {
			return 0, io.ErrUnexpectedEOF
		}
		if newline := bytes.LastIndexByte(buffer, '\n'); newline >= 0 {
			keepBytes = start + int64(newline) + 1
			break
		}
		cursor = start
	}

	if keepBytes != originalSize {
		if err := file.Truncate(keepBytes); err != nil {
			return 0, err
		}
	}
	return originalSize - keepBytes, nil
}

func writeAll(file *os.File, data []byte) error {
	for len(data) > 0 {
		written, err := file.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
