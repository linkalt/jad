package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/transcript"
)

func TestRunReportsTheAcceptedInputBeforeAFailureAndTheOffsetStartsItsRecord(t *testing.T) {
	messagesPath := filepath.Join(t.TempDir(), "messages.jsonl")
	journal, err := transcript.NewJournal(messagesPath, transcript.DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := transcript.NewTurnStore(journal, filepath.Join(filepath.Dir(messagesPath), "staging"), transcript.TurnStoreOptions{MaxItemBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(store, &fakeStreamer{err: errors.New("provider stream ended unexpectedly")}, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var accepted []PersistedInput
	run := func(text string) error {
		input, _ := json.Marshal(text)
		_, err := runner.Run(context.Background(), input, RunOptions{
			TurnID:          "turn-" + text,
			OnInputAccepted: func(input PersistedInput) { accepted = append(accepted, input) },
		})
		return err
	}
	for _, text := range []string{"first", "second"} {
		if err := run(text); err == nil {
			t.Fatal("the stream failure must fail the run")
		}
	}
	if len(accepted) != 2 {
		t.Fatalf("OnInputAccepted fired %d times, want once per run even though both failed", len(accepted))
	}
	if accepted[0].SessionTurn != 1 || accepted[1].SessionTurn != 2 {
		t.Fatalf("turns = %d, %d; want 1, 2", accepted[0].SessionTurn, accepted[1].SessionTurn)
	}
	data, err := os.ReadFile(messagesPath)
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range []string{"first", "second"} {
		got := accepted[index]
		if !got.HasMessage {
			t.Fatalf("run %d: input with text must report a message", index)
		}
		record := data[got.MessageOffset:]
		if line, _, _ := bytes.Cut(record, []byte("\n")); !bytes.Contains(line, []byte(`"text":"`+want+`"`)) {
			t.Fatalf("run %d: offset %d does not start the %q user record: %s", index, got.MessageOffset, want, line)
		}
	}
}

// An image-only user input is a visible message in the Go web-ui projection (the
// TypeScript one hides it), so a screenshot sent without text is not lost from the screen.
func TestPersistInputReportsAMessageForAnImageOnlyInput(t *testing.T) {
	store, _, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	input := json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]`)
	persisted, err := PersistInput(store, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.HasMessage {
		t.Fatal("an image-only input must report a web-ui message")
	}
	if persisted.SessionTurn != 1 {
		t.Fatalf("session turn = %d, want 1", persisted.SessionTurn)
	}
}
