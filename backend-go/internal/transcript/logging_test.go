package transcript

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommitObserverPanicIsLoggedThroughTheInjectedLogger checks that a panicking
// commit observer is reported on the store's logger (not the process-wide one),
// without the response text and with the panic value.
func TestCommitObserverPanicIsLoggedThroughTheInjectedLogger(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	root := t.TempDir()
	journal, err := NewJournal(filepath.Join(root, "messages.jsonl"), DurabilityStrict)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(root, "staging"), TurnStoreOptions{
		Logger: logger, MaxItemBytes: 1024 * 1024, ResponseMetadata: ResponseMetadataMinimal,
	})
	if err != nil {
		t.Fatal(err)
	}
	item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"PRIVATE-ANSWER"}]}`
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_log","status":"completed","output":[` + item + `]}}`)
	turn, err := store.BeginTurn("turn-log")
	if err != nil {
		t.Fatal(err)
	}
	if err := turn.StageOutputItem(outputItemEvent(0, item)); err != nil {
		t.Fatal(err)
	}
	if _, err := turn.CommitWithObserver(completed, func(CommitResult) { panic("observer failure") }); err != nil {
		t.Fatal(err)
	}
	logged := output.String()
	if !strings.Contains(logged, "level=ERROR") || !strings.Contains(logged, "observer failure") {
		t.Errorf("observer panic not logged on the injected logger:\n%s", logged)
	}
	if strings.Contains(logged, "PRIVATE-ANSWER") {
		t.Errorf("response text leaked into the log:\n%s", logged)
	}
}

func TestStoreWithoutALoggerStaysQuiet(t *testing.T) {
	root := t.TempDir()
	journal, err := NewJournal(filepath.Join(root, "messages.jsonl"), DurabilityStrict)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(root, "staging"), TurnStoreOptions{MaxItemBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.BeginTurn("turn-quiet")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.CommitWithObserver([]byte(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}`), func(CommitResult) { panic("x") }); err != nil {
		t.Fatal(err)
	}
}
