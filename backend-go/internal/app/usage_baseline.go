package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"jad/backend-go/internal/logging"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/transcript"
	"jad/backend-go/internal/webui"
)

// The usage baseline keeps the session's usage totals across a rewrite that removes records from
// the transcript (compaction, front compaction). The totals of the removed records exist nowhere
// else, so unlike usage-cache.json, which only saves a rescan, the baseline cannot be rebuilt
// and is not deleted by a rewrite.
//
// Each entry says: the first Offset bytes of the transcript hash to SHA256, and Summary is the
// usage of those bytes plus everything the rewrite removed. Readers that find no valid usage
// cache look for the newest entry that fits the file and continue the scan at Offset. Tying an
// entry to the bytes it covers, not to an inode or a time, lets it survive a copy or restore of
// the session folder and be written before the rewrite is published.
//
// An entry also names the transcript it replaced (ReplacesSize, ReplacesSHA256). The summary
// holds the usage of that whole file, so if the rewrite never reached the rename and the old
// file is still current, counting its records again from Offset would count them twice; the
// entry does not fit a file that begins with the bytes it replaced.
//
// A rewrite writes its entry first and keeps the newest entry that fits the transcript it is
// about to replace. Until the rename the old file is current and its entry fits it; after the
// rename the new file is current and the new entry fits it. A crash or an error between the
// two steps therefore loses nothing.

const usageBaselineSchemaVersion = 1

// usageBaselineEntry is one covered prefix of the transcript and the usage it stands for.
type usageBaselineEntry struct {
	Offset          int64              `json:"offset"`
	SHA256          string             `json:"sha256"`
	ReplacesSize    int64              `json:"replaces_size"`
	ReplacesSHA256  string             `json:"replaces_sha256"`
	Summary         webui.UsageSummary `json:"summary"`
	LastResponseKey string             `json:"last_response_key,omitempty"`
}

// usageBaselineFile holds the entries newest first.
type usageBaselineFile struct {
	SchemaVersion int                  `json:"schema_version"`
	Entries       []usageBaselineEntry `json:"entries"`
}

// errUsageBaselineStale reports that the transcript grew between the usage scan a rewrite starts
// from and the rewrite itself, so the scan no longer accounts for every byte it replaces. The
// rewrite did not happen.
var errUsageBaselineStale = errors.New("the transcript changed while the compaction was prepared; retry")

// readUsageBaselineFile reads the baseline; a missing file is an empty baseline.
func readUsageBaselineFile(path string) (usageBaselineFile, error) {
	if path == "" {
		return usageBaselineFile{}, nil
	}
	encoded, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return usageBaselineFile{}, nil
	}
	if err != nil {
		return usageBaselineFile{}, fmt.Errorf("read usage baseline: %w", err)
	}
	var file usageBaselineFile
	if err := json.Unmarshal(encoded, &file); err != nil || file.SchemaVersion != usageBaselineSchemaVersion {
		return usageBaselineFile{}, fmt.Errorf("usage baseline %q is not a version %d baseline file", path, usageBaselineSchemaVersion)
	}
	return file, nil
}

// matches reports whether the transcript at path, size bytes long, is the file the entry was
// made for, or that file with records appended: it begins with the Offset bytes the entry
// covers and not with the file the entry replaced.
func (e usageBaselineEntry) matches(path string, size int64) bool {
	if e.Offset < 0 || e.Offset > size || e.ReplacesSize < 0 || !validUsageSummary(e.Summary) {
		return false
	}
	actual, err := transcript.HashFileRange(path, 0, e.Offset)
	if err != nil || actual != e.SHA256 {
		return false
	}
	if size >= e.ReplacesSize {
		replaced, err := transcript.HashFileRange(path, 0, e.ReplacesSize)
		if err != nil || replaced == e.ReplacesSHA256 {
			return false
		}
	}
	return true
}

// matchUsageBaseline returns the newest baseline entry that matches the transcript.
func (s *sharedSession) matchUsageBaseline(config sessionfs.SessionConfig, stat fileStat) (usageBaselineEntry, bool) {
	path := sessionfs.UsageBaselinePath(config.UsageCachePath)
	file, err := readUsageBaselineFile(path)
	if err != nil {
		// Without the baseline the totals of removed history are unknown; say so once.
		if !s.usageBaselineFailed.Swap(true) {
			logging.Or(s.logger).Warn("usage baseline unreadable; usage totals may be incomplete", "path", path, "error", err)
		}
		return usageBaselineEntry{}, false
	}
	for _, entry := range file.Entries {
		if entry.matches(config.MessagesPath, stat.Size) {
			s.usageBaselineFailed.Store(false)
			return entry, true
		}
	}
	return usageBaselineEntry{}, false
}

// storeUsageBaseline durably records entry, keeping the newest existing entry that matches the
// transcript at config.MessagesPath, which the rewrite has not replaced yet.
func (s *sharedSession) storeUsageBaseline(config sessionfs.SessionConfig, entry usageBaselineEntry) error {
	path := sessionfs.UsageBaselinePath(config.UsageCachePath)
	file := usageBaselineFile{SchemaVersion: usageBaselineSchemaVersion, Entries: []usageBaselineEntry{entry}}
	existing, err := readUsageBaselineFile(path)
	if err != nil {
		// An unreadable baseline protects nothing that can still be recovered, and refusing to
		// compact until someone deletes it would not bring those totals back.
		logging.Or(s.logger).Warn("replacing an unreadable usage baseline", "path", path, "error", err)
	} else if stat, statErr := statFile(config.MessagesPath); statErr == nil {
		for _, previous := range existing.Entries {
			if previous.matches(config.MessagesPath, stat.Size) {
				file.Entries = append(file.Entries, previous)
				break
			}
		}
	}
	encoded, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("encode usage baseline: %w", err)
	}
	durable := config.Persistence.Durability != "process"
	return replaceFileAtomically(path, encoded, durable, durable, "usage baseline")
}

// usageBaselineHook returns the hook that stores the baseline for a rewrite which starts from
// the usage in before, and records the replacement it saw in *seen. It returns nil when the
// session has no usage cache path, and with it no baseline.
func (s *sharedSession) usageBaselineHook(config sessionfs.SessionConfig, before transcriptIndex, seen *transcript.Replacement) func(transcript.Replacement) error {
	if sessionfs.UsageBaselinePath(config.UsageCachePath) == "" {
		return nil
	}
	return func(replacement transcript.Replacement) error {
		if replacement.PreviousSize != before.through {
			return errUsageBaselineStale
		}
		if *seen == replacement {
			// Asked again right before the rename about the same replacement: already stored.
			return nil
		}
		// The transcript is still the file being replaced, and the journal lock keeps it so.
		replaced, err := transcript.HashFileRange(config.MessagesPath, 0, replacement.PreviousSize)
		if err != nil {
			return fmt.Errorf("hash the transcript being replaced: %w", err)
		}
		*seen = replacement
		return s.storeUsageBaseline(config, usageBaselineEntry{
			Offset:          replacement.Size,
			SHA256:          replacement.SHA256,
			ReplacesSize:    replacement.PreviousSize,
			ReplacesSHA256:  replaced,
			Summary:         resetLatestResponseUsage(before.usage),
			LastResponseKey: before.lastResponseKey,
		})
	}
}
