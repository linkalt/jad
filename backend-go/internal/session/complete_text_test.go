package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/responses"
)

func TestCompleteTextSendsTheMessagesWithoutToolsAndReturnsTheText(t *testing.T) {
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{finalAnswerRound("sampled answer", 5)}}
	text, err := CompleteText(context.Background(), streamer, ModelSettings{Model: "gpt-test", PromptRole: "developer", PromptText: "session prompt", Request: json.RawMessage(`{"tool_choice":"auto","parallel_tool_calls":true,"max_output_tokens":9000}`)},
		"be brief", []TextMessage{{Role: "user", Text: "say hi"}, {Role: "assistant", Text: "hi"}, {Role: "user", Text: "again"}}, 40, 0)
	if err != nil || text != "sampled answer" {
		t.Fatalf("CompleteText = %q, %v", text, err)
	}
	request := string(streamer.requests[0])
	for _, want := range []string{`"be brief"`, `"say hi"`, `"output_text","text":"hi"`, `"max_output_tokens":40`} {
		if !strings.Contains(request, want) {
			t.Errorf("request misses %s: %s", want, request)
		}
	}
	for _, unwanted := range []string{"session prompt", "tool_choice", "parallel_tool_calls", "9000"} {
		if strings.Contains(request, unwanted) {
			t.Errorf("request holds %s: %s", unwanted, request)
		}
	}
	if _, err := CompleteText(context.Background(), streamer, ModelSettings{Model: "gpt-test"}, "", []TextMessage{{Role: "tool", Text: "x"}}, 0, 0); err == nil {
		t.Fatal("a tool role was accepted")
	}
	if _, err := CompleteText(context.Background(), streamer, ModelSettings{Model: "gpt-test"}, "", nil, 0, 0); err == nil {
		t.Fatal("no messages was accepted")
	}
}
