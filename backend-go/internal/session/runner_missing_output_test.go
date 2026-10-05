package session

import (
	"context"
	"encoding/json"
	"testing"

	"example.com/agent-harness/backend-go/internal/responses"
	"example.com/agent-harness/backend-go/internal/transcript"
)

func TestRunnerRebuildsOmittedOutputFromStagedItems(t *testing.T) {
	store, _, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	streamer := &fakeStreamer{events: []responses.Event{
		{
			Type: "response.output_item.done",
			Raw:  json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered text"}]}}`),
		},
		{
			Type: "response.completed",
			Raw:  json.RawMessage(`{"type":"response.completed","response":{"id":"resp_missing_output","status":"completed"}}`),
		},
	}}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), json.RawMessage(`"prompt"`), RunOptions{TurnID: "turn-missing-output"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var envelope struct {
		Response struct {
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(result.Event, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Response.Output) != 1 || envelope.Response.Output[0].Type != "message" || envelope.Response.Output[0].Content[0].Text != "recovered text" {
		t.Fatalf("completed response output was not reconstructed: %+v", envelope.Response.Output)
	}
}
