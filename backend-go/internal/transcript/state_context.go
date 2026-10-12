package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const stateContextSchemaVersion = 1

// StateContextSnapshot is the rendered application-state envelope. It is stored
// beside, but never inside, the golden messages.jsonl transcript.
type StateContextSnapshot struct {
	SchemaVersion int    `json:"schema_version"`
	GeneratedAt   string `json:"generated_at"`
	Trigger       string `json:"trigger"`
	Text          string `json:"text"`
}

// StateContextEntry is one block of the state-context sidecar (a JSON line). The text
// is stored rendered and never re-rendered, so the request prefix before it stays
// byte-identical from one request to the next.
type StateContextEntry struct {
	StateContextSnapshot
	Role string `json:"role"`
	// After is how many request-history items precede the block when it is sent.
	After int `json:"after"`
}

// AppendStateContext adds a block after the history items that exist now. Earlier
// blocks stay untouched; a compaction or reset removes them all.
func (store *TurnStore) AppendStateContext(snapshot StateContextSnapshot, role string) (StateContextEntry, error) {
	if store == nil || store.journal == nil {
		return StateContextEntry{}, errors.New("turn store is not initialized")
	}
	if store.options.StateContextPath == "" {
		return StateContextEntry{}, errors.New("state context path is not configured")
	}
	if snapshot.SchemaVersion != stateContextSchemaVersion {
		return StateContextEntry{}, fmt.Errorf("unsupported state context schema version %d", snapshot.SchemaVersion)
	}
	if role == "" {
		return StateContextEntry{}, errors.New("state context role is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	items, err := store.readInputItems(nil)
	if err != nil {
		return StateContextEntry{}, fmt.Errorf("count history items for state context: %w", err)
	}
	entry := StateContextEntry{StateContextSnapshot: snapshot, Role: role, After: len(items)}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return StateContextEntry{}, fmt.Errorf("encode state context entry: %w", err)
	}
	encoded = append(encoded, '\n')
	path := store.options.StateContextPath
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return StateContextEntry{}, fmt.Errorf("create state context directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return StateContextEntry{}, fmt.Errorf("open state context file: %w", err)
	}
	if err := writeAll(file, encoded); err != nil {
		_ = file.Close()
		return StateContextEntry{}, fmt.Errorf("append state context entry: %w", err)
	}
	if store.journal.durability != DurabilityProcess {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return StateContextEntry{}, fmt.Errorf("sync state context file: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		return StateContextEntry{}, fmt.Errorf("close state context file: %w", err)
	}
	return entry, nil
}

// ReadStateContextEntries returns the stored blocks in the order they were written. A
// missing file is no blocks; a malformed line is skipped, as the file is a derived cache.
func (store *TurnStore) ReadStateContextEntries() ([]StateContextEntry, error) {
	if store == nil || store.journal == nil {
		return nil, errors.New("turn store is not initialized")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.readStateContextEntries()
}

func (store *TurnStore) readStateContextEntries() ([]StateContextEntry, error) {
	if store.options.StateContextPath == "" {
		return nil, nil
	}
	file, err := os.Open(store.options.StateContextPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read state context file: %w", err)
	}
	defer file.Close()
	var entries []StateContextEntry
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var record struct {
				SchemaVersion int     `json:"schema_version"`
				GeneratedAt   *string `json:"generated_at"`
				Trigger       *string `json:"trigger"`
				Text          *string `json:"text"`
				Role          *string `json:"role"`
				After         *int    `json:"after"`
			}
			if json.Unmarshal(line, &record) == nil && record.SchemaVersion == stateContextSchemaVersion &&
				record.GeneratedAt != nil && record.Trigger != nil && record.Text != nil &&
				record.Role != nil && *record.Role != "" && record.After != nil && *record.After >= 0 {
				entries = append(entries, StateContextEntry{
					StateContextSnapshot: StateContextSnapshot{
						SchemaVersion: record.SchemaVersion, GeneratedAt: *record.GeneratedAt,
						Trigger: *record.Trigger, Text: *record.Text,
					},
					Role: *record.Role, After: *record.After,
				})
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return nil, fmt.Errorf("read state context file: %w", readErr)
			}
			break
		}
	}
	return entries, nil
}

// clampStateContext re-anchors the blocks that followed history items a front compaction
// removed: only the latest of them is kept, now after the last remaining item, because
// an older block is superseded by it. The caller holds store.mu.
func (store *TurnStore) clampStateContext() error {
	entries, err := store.readStateContextEntries()
	if err != nil || len(entries) == 0 {
		return err
	}
	items, err := store.readInputItems(nil)
	if err != nil {
		return fmt.Errorf("count history items for state context: %w", err)
	}
	kept := entries[:0:0]
	var stranded *StateContextEntry
	for index := range entries {
		if entries[index].After <= len(items) {
			kept = append(kept, entries[index])
			continue
		}
		stranded = &entries[index]
	}
	if stranded == nil {
		return nil
	}
	stranded.After = len(items)
	kept = append(kept, *stranded)
	path := store.options.StateContextPath
	var buffer bytes.Buffer
	for _, entry := range kept {
		encoded, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("encode state context entry: %w", err)
		}
		buffer.Write(encoded)
		buffer.WriteByte('\n')
	}
	if err := writeJSONAtomic(path, buffer.Bytes(), store.journal.durability != DurabilityProcess); err != nil {
		return fmt.Errorf("rewrite state context file: %w", err)
	}
	return nil
}
