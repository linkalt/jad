package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/transcript"
)

// Notes go after the history and before the plan prompt, in their own roles, and are never
// stored; an empty note is skipped.
func TestRunNotesAreSentAfterTheHistoryAndNeverStored(t *testing.T) {
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	streamer := &requestStreamer{}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test", PromptRole: "developer", PromptText: "base prompt"})
	if err != nil {
		t.Fatal(err)
	}
	options := RunOptions{
		TurnID:     "turn-notes",
		PlanPrompt: StateContextPrompt{Role: "system", Text: "Plan only."},
		Notes:      []StateContextPrompt{{Role: "developer", Text: "docs://plan changed", After: 99}, {Role: "developer"}},
	}
	if _, err := runner.Run(context.Background(), json.RawMessage(`"go"`), options); err != nil {
		t.Fatal(err)
	}
	var request struct {
		Input []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(streamer.requests[0], &request); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, item := range request.Input {
		got = append(got, item.Role+":"+item.Content[0].Text)
	}
	if want := "developer:base prompt|user:go|developer:docs://plan changed|system:Plan only."; strings.Join(got, "|") != want {
		t.Fatalf("input = %v, want %s", got, want)
	}
	if err := journal.ReadLines(func(line []byte) error {
		if strings.Contains(string(line), "docs://plan changed") {
			t.Fatalf("the note was stored: %s", line)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
