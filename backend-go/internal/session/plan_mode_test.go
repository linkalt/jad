package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/transcript"
)

// requestStreamer records each request and answers it with one completed message.
type requestStreamer struct {
	requests []json.RawMessage
}

func (s *requestStreamer) Stream(_ context.Context, request json.RawMessage, handle responses.EventHandler) error {
	s.requests = append(s.requests, append(json.RawMessage(nil), request...))
	for _, raw := range []string{
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"the plan"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-test","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"output":[]}}`,
	} {
		var typed struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(raw), &typed)
		if err := handle(responses.Event{Type: typed.Type, Raw: json.RawMessage(raw)}); err != nil {
			return err
		}
	}
	return nil
}

func TestPlanPromptIsSentLastAndNeverStored(t *testing.T) {
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	streamer := &requestStreamer{}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test", PromptRole: "developer", PromptText: "base prompt"})
	if err != nil {
		t.Fatal(err)
	}
	options := RunOptions{
		TurnID:       "turn-plan",
		Plan:         true,
		PlanPrompt:   StateContextPrompt{Role: "system", Text: "Plan only.", After: 99},
		StateContext: []StateContextPrompt{{Role: "user", Text: "state block", After: 0}},
	}
	if _, err := runner.Run(context.Background(), json.RawMessage(`"add a cache"`), options); err != nil {
		t.Fatal(err)
	}
	if len(streamer.requests) != 1 {
		t.Fatalf("requests = %d", len(streamer.requests))
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
	want := "developer:base prompt|user:state block|user:add a cache|system:Plan only."
	if strings.Join(got, "|") != want {
		t.Fatalf("input = %v, want %s", got, want)
	}
	if len(options.StateContext) != 1 {
		t.Fatal("the caller's state context was changed")
	}

	var records []string
	if err := journal.ReadLines(func(line []byte) error {
		records = append(records, string(line))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if strings.Contains(record, "Plan only.") {
			t.Fatalf("the plan prompt was stored: %s", record)
		}
	}

	// Without plan mode no plan prompt is sent.
	streamer.requests = nil
	if _, err := runner.Run(context.Background(), json.RawMessage(`"next"`), RunOptions{TurnID: "turn-work"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(streamer.requests[0]), "Plan only.") {
		t.Fatal("a run without plan mode sent the plan prompt")
	}
}
