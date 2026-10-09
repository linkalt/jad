package session

import (
	"encoding/json"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/openaiapi"
	"example.com/agent-harness/backend-go/internal/transcript"
)

func requestTools(t *testing.T, request json.RawMessage) []map[string]any {
	t.Helper()
	var body struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(request, &body); err != nil {
		t.Fatal(err)
	}
	return body.Tools
}

func functionToolJSON(name string) json.RawMessage {
	return json.RawMessage(`{"type":"function","name":"` + name + `","description":"d","parameters":{"type":"object"},"strict":false}`)
}

func TestHostedToolsFollowTheFunctionTools(t *testing.T) {
	settings := ModelSettings{Model: "m", HostedTools: []json.RawMessage{
		json.RawMessage(`{"type":"web_search","search_context_size":"low"}`),
		json.RawMessage(`{"type":"file_search"}`),
	}}
	request, err := BuildRequestWithTools(settings, nil, []json.RawMessage{functionToolJSON("read"), functionToolJSON("grep")})
	if err != nil {
		t.Fatal(err)
	}
	tools := requestTools(t, request)
	var types []string
	for _, tool := range tools {
		types = append(types, tool["type"].(string))
	}
	if got := strings.Join(types, ","); got != "function,function,web_search,file_search" {
		t.Fatalf("tool order = %s, want the function tools first, then the hosted tools in file order", got)
	}
	if tools[2]["search_context_size"] != "low" {
		t.Fatalf("hosted tool fields must pass on unchanged, got %v", tools[2])
	}
}

func TestHostedToolsAloneStillReachTheRequest(t *testing.T) {
	// No function tool is exposed: the hosted tools must still be sent.
	settings := ModelSettings{Model: "m", HostedTools: []json.RawMessage{json.RawMessage(`{"type":"web_search"}`)}}
	request, err := BuildRequest(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := requestTools(t, request)
	if len(tools) != 1 || tools[0]["type"] != "web_search" {
		t.Fatalf("tools = %v, want exactly the web_search tool", tools)
	}
}

func TestNoHostedToolsLeavesTheRequestUnchanged(t *testing.T) {
	functionTools := []json.RawMessage{functionToolJSON("read")}
	with, err := BuildRequestWithTools(ModelSettings{Model: "m"}, nil, functionTools)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := BuildRequestWithTools(ModelSettings{Model: "m", HostedTools: []json.RawMessage{}}, nil, functionTools)
	if err != nil {
		t.Fatal(err)
	}
	if string(with) != string(empty) {
		t.Fatalf("an empty hosted list changed the request:\n%s\n%s", with, empty)
	}
	if got := requestTools(t, with); len(got) != 1 {
		t.Fatalf("tools = %v, want only the function tool", got)
	}
	bare, err := BuildRequest(ModelSettings{Model: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), `"tools"`) {
		t.Fatalf("a request with no tools at all must not carry a tools key: %s", bare)
	}
}

func TestBuildRequestDoesNotShareTheHostedToolsWithTheFunctionList(t *testing.T) {
	functionTools := make([]json.RawMessage, 1, 8) // spare capacity: an append in place would write into it
	functionTools[0] = functionToolJSON("read")
	settings := ModelSettings{Model: "m", HostedTools: []json.RawMessage{json.RawMessage(`{"type":"web_search"}`)}}
	if _, err := BuildRequestWithTools(settings, nil, functionTools); err != nil {
		t.Fatal(err)
	}
	if len(functionTools) != 1 || len(functionTools[:2]) < 2 || functionTools[:2][1] != nil {
		t.Fatalf("the caller's tool slice was written into: %v", functionTools[:2])
	}
}

func TestHostedToolsAreRefusedWhenMalformed(t *testing.T) {
	cases := map[string]string{
		"not an object":     `"web_search"`,
		"null entry":        `null`,
		"no type":           `{"name":"x"}`,
		"empty type":        `{"type":""}`,
		"blank type":        `{"type":"  "}`,
		"type not a string": `{"type":3}`,
		"function type":     `{"type":"function","name":"x"}`,
		"function padded":   `{"type":" function "}`,
		"function cased":    `{"type":"Function"}`,
	}
	for name, tool := range cases {
		t.Run(name, func(t *testing.T) {
			store, _, _ := newTestStore(t, transcript.ResponseMetadataFull)
			client := openaiapi.NewClient(openaiapi.Config{APIKey: "k", BaseURL: "http://127.0.0.1:1/v1"})
			if _, err := NewRunner(store, client, ModelSettings{Model: "m", HostedTools: []json.RawMessage{json.RawMessage(tool)}}); err == nil {
				t.Fatalf("NewRunner accepted %s", tool)
			}
			if err := validateModelSettings(ModelSettings{Model: "m", HostedTools: []json.RawMessage{json.RawMessage(tool)}}); err == nil {
				t.Fatalf("validateModelSettings accepted %s", tool)
			}
		})
	}
	if err := validateModelSettings(ModelSettings{Model: "m", HostedTools: []json.RawMessage{json.RawMessage(`{"type":"web_search"}`)}}); err != nil {
		t.Fatalf("a valid hosted tool was refused: %v", err)
	}
}

func TestRunnerKeepsItsOwnCopyOfTheHostedTools(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{"type":"web_search"}`)}
	list := cloneRawList(tools)
	tools[0][2] = 'X' // change the caller's bytes
	tools[0] = json.RawMessage(`{"type":"other"}`)
	if string(list[0]) != `{"type":"web_search"}` {
		t.Fatalf("clone shares memory with the caller: %s", list[0])
	}
	if cloneRawList(nil) != nil || cloneRawList([]json.RawMessage{}) != nil {
		t.Fatal("an empty list must stay nil")
	}
}

func TestNewRunnerCopiesTheHostedToolsItIsGiven(t *testing.T) {
	store, _, _ := newTestStore(t, transcript.ResponseMetadataFull)
	client := openaiapi.NewClient(openaiapi.Config{APIKey: "k", BaseURL: "http://127.0.0.1:1/v1"})
	given := []json.RawMessage{json.RawMessage(`{"type":"web_search"}`)}
	runner, err := NewRunner(store, client, ModelSettings{Model: "m", HostedTools: given})
	if err != nil {
		t.Fatal(err)
	}
	// The caller reuses its buffers after the runner was built.
	copy(given[0], []byte(`{"type":"XXXXXXXXXX"}`))
	given[0] = json.RawMessage(`{"type":"other"}`)
	request, err := BuildRequest(runner.settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := requestTools(t, request)
	if len(tools) != 1 || tools[0]["type"] != "web_search" {
		t.Fatalf("the runner followed the caller's later change: %v", tools)
	}
}
