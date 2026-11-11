package transcript

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var frontCompactionMessageIDPattern = regexp.MustCompile(`^session-user-o(0|[1-9][0-9]*)$`)

type FrontCompactionResult struct {
	DeleteMode  string `json:"delete_mode"`
	ArchiveName string `json:"archive_name,omitempty"`
}

type FrontCompactionConflict struct {
	Message string
}

func (e *FrontCompactionConflict) Error() string { return e.Message }

// FrontCompactFromMessage removes the active transcript suffix beginning at a
// user message. Recoverable deletion archives the exact removed bytes beside
// the transcript; permanent deletion does not create an archive.
func (s *TurnStore) FrontCompactFromMessage(messageID, deleteMode string) (FrontCompactionResult, error) {
	return s.FrontCompactFromMessageWith(messageID, deleteMode, nil)
}

// FrontCompactFromMessageWith is FrontCompactFromMessage with a hook that sees the transcript
// that will replace the active one before anything is archived or published (see
// Journal.ReplaceWithPrefixWith). An error from before leaves the transcript as it was.
func (s *TurnStore) FrontCompactFromMessageWith(messageID, deleteMode string, before func(Replacement) error) (FrontCompactionResult, error) {
	result := FrontCompactionResult{DeleteMode: deleteMode}
	if s == nil || s.journal == nil {
		return result, errors.New("turn store is not initialized")
	}
	if deleteMode != "permanent" && deleteMode != "recoverable" {
		return result, &FrontCompactionConflict{Message: "invalid front-compaction delete mode"}
	}
	match := frontCompactionMessageIDPattern.FindStringSubmatch(messageID)
	if match == nil {
		return result, &FrontCompactionConflict{Message: "front compaction only accepts an active transcript user message"}
	}
	requestedOffset, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || requestedOffset < 0 || requestedOffset > maxJavaScriptSafeInteger {
		return result, &FrontCompactionConflict{Message: "invalid front-compaction message offset"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recovering {
		return result, errors.New("turn store recovery is in progress")
	}
	if len(s.activeTurns) > 0 {
		return result, &FrontCompactionConflict{Message: "cannot front compact while response turns are active"}
	}

	var size int64
	var targetFound, targetInvalid bool
	var boundary int64
	activeBatchID := ""
	var activeBatchNext int64
	activeBatchOffsets := make(map[int64]int64)
	if err := s.journal.ReadRecords(func(offset int64, line []byte) error {
		size = offset + int64(len(line)) + 1
		fields, parseErr := objectFields(line)
		if parseErr != nil {
			activeBatchID = ""
			activeBatchNext = 0
			activeBatchOffsets = make(map[int64]int64)
			if offset == requestedOffset {
				targetFound = true
				targetInvalid = true
			}
			return nil
		}
		isUser := isTranscriptUserMessage(fields)
		batchIDRaw, hasBatchID := fieldValue(fields, SessionInputBatchIDField)
		batchID, isBatchID := stringValue(batchIDRaw)
		batchIndexRaw, hasBatchIndex := fieldValue(fields, SessionInputBatchIndexField)
		batchIndex, batchIndexValid := transcriptBatchIndex(batchIndexRaw)
		inBatch := isUser && hasBatchID && isBatchID && hasBatchIndex && batchIndexValid
		if inBatch {
			if batchID == activeBatchID && batchIndex == activeBatchNext {
				activeBatchOffsets[batchIndex] = offset
			} else {
				activeBatchID = batchID
				activeBatchOffsets = map[int64]int64{batchIndex: offset}
			}
			activeBatchNext = batchIndex + 1
		} else {
			activeBatchID = ""
			activeBatchNext = 0
			activeBatchOffsets = make(map[int64]int64)
		}
		if offset != requestedOffset {
			return nil
		}
		targetFound = true
		if !isUser {
			targetInvalid = true
			return nil
		}
		boundary = offset
		if inBatch && batchIndex > 0 {
			for expectedIndex := batchIndex - 1; expectedIndex >= 0; expectedIndex-- {
				previousOffset, ok := activeBatchOffsets[expectedIndex]
				if !ok {
					break
				}
				boundary = previousOffset
			}
		}
		return nil
	}); err != nil {
		return result, err
	}
	if requestedOffset >= size {
		return result, &FrontCompactionConflict{Message: "front-compaction message is not in the active transcript"}
	}
	if !targetFound {
		return result, &FrontCompactionConflict{Message: "front-compaction message offset is not a record boundary"}
	}
	if targetInvalid {
		return result, &FrontCompactionConflict{Message: "front compaction must start at an active user message"}
	}

	archivePath := ""
	if deleteMode == "recoverable" {
		archiveName, err := newDeletedHistoryArchiveName()
		if err != nil {
			return result, fmt.Errorf("create front-compaction archive name: %w", err)
		}
		deletedHistoryDir := s.journal.path + ".deleted-history"
		if err := os.MkdirAll(deletedHistoryDir, 0o755); err != nil {
			return result, fmt.Errorf("create deleted-history directory: %w", err)
		}
		archivePath = filepath.Join(deletedHistoryDir, archiveName)
		result.ArchiveName = archiveName
	}
	if err := s.journal.ReplaceWithPrefixWith(before, boundary, size, archivePath); err != nil {
		return FrontCompactionResult{}, fmt.Errorf("front compact transcript: %w", err)
	}
	if err := s.removeDerivedFiles(false, true); err != nil {
		return FrontCompactionResult{}, err
	}
	if err := s.clampStateContext(); err != nil {
		return FrontCompactionResult{}, err
	}
	return result, nil
}

func isTranscriptUserMessage(fields []jsonField) bool {
	typeRaw, hasType := fieldValue(fields, "type")
	recordType, typeOK := stringValue(typeRaw)
	roleRaw, hasRole := fieldValue(fields, "role")
	role, roleOK := stringValue(roleRaw)
	return hasType && typeOK && recordType == "message" && hasRole && roleOK && role == "user"
}

func transcriptBatchIndex(raw []byte) (int64, bool) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return 0, false
	}
	var index int64
	if err := json.Unmarshal(raw, &index); err != nil || index < 0 || index > maxJavaScriptSafeInteger {
		return 0, false
	}
	return index, true
}

func newDeletedHistoryArchiveName() (string, error) {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	timestamp = strings.NewReplacer("-", "", ":", "", ".", "").Replace(timestamp)
	return fmt.Sprintf("messages-%s-%s.jsonl", timestamp, hex.EncodeToString(random[:])), nil
}

// ReplaceWithPrefix keeps the transcript bytes before boundary and optionally
// archives the removed suffix before publishing the replacement file.
func (j *Journal) ReplaceWithPrefix(boundary, expectedSize int64, archivePath string) error {
	return j.ReplaceWithPrefixWith(nil, boundary, expectedSize, archivePath)
}

// ReplaceWithPrefixWith is ReplaceWithPrefix with a hook: before is called with the prefix that
// will be published, while the journal is locked and before the archive is written, so an error
// from it leaves both the transcript and the archive directory untouched.
func (j *Journal) ReplaceWithPrefixWith(before func(Replacement) error, boundary, expectedSize int64, archivePath string) error {
	if j == nil {
		return errors.New("transcript journal is not initialized")
	}
	if boundary < 0 || expectedSize < boundary {
		return errors.New("front-compaction transcript range is invalid")
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	source, err := os.Open(j.path)
	if err != nil {
		return fmt.Errorf("open transcript for front compaction: %w", err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat transcript for front compaction: %w", err)
	}
	if info.Size() != expectedSize {
		return &FrontCompactionConflict{Message: "transcript changed during front compaction"}
	}
	if boundary > 0 {
		var previous [1]byte
		if n, readErr := source.ReadAt(previous[:], boundary-1); readErr != nil || n != 1 || previous[0] != '\n' {
			return errors.New("front-compaction offset is not a record boundary")
		}
	}

	if before != nil {
		digest := sha256.New()
		if _, err := io.Copy(digest, io.NewSectionReader(source, 0, boundary)); err != nil {
			return fmt.Errorf("hash front-compaction transcript: %w", err)
		}
		if err := before(Replacement{PreviousSize: expectedSize, Size: boundary, SHA256: hex.EncodeToString(digest.Sum(nil))}); err != nil {
			return err
		}
	}

	if archivePath != "" {
		if _, err := os.Lstat(archivePath); err == nil {
			return errors.New("front-compaction archive already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check front-compaction archive: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
			return fmt.Errorf("create front-compaction archive directory: %w", err)
		}
		archiveTemp, err := os.CreateTemp(filepath.Dir(archivePath), ".front-compact-archive-*")
		if err != nil {
			return fmt.Errorf("create front-compaction archive: %w", err)
		}
		archiveTempPath := archiveTemp.Name()
		defer os.Remove(archiveTempPath)
		length := expectedSize - boundary
		written, copyErr := io.Copy(archiveTemp, io.NewSectionReader(source, boundary, length))
		if copyErr != nil || written != length {
			_ = archiveTemp.Close()
			if copyErr != nil {
				return fmt.Errorf("write front-compaction archive: %w", copyErr)
			}
			return errors.New("front-compaction archive was incomplete")
		}
		if j.durability != DurabilityProcess {
			if err := archiveTemp.Sync(); err != nil {
				_ = archiveTemp.Close()
				return fmt.Errorf("sync front-compaction archive: %w", err)
			}
		}
		if err := archiveTemp.Close(); err != nil {
			return fmt.Errorf("close front-compaction archive: %w", err)
		}
		if err := os.Rename(archiveTempPath, archivePath); err != nil {
			return fmt.Errorf("publish front-compaction archive: %w", err)
		}
		if j.durability != DurabilityProcess {
			if err := j.syncDirectory(filepath.Dir(archivePath)); err != nil {
				return fmt.Errorf("sync front-compaction archive directory: %w", err)
			}
		}
	}

	prefixTemp, err := os.CreateTemp(filepath.Dir(j.path), ".front-compact-prefix-*")
	if err != nil {
		return fmt.Errorf("create front-compaction transcript: %w", err)
	}
	prefixTempPath := prefixTemp.Name()
	defer os.Remove(prefixTempPath)
	written, copyErr := io.Copy(prefixTemp, io.NewSectionReader(source, 0, boundary))
	if copyErr != nil || written != boundary {
		_ = prefixTemp.Close()
		if copyErr != nil {
			return fmt.Errorf("write front-compaction transcript: %w", copyErr)
		}
		return errors.New("front-compaction transcript prefix was incomplete")
	}
	if j.durability != DurabilityProcess {
		if err := prefixTemp.Sync(); err != nil {
			_ = prefixTemp.Close()
			return fmt.Errorf("sync front-compaction transcript: %w", err)
		}
	}
	if err := prefixTemp.Close(); err != nil {
		return fmt.Errorf("close front-compaction transcript: %w", err)
	}
	if err := source.Close(); err != nil {
		return fmt.Errorf("close source transcript before front compaction: %w", err)
	}
	if err := os.Rename(prefixTempPath, j.path); err != nil {
		return fmt.Errorf("publish front-compaction transcript: %w", err)
	}
	j.generation++
	if j.durability != DurabilityProcess {
		if err := j.syncDirectory(filepath.Dir(j.path)); err != nil {
			return fmt.Errorf("sync transcript directory: %w", err)
		}
	}
	return nil
}
