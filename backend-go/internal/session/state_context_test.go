package session

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestBuildRequestWithStateContextAppendsPromptAfterHistory(t *testing.T) {
	settings := ModelSettings{
		Model:      "gpt-test",
		Request:    json.RawMessage(`{"temperature":0}`),
		PromptRole: "developer",
		PromptText: "configured prompt",
	}
	history := [][]byte{[]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`)}
	withoutContext, err := BuildRequestWithTools(settings, history, nil)
	if err != nil {
		t.Fatal(err)
	}
	withoutContextViaNewAPI, err := BuildRequestWithStateContext(settings, history, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(withoutContext, withoutContextViaNewAPI) {
		t.Fatalf("nil state context changed request bytes:\n%s\n%s", withoutContext, withoutContextViaNewAPI)
	}

	requestBytes, err := BuildRequestWithStateContext(settings, history, nil, &StateContextPrompt{
		Role: "system",
		Text: "STATE CONTEXT SNAPSHOT\nstructured state",
	})
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(requestBytes, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 3 {
		t.Fatalf("request input has %d items, want configured prompt, history, and state context", len(request.Input))
	}
	var configuredPrompt struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(request.Input[0], &configuredPrompt); err != nil || configuredPrompt.Role != "developer" {
		t.Fatalf("configured prompt = %s, err = %v", request.Input[0], err)
	}
	if !bytes.Equal(request.Input[1], history[0]) {
		t.Fatalf("history item changed: got %s, want %s", request.Input[1], history[0])
	}
	var statePrompt struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(request.Input[2], &statePrompt); err != nil {
		t.Fatal(err)
	}
	if statePrompt.Role != "system" || len(statePrompt.Content) != 1 || statePrompt.Content[0].Text != "STATE CONTEXT SNAPSHOT\nstructured state" {
		t.Fatalf("state context prompt = %#v", statePrompt)
	}
}
