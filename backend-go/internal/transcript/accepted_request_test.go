package transcript

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcceptedRequestSnapshotIsSeparateVersionedAndResettable(t *testing.T) {
	root := t.TempDir()
	messages := filepath.Join(root, "messages.jsonl")
	acceptedPath := filepath.Join(root, "staging", "accepted-request.json")
	journal, err := NewJournal(messages, DurabilityTurn)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(root, "staging"), TurnStoreOptions{MaxItemBytes: 1024, AcceptedRequestPath: acceptedPath})
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.WriteAcceptedRequest(json.RawMessage(`{"goal":"ship"}`), "user", "accept", "call-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.SchemaVersion != 1 || first.Version != 1 || first.Source != "user" || first.Action != "accept" || first.CallID != "call-1" || string(first.Draft) != `{"goal":"ship"}` {
		t.Fatalf("first accepted request = %+v", first)
	}
	read, exists, err := store.ReadAcceptedRequest()
	if err != nil || !exists || read.Version != 1 || string(read.Draft) != string(first.Draft) {
		t.Fatalf("read accepted request = %+v, exists=%t, err=%v", read, exists, err)
	}
	if items, err := store.ReadInputItems(); err != nil || len(items) != 0 {
		t.Fatalf("accepted request changed transcript items: %q, err=%v", items, err)
	}
	if data, err := os.ReadFile(messages); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	} else if len(data) != 0 {
		t.Fatalf("messages.jsonl contains accepted-request data: %q", data)
	}

	second, err := store.WriteAcceptedRequest(json.RawMessage(`{"goal":"ship safely"}`), "timeout", "update", "call-2")
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != 2 || second.Source != "timeout" || second.Action != "update" || second.CallID != "call-2" {
		t.Fatalf("second accepted request = %+v", second)
	}
	if err := store.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(acceptedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("accepted request after reset stat error = %v", err)
	}
}

func TestReadAcceptedRequestTreatsMalformedSnapshotAsAbsent(t *testing.T) {
	root := t.TempDir()
	messages := filepath.Join(root, "messages.jsonl")
	path := filepath.Join(root, "accepted-request.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := NewJournal(messages, DurabilityTurn)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(root, "staging"), TurnStoreOptions{MaxItemBytes: 1024, AcceptedRequestPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.ReadAcceptedRequest(); err != nil || exists {
		t.Fatalf("malformed accepted request exists=%t err=%v", exists, err)
	}
}
