package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

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

// readUsageCache accepts only a checkpoint that still describes the current
// transcript. A stale, malformed, or platform-incompatible cache is ignored.
func readUsageCache(config sessionfs.SessionConfig) (webui.UsageSummary, int64, bool) {
	cache, valid := readUsageCacheState(config)
	if !valid {
		return webui.UsageSummary{}, 0, false
	}
	return cache.Summary, cache.Transcript.Offset, true
}

func readUsageCacheState(config sessionfs.SessionConfig) (usageCacheFile, bool) {
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
	info, err := os.Stat(config.MessagesPath)
	if err != nil || info.Size() < checkpoint.Offset {
		return usageCacheFile{}, false
	}
	device, inode, ok := statIdentity(info)
	if !ok || device != checkpoint.Device || inode != checkpoint.Inode {
		return usageCacheFile{}, false
	}
	if info.Size() == checkpoint.Offset {
		changedAtMS, hasChangedAt := statChangeTimeMS(info)
		if !hasChangedAt || modTimeMS(info) != checkpoint.ModifiedAtMS || changedAtMS != checkpoint.ChangedAtMS {
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

func statIdentity(info os.FileInfo) (uint64, uint64, bool) {
	stat := reflect.ValueOf(info.Sys())
	if stat.Kind() == reflect.Pointer {
		if stat.IsNil() {
			return 0, 0, false
		}
		stat = stat.Elem()
	}
	device, deviceOK := unsignedStatField(stat, "Dev")
	inode, inodeOK := unsignedStatField(stat, "Ino")
	return device, inode, deviceOK && inodeOK
}

// modTimeMS returns the modification time as whole milliseconds, truncated
// toward zero. The TypeScript backend stores Math.trunc(mtimeMs) and rejects a
// usage cache whose timestamps are not safe integers, so both backends must
// write and compare whole milliseconds.
func modTimeMS(info os.FileInfo) int64 {
	return info.ModTime().UnixNano() / int64(time.Millisecond)
}

// statChangeTimeMS returns the inode change time as whole milliseconds,
// truncated toward zero like Math.trunc(ctimeMs) in the TypeScript backend.
func statChangeTimeMS(info os.FileInfo) (int64, bool) {
	stat := reflect.ValueOf(info.Sys())
	if stat.Kind() == reflect.Pointer {
		if stat.IsNil() {
			return 0, false
		}
		stat = stat.Elem()
	}
	if stat.Kind() != reflect.Struct {
		return 0, false
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		field := stat.FieldByName(name)
		if !field.IsValid() || field.Kind() != reflect.Struct {
			continue
		}
		seconds, secondsOK := signedOrUnsignedInt(field.FieldByName("Sec"))
		nanoseconds, nanosOK := signedOrUnsignedInt(field.FieldByName("Nsec"))
		if secondsOK && nanosOK {
			return seconds*1000 + nanoseconds/int64(time.Millisecond), true
		}
	}
	return 0, false
}

func signedOrUnsignedInt(value reflect.Value) (int64, bool) {
	if !value.IsValid() {
		return 0, false
	}
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		unsigned := value.Uint()
		if unsigned <= uint64(maxSafeUsageInteger) {
			return int64(unsigned), true
		}
	}
	return 0, false
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

func stringField(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func writeUsageCacheCheckpoint(config sessionfs.SessionConfig, summary webui.UsageSummary, offset int64, lastResponseKey string) error {
	path := config.UsageCachePath
	if path == "" {
		return nil
	}
	info, err := os.Stat(config.MessagesPath)
	if err != nil {
		return fmt.Errorf("stat transcript for usage cache: %w", err)
	}
	if offset < 0 || offset > info.Size() {
		return nil
	}
	device, inode, identityValid := statIdentity(info)
	changedAtMS, changedAtValid := statChangeTimeMS(info)
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
			ModifiedAtMS:    modTimeMS(info),
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
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create usage-cache directory: %w", err)
	}
	file, err := os.CreateTemp(directory, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create usage-cache temporary file: %w", err)
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return fmt.Errorf("write usage cache: %w", err)
	}
	if config.Persistence.Durability != "process" {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return fmt.Errorf("sync usage cache: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close usage cache: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace usage cache: %w", err)
	}
	if config.Persistence.Durability == "strict" {
		directoryFile, err := os.Open(directory)
		if err != nil {
			return fmt.Errorf("open usage-cache directory for sync: %w", err)
		}
		syncErr := directoryFile.Sync()
		closeErr := directoryFile.Close()
		if syncErr != nil {
			return fmt.Errorf("sync usage-cache directory: %w", syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close usage-cache directory: %w", closeErr)
		}
	}
	return nil
}
