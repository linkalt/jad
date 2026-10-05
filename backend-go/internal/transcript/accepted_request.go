package transcript

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AcceptedRequestSnapshot preserves the latest user-approved draft outside
// messages.jsonl so compaction can restore approved request context if needed.
type AcceptedRequestSnapshot struct {
	SchemaVersion int             `json:"schema_version"`
	Version       int64           `json:"version"`
	AcceptedAt    string          `json:"accepted_at"`
	Source        string          `json:"source"`
	Action        string          `json:"action"`
	CallID        string          `json:"call_id"`
	Draft         json.RawMessage `json:"draft"`
}

func (store *TurnStore) WriteAcceptedRequest(draft json.RawMessage, source, action, callID string) (AcceptedRequestSnapshot, error) {
	if store == nil || store.journal == nil {
		return AcceptedRequestSnapshot{}, errors.New("turn store is not initialized")
	}
	if store.options.AcceptedRequestPath == "" {
		return AcceptedRequestSnapshot{}, errors.New("accepted request path is not configured")
	}
	if !validAcceptedRequestDraft(draft) {
		return AcceptedRequestSnapshot{}, errors.New("accepted request draft must be a JSON object")
	}
	if source != "user" && source != "timeout" {
		return AcceptedRequestSnapshot{}, fmt.Errorf("unsupported accepted request source %q", source)
	}
	if action != "accept" && action != "update" {
		return AcceptedRequestSnapshot{}, fmt.Errorf("unsupported accepted request action %q", action)
	}
	if callID == "" {
		return AcceptedRequestSnapshot{}, errors.New("accepted request call ID is required")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	path := store.options.AcceptedRequestPath
	var previous AcceptedRequestSnapshot
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &previous)
		if !validAcceptedRequestSnapshot(previous) {
			previous = AcceptedRequestSnapshot{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return AcceptedRequestSnapshot{}, fmt.Errorf("read previous accepted request: %w", err)
	}
	snapshot := AcceptedRequestSnapshot{
		SchemaVersion: 1,
		Version:       previous.Version + 1,
		AcceptedAt:    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Source:        source,
		Action:        action,
		CallID:        callID,
		Draft:         append(json.RawMessage(nil), draft...),
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return AcceptedRequestSnapshot{}, fmt.Errorf("encode accepted request: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return AcceptedRequestSnapshot{}, fmt.Errorf("create accepted request directory: %w", err)
	}
	if err := writeJSONAtomic(path, encoded, store.journal.durability != DurabilityProcess); err != nil {
		return AcceptedRequestSnapshot{}, fmt.Errorf("persist accepted request: %w", err)
	}
	return snapshot, nil
}

// ReadAcceptedRequest returns false for absent or malformed derived snapshots.
func (store *TurnStore) ReadAcceptedRequest() (AcceptedRequestSnapshot, bool, error) {
	if store == nil || store.journal == nil {
		return AcceptedRequestSnapshot{}, false, errors.New("turn store is not initialized")
	}
	if store.options.AcceptedRequestPath == "" {
		return AcceptedRequestSnapshot{}, false, nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	data, err := os.ReadFile(store.options.AcceptedRequestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AcceptedRequestSnapshot{}, false, nil
		}
		return AcceptedRequestSnapshot{}, false, fmt.Errorf("read accepted request: %w", err)
	}
	var snapshot AcceptedRequestSnapshot
	if json.Unmarshal(data, &snapshot) != nil || !validAcceptedRequestSnapshot(snapshot) {
		return AcceptedRequestSnapshot{}, false, nil
	}
	snapshot.Draft = append(json.RawMessage(nil), snapshot.Draft...)
	return snapshot, true, nil
}

func validAcceptedRequestSnapshot(snapshot AcceptedRequestSnapshot) bool {
	return snapshot.SchemaVersion == 1 && snapshot.Version > 0 && snapshot.AcceptedAt != "" &&
		(snapshot.Source == "user" || snapshot.Source == "timeout") &&
		(snapshot.Action == "accept" || snapshot.Action == "update") && snapshot.CallID != "" &&
		validAcceptedRequestDraft(snapshot.Draft)
}

func validAcceptedRequestDraft(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}
