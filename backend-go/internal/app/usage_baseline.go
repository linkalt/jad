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
//
// An entry's summary is cumulative, so it cannot be split to give the usage of a shorter prefix
// of the bytes it covers; a fork that cuts inside them (usage_baseline_fork.go) needs a checkpoint
// of its own. Two things provide one. A front compaction keeps a prefix of the file it replaces
// and removes what followed, so every older entry that still fits the kept prefix stays beside the
// new one: each is the exact usage at its own offset. And a front compaction says, in
// PrefixExact, whether anything was ever removed from before the first byte it kept; when nothing
// was, a scan of any prefix is that prefix's usage. A compaction that keeps only the preserved
// response (the records after it are removed) writes neither: its bytes hold no turn start.

const usageBaselineSchemaVersion = 1

// usageBaselineEntry is one covered prefix of the transcript and the usage it stands for.
type usageBaselineEntry struct {
	Offset          int64              `json:"offset"`
	SHA256          string             `json:"sha256"`
	ReplacesSize    int64              `json:"replaces_size"`
	ReplacesSHA256  string             `json:"replaces_sha256"`
	Summary         webui.UsageSummary `json:"summary"`
	LastResponseKey string             `json:"last_response_key,omitempty"`
	// PrefixExact says that no record was ever removed from before the first byte the entry
	// covers: whatever the rewrite removed followed those bytes (a front compaction), so the
	// usage of a prefix of them is what a scan of that prefix counts. Absent means unknown, as in
	// every entry written before the field existed.
	PrefixExact bool `json:"prefix_exact,omitempty"`
}

// usageBaselineFile holds the entries newest first.
type usageBaselineFile struct {
	SchemaVersion int                  `json:"schema_version"`
	Entries       []usageBaselineEntry `json:"entries"`
}

// errUsageBaselineStale reports that the transcript grew between the usage scan a rewrite starts
// from and the rewrite itself, so the scan no longer accounts for every byte it replaces. The
// rewrite did not happen.
var errUsageBaselineStale = errors.New("usage baseline not stored: the transcript changed while the compaction was prepared; retry")

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
	entries := s.matchingUsageBaselines(config, stat, 1)
	if len(entries) == 0 {
		return usageBaselineEntry{}, false
	}
	return entries[0], true
}

// matchingUsageBaselines returns the baseline entries that match the transcript, newest first:
// all of them, or the first limit when limit is positive.
func (s *sharedSession) matchingUsageBaselines(config sessionfs.SessionConfig, stat fileStat, limit int) []usageBaselineEntry {
	path := sessionfs.UsageBaselinePath(config.UsageCachePath)
	file, err := readUsageBaselineFile(path)
	if err != nil {
		// Without the baseline the totals of removed history are unknown; say so once.
		if !s.usageBaselineFailed.Swap(true) {
			logging.Or(s.logger).Warn("usage baseline unreadable; usage totals may be incomplete", "path", path, "error", err)
		}
		return nil
	}
	var matching []usageBaselineEntry
	for _, entry := range file.Entries {
		if entry.matches(config.MessagesPath, stat.Size) {
			matching = append(matching, entry)
			if limit > 0 && len(matching) == limit {
				break
			}
		}
	}
	if len(matching) > 0 {
		s.usageBaselineFailed.Store(false)
	}
	return matching
}

// storeUsageBaseline durably records entry, keeping the newest existing entry that matches the
// transcript at config.MessagesPath, which the rewrite has not replaced yet (a crash before the
// rename must find it). keptPrefix says the rewrite publishes a prefix of that transcript (front
// compaction): the entry then also keeps every older entry that fits the prefix, and says whether
// nothing was removed from before it. historyPartial is the usage scan's own HistoryPartial.
func (s *sharedSession) storeUsageBaseline(config sessionfs.SessionConfig, entry usageBaselineEntry, keptPrefix, historyPartial bool) error {
	path := sessionfs.UsageBaselinePath(config.UsageCachePath)
	var previous []usageBaselineEntry
	readable := false
	existing, err := readUsageBaselineFile(path)
	if err != nil {
		// An unreadable baseline protects nothing that can still be recovered, and refusing to
		// compact until someone deletes it would not bring those totals back.
		logging.Or(s.logger).Warn("replacing an unreadable usage baseline", "path", path, "error", err)
	} else if stat, statErr := statFile(config.MessagesPath); statErr == nil {
		readable = true
		for _, candidate := range existing.Entries {
			if candidate.matches(config.MessagesPath, stat.Size) {
				previous = append(previous, candidate)
			}
		}
	}
	if keptPrefix {
		// The kept bytes must be a prefix of the file being replaced, or the older entries
		// describe other bytes.
		if hash, hashErr := transcript.HashFileRange(config.MessagesPath, 0, entry.Offset); hashErr != nil || hash != entry.SHA256 {
			keptPrefix = false
		}
	}
	if keptPrefix {
		switch {
		case !readable:
		case len(previous) == 0:
			// Nothing stands for removed history: either none was removed, or the scan says it
			// is unknown.
			entry.PrefixExact = !historyPartial
		default:
			entry.PrefixExact = previous[0].PrefixExact
		}
	}
	file := usageBaselineFile{SchemaVersion: usageBaselineSchemaVersion, Entries: []usageBaselineEntry{entry}}
	if len(previous) > 0 {
		file.Entries = append(file.Entries, previous[0])
	}
	if keptPrefix {
		offsets := map[int64]struct{}{entry.Offset: {}}
		for _, kept := range file.Entries {
			offsets[kept.Offset] = struct{}{}
		}
		for _, candidate := range previous[min(1, len(previous)):] {
			if _, seen := offsets[candidate.Offset]; seen || candidate.Offset > entry.Offset {
				continue
			}
			offsets[candidate.Offset] = struct{}{}
			file.Entries = append(file.Entries, candidate)
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
// the usage in before, and records the replacement it saw in *seen. keptPrefix says the rewrite
// publishes a prefix of the transcript (front compaction). It returns nil when the session has no
// usage cache path, and with it no baseline.
func (s *sharedSession) usageBaselineHook(config sessionfs.SessionConfig, before transcriptIndex, seen *transcript.Replacement, keptPrefix bool) func(transcript.Replacement) error {
	if sessionfs.UsageBaselinePath(config.UsageCachePath) == "" {
		return nil
	}
	return func(replacement transcript.Replacement) error {
		if replacement.PreviousSize != before.through {
			// The scan stops at the last record, so blank lines after it are bytes it did not
			// count and could not have: they hold no usage. Anything else is a record the scan
			// has not seen.
			if replacement.PreviousSize < before.through || !onlyNewlines(config.MessagesPath, before.through, replacement.PreviousSize) {
				return errUsageBaselineStale
			}
		}
		if *seen == replacement {
			// Asked again right before the rename about the same replacement: already stored.
			return nil
		}
		// The transcript is still the file being replaced, and the journal lock keeps it so.
		replaced, err := transcript.HashFileRange(config.MessagesPath, 0, replacement.PreviousSize)
		if err != nil {
			return fmt.Errorf("usage baseline not stored: hash the transcript being replaced: %w", err)
		}
		*seen = replacement
		return s.storeUsageBaseline(config, usageBaselineEntry{
			Offset:          replacement.Size,
			SHA256:          replacement.SHA256,
			ReplacesSize:    replacement.PreviousSize,
			ReplacesSHA256:  replaced,
			Summary:         resetLatestResponseUsage(before.usage),
			LastResponseKey: before.lastResponseKey,
		}, keptPrefix, before.usage.HistoryPartial)
	}
}

// onlyNewlines reports whether bytes [from, to) of the file at path are all newlines: empty lines,
// which the transcript readers skip.
func onlyNewlines(path string, from, to int64) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	buffer := make([]byte, 4096)
	for from < to {
		chunk := buffer[:min(int64(len(buffer)), to-from)]
		n, err := file.ReadAt(chunk, from)
		if n == 0 && err != nil {
			return false
		}
		for _, b := range chunk[:n] {
			if b != '\n' {
				return false
			}
		}
		from += int64(n)
	}
	return true
}
