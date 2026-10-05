package transcript

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// runErrorCacheSchemaVersion is the only version of run-error-cache.json this
// store reads; a file with another version reads as empty.
const runErrorCacheSchemaVersion = 1

// RunErrorEntry is one failed run kept beside the transcript.
//
// messages.jsonl is the provider request history and never holds a failure, so
// a failed run leaves an orphan user turn there and nothing else. This file
// keeps the failure so it outlives the in-memory run-state window. The JSON
// matches the TypeScript backend's run-error-cache.json.
type RunErrorEntry struct {
	// Turn is the session turn whose user message has no answer.
	Turn int64 `json:"turn"`
	// MessageID is the web-ui id of that user message (session-user-o<offset>).
	// Empty when the input had no visible text; readers then show the failure
	// after the last message.
	MessageID string `json:"message_id,omitempty"`
	TurnID    string `json:"turn_id"`
	// At is the ISO timestamp of the failure.
	At string `json:"at"`
	// Message is client-safe text: the same message the browser got as the
	// error event.
	Message string `json:"message"`
}

type runErrorCacheFile struct {
	SchemaVersion int             `json:"schema_version"`
	Errors        []RunErrorEntry `json:"errors"`
}

// RecordRunError adds one entry and rewrites the cache atomically. A later
// failure of the same turn replaces the earlier one, and the oldest entries are
// dropped past maxEntries. It does nothing when no cache path is configured.
func (s *TurnStore) RecordRunError(entry RunErrorEntry, maxEntries int) error {
	if s == nil || s.journal == nil {
		return errors.New("turn store is not initialized")
	}
	path := s.options.RunErrorCachePath
	if path == "" {
		return nil
	}
	if maxEntries < 1 {
		return fmt.Errorf("run error cache max entries must be at least 1, got %d", maxEntries)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing := readRunErrorFile(path)
	entries := make([]RunErrorEntry, 0, len(existing)+1)
	for _, item := range existing {
		if item.Turn != entry.Turn {
			entries = append(entries, item)
		}
	}
	entries = append(entries, entry)
	sort.SliceStable(entries, func(left, right int) bool { return entries[left].Turn < entries[right].Turn })
	if len(entries) > maxEntries {
		entries = entries[len(entries)-maxEntries:]
	}
	encoded, err := json.Marshal(runErrorCacheFile{SchemaVersion: runErrorCacheSchemaVersion, Errors: entries})
	if err != nil {
		return fmt.Errorf("encode run error cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create run error cache directory: %w", err)
	}
	if err := writeJSONAtomic(path, append(encoded, '\n'), s.journal.durability != DurabilityProcess); err != nil {
		return fmt.Errorf("persist run error cache: %w", err)
	}
	return nil
}

// ReadRunErrors returns the failed runs still on record, oldest first. A
// missing, corrupt or foreign-version file reads as none: the cache is only a
// convenience and can be deleted at any time.
func (s *TurnStore) ReadRunErrors() []RunErrorEntry {
	if s == nil || s.journal == nil || s.options.RunErrorCachePath == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return readRunErrorFile(s.options.RunErrorCachePath)
}

func readRunErrorFile(path string) []RunErrorEntry {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var file struct {
		SchemaVersion int               `json:"schema_version"`
		Errors        []json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(encoded, &file) != nil || file.SchemaVersion != runErrorCacheSchemaVersion {
		return nil
	}
	entries := make([]RunErrorEntry, 0, len(file.Errors))
	for _, raw := range file.Errors {
		// Pointers tell a missing field from an empty one, as the TypeScript
		// reader does: every field but message_id must be present.
		var item struct {
			Turn      *int64  `json:"turn"`
			MessageID *string `json:"message_id"`
			TurnID    *string `json:"turn_id"`
			At        *string `json:"at"`
			Message   *string `json:"message"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Turn == nil || *item.Turn < 1 ||
			item.TurnID == nil || item.At == nil || item.Message == nil {
			continue
		}
		entry := RunErrorEntry{Turn: *item.Turn, TurnID: *item.TurnID, At: *item.At, Message: *item.Message}
		if item.MessageID != nil {
			entry.MessageID = *item.MessageID
		}
		entries = append(entries, entry)
	}
	return entries
}
