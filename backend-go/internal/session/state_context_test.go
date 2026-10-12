package session

import (
	"bytes"
	"encoding/json"
	"strings"
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

	requestBytes, err := BuildRequestWithStateContext(settings, history, nil, []StateContextPrompt{{
		Role:  "system",
		Text:  "STATE CONTEXT SNAPSHOT\nstructured state",
		After: len(history),
	}})
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

func inputRoles(t *testing.T, requestBytes []byte) []string {
	t.Helper()
	var request struct {
		Input []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(requestBytes, &request); err != nil {
		t.Fatal(err)
	}
	labels := make([]string, 0, len(request.Input))
	for _, item := range request.Input {
		label := item.Role
		if len(item.Content) > 0 {
			label += ":" + item.Content[0].Text
		}
		labels = append(labels, label)
	}
	return labels
}

func TestBuildRequestWithStateContextPlacesEachBlockAfterItsHistoryItem(t *testing.T) {
	settings := ModelSettings{Model: "test-model"}
	history := [][]byte{
		[]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"h0"}]}`),
		[]byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"h1"}]}`),
		[]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"h2"}]}`),
	}
	blocks := []StateContextPrompt{
		{Role: "developer", Text: "A", After: 0},
		{Role: "developer", Text: "B", After: 2},
		{Role: "developer", Text: "C", After: 2},
		{Role: "developer", Text: "D", After: 3},
		{Role: "developer", Text: "never sent", After: 4},
	}
	request, err := BuildRequestWithStateContext(settings, history, nil, blocks)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(inputRoles(t, request), " | ")
	want := "developer:A | user:h0 | assistant:h1 | developer:B | developer:C | user:h2 | developer:D"
	if got != want {
		t.Fatalf("request input order\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildRequestWithStateContextKeepsThePrefixBeforeABlockUnchanged(t *testing.T) {
	settings := ModelSettings{Model: "test-model"}
	history := [][]byte{
		[]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"h0"}]}`),
		[]byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"h1"}]}`),
	}
	block := StateContextPrompt{Role: "developer", Text: "B", After: 2}
	before, err := BuildRequestWithStateContext(settings, history, nil, []StateContextPrompt{block})
	if err != nil {
		t.Fatal(err)
	}
	longer := append(append([][]byte(nil), history...), []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"h2"}]}`))
	after, err := BuildRequestWithStateContext(settings, longer, nil, []StateContextPrompt{block})
	if err != nil {
		t.Fatal(err)
	}
	cut := strings.Index(string(before), `"text":"B"`)
	if cut < 0 || !strings.HasPrefix(string(after), string(before)[:cut]) {
		t.Fatalf("appending a history item changed the request before the block:\n%s\n%s", before, after)
	}
}
