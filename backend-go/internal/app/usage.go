package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"jad/backend-go/internal/logging"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/transcript"
	"jad/backend-go/internal/webui"
)

const maxSafeUsageInteger int64 = 9007199254740991

type usageCheckpoint struct {
	Device          uint64 `json:"device"`
	Inode           uint64 `json:"inode"`
	Offset          int64  `json:"offset"`
	ModifiedAtMS    int64  `json:"modified_at_ms"`
	ChangedAtMS     int64  `json:"changed_at_ms"`
	CheckpointStart int64  `json:"checkpoint_start"`
	CheckpointSHA   string `json:"checkpoint_sha256"`
}

type usageCacheFile struct {
	SchemaVersion   int                `json:"schema_version"`
	Transcript      usageCheckpoint    `json:"transcript"`
	Summary         webui.UsageSummary `json:"summary"`
	LastResponseKey string             `json:"last_response_key,omitempty"`
}

func readUsageCacheState(config sessionfs.SessionConfig) (usageCacheFile, bool) {
	stat, err := statFile(config.MessagesPath)
	if err != nil {
		return usageCacheFile{}, false
	}
	return readUsageCacheStateAt(config, stat)
}

// readUsageCacheStateAt validates the cache against stat, one earlier stat of the transcript, so
// a caller can tie the cache it accepts to the file it later scans. The checkpoint window is
// still read from the path, and a different file there fails the digest.
func readUsageCacheStateAt(config sessionfs.SessionConfig, stat fileStat) (usageCacheFile, bool) {
	encoded, err := os.ReadFile(config.UsageCachePath)
	if err != nil {
		return usageCacheFile{}, false
	}
	var cache usageCacheFile
	if json.Unmarshal(encoded, &cache) != nil || cache.SchemaVersion != 1 || !validUsageSummary(cache.Summary) {
		return usageCacheFile{}, false
	}
	checkpoint := cache.Transcript
	if checkpoint.Offset < 0 || checkpoint.CheckpointStart < 0 || checkpoint.CheckpointStart > checkpoint.Offset {
		return usageCacheFile{}, false
	}
	digest, err := hex.DecodeString(checkpoint.CheckpointSHA)
	if err != nil || len(digest) != sha256.Size {
		return usageCacheFile{}, false
	}
	if stat.Size < checkpoint.Offset {
		return usageCacheFile{}, false
	}
	if !stat.HasIdentity || stat.Dev != checkpoint.Device || stat.Ino != checkpoint.Inode {
		return usageCacheFile{}, false
	}
	if stat.Size == checkpoint.Offset {
		changedAtMS, hasChangedAt := stat.changeMS()
		if !hasChangedAt || stat.modMS() != checkpoint.ModifiedAtMS || changedAtMS != checkpoint.ChangedAtMS {
			return usageCacheFile{}, false
		}
		return cache, true
	}
	actual, err := transcript.HashFileRange(config.MessagesPath, checkpoint.CheckpointStart, checkpoint.Offset)
	if err != nil || actual != checkpoint.CheckpointSHA {
		return usageCacheFile{}, false
	}
	return cache, true
}

func validUsageSummary(summary webui.UsageSummary) bool {
	if !validUsageCount(summary.CompletedResponses) || !validUsageCount(summary.ResponsesWithUsage) ||
		summary.ResponsesWithUsage > summary.CompletedResponses || !validUsageCount(summary.TurnCount) ||
		!validTokenUsage(summary.TokenUsage) {
		return false
	}
	if summary.Current == nil {
		return true
	}
	return summary.Current.Turn > 0 && validUsageCount(summary.Current.Turn) &&
		summary.Current.Step > 0 && validUsageCount(summary.Current.Step) &&
		validTokenUsage(summary.Current.TokenUsage)
}

func validTokenUsage(usage webui.TokenUsage) bool {
	for _, value := range []*int64{usage.InputTokens, usage.OutputTokens, usage.TotalTokens, usage.CachedInputTokens} {
		if value != nil && !validUsageCount(*value) {
			return false
		}
	}
	return true
}

func validUsageCount(value int64) bool {
	return value >= 0 && value <= maxSafeUsageInteger
}

func usageCacheResponseKey(raw json.RawMessage, response map[string]json.RawMessage) string {
	if response == nil {
		return ""
	}
	identity := "record:" + string(raw)
	var responseID string
	if json.Unmarshal(response["id"], &responseID) == nil && responseID != "" {
		identity = "response:" + responseID
	}
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

// errUsageCacheSuperseded reports a cache that was not written because the transcript is no
// longer the file its summary was computed from. It is not a failure: the next read rebuilds.
var errUsageCacheSuperseded = errors.New("transcript was replaced since the usage was computed")

// writeUsageCacheCheckpoint writes the cache for summary, which accounts for the transcript up
// to offset. The identity it stamps is read from the file at the path now, so expectGeneration
// names the file summary came from (see fileStat.generation); when the path holds another file,
// stamping it would make a cache for the old file's totals pass for the new file.
func writeUsageCacheCheckpoint(config sessionfs.SessionConfig, summary webui.UsageSummary, offset int64, lastResponseKey, expectGeneration string) error {
	path := config.UsageCachePath
	if path == "" {
		return nil
	}
	stat, err := statFile(config.MessagesPath)
	if err != nil {
		return fmt.Errorf("stat transcript for usage cache: %w", err)
	}
	if expectGeneration != "" && stat.generation() != expectGeneration {
		return errUsageCacheSuperseded
	}
	if offset < 0 || offset > stat.Size {
		return nil
	}
	device, inode, identityValid := stat.Dev, stat.Ino, stat.HasIdentity
	changedAtMS, changedAtValid := stat.changeMS()
	if !identityValid || !changedAtValid {
		return nil
	}
	checkpointStart := offset - 64*1024
	if checkpointStart < 0 {
		checkpointStart = 0
	}
	checkpointHash, err := transcript.HashFileRange(config.MessagesPath, checkpointStart, offset)
	if err != nil {
		return fmt.Errorf("hash transcript for usage cache: %w", err)
	}
	cache := usageCacheFile{
		SchemaVersion: 1,
		Transcript: usageCheckpoint{
			Device:          device,
			Inode:           inode,
			Offset:          offset,
			ModifiedAtMS:    stat.modMS(),
			ChangedAtMS:     changedAtMS,
			CheckpointStart: checkpointStart,
			CheckpointSHA:   checkpointHash,
		},
		Summary:         summary,
		LastResponseKey: lastResponseKey,
	}
	encoded, err := json.Marshal(cache)
	if err != nil {
		return fmt.Errorf("encode usage cache: %w", err)
	}
	return replaceFileAtomically(path, encoded, config.Persistence.Durability != "process", config.Persistence.Durability == "strict", "usage cache")
}

// replaceFileAtomically replaces path with data through a temporary file in the same directory, so a
// reader sees the old file or the new one. syncFile fsyncs the data before the rename and
// syncDirectory fsyncs the directory after it. what names the file in errors.
func replaceFileAtomically(path string, data []byte, syncFile, syncDirectory bool, what string) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create %s directory: %w", what, err)
	}
	file, err := os.CreateTemp(directory, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create %s temporary file: %w", what, err)
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", what, err)
	}
	if syncFile {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return fmt.Errorf("sync %s: %w", what, err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", what, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", what, err)
	}
	if syncDirectory {
		directoryFile, err := os.Open(directory)
		if err != nil {
			return fmt.Errorf("open %s directory for sync: %w", what, err)
		}
		syncErr := directoryFile.Sync()
		closeErr := directoryFile.Close()
		if syncErr != nil {
			return fmt.Errorf("sync %s directory: %w", what, syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close %s directory: %w", what, closeErr)
		}
	}
	return nil
}

// writeUsageCache stores the cache best-effort: a failed write is logged on the
// first failure of a run of failures and never fails the caller. It reports
// whether the cache was written.
func (s *sharedSession) writeUsageCache(config sessionfs.SessionConfig, summary webui.UsageSummary, offset int64, lastResponseKey, expectGeneration string) bool {
	err := writeUsageCacheCheckpoint(config, summary, offset, lastResponseKey, expectGeneration)
	if err == nil {
		s.usageCacheFailed.Store(false)
		return true
	}
	if errors.Is(err, errUsageCacheSuperseded) {
		return false
	}
	if !s.usageCacheFailed.Swap(true) {
		logging.Or(s.logger).Warn("usage cache write failed; continuing without it", "path", config.UsageCachePath, "error", err)
	}
	return false
}
