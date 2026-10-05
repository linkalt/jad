package transcript

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// StateContextSnapshot is the compacted application-state envelope stored
// beside, but never inside, the golden messages.jsonl transcript.
type StateContextSnapshot struct {
	SchemaVersion int    `json:"schema_version"`
	GeneratedAt   string `json:"generated_at"`
	Trigger       string `json:"trigger"`
	Text          string `json:"text"`
}

// WriteStateContextSnapshot atomically persists a derived context snapshot.
func (store *TurnStore) WriteStateContextSnapshot(snapshot StateContextSnapshot) error {
	if store == nil || store.journal == nil {
		return errors.New("turn store is not initialized")
	}
	if store.options.StateContextPath == "" {
		return errors.New("state context path is not configured")
	}
	if snapshot.SchemaVersion != 1 {
		return fmt.Errorf("unsupported state context schema version %d", snapshot.SchemaVersion)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode state context snapshot: %w", err)
	}
	encoded = append(encoded, '\n')
	path := store.options.StateContextPath

	store.mu.Lock()
	defer store.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create state context directory: %w", err)
	}
	if err := writeJSONAtomic(path, encoded, store.journal.durability != DurabilityProcess); err != nil {
		return fmt.Errorf("persist state context snapshot: %w", err)
	}
	return nil
}

// ReadStateContextSnapshot returns false for missing or malformed snapshots,
// matching the TypeScript store's best-effort derived-cache behavior.
func (store *TurnStore) ReadStateContextSnapshot() (StateContextSnapshot, bool, error) {
	if store == nil || store.journal == nil {
		return StateContextSnapshot{}, false, errors.New("turn store is not initialized")
	}
	if store.options.StateContextPath == "" {
		return StateContextSnapshot{}, false, nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	data, err := os.ReadFile(store.options.StateContextPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StateContextSnapshot{}, false, nil
		}
		return StateContextSnapshot{}, false, fmt.Errorf("read state context snapshot: %w", err)
	}
	var record struct {
		SchemaVersion int     `json:"schema_version"`
		GeneratedAt   *string `json:"generated_at"`
		Trigger       *string `json:"trigger"`
		Text          *string `json:"text"`
	}
	if err := json.Unmarshal(data, &record); err != nil || record.SchemaVersion != 1 || record.GeneratedAt == nil || record.Trigger == nil || record.Text == nil {
		return StateContextSnapshot{}, false, nil
	}
	return StateContextSnapshot{
		SchemaVersion: record.SchemaVersion,
		GeneratedAt:   *record.GeneratedAt,
		Trigger:       *record.Trigger,
		Text:          *record.Text,
	}, true, nil
}

// HasStateContextSnapshot reports whether a valid derived snapshot exists.
func (store *TurnStore) HasStateContextSnapshot() (bool, error) {
	_, exists, err := store.ReadStateContextSnapshot()
	return exists, err
}
