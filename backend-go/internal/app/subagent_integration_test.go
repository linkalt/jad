package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// subagentProvider is a fake Responses endpoint. It tells the parent run and the child
// run apart by the model name in the request, and records what each of them sent.
type subagentProvider struct {
	mu       sync.Mutex
	requests []providerRequest
	// childFails makes every child request answer with this HTTP status when non-zero.
	childFails int
	// childBlocks makes every child request wait until the client goes away. The
	// channels are created by the test before the run starts.
	childBlocks bool
	// parentCalls is how many subagent calls the parent makes in its first response;
	// zero means one.
	parentCalls    int
	childEntered   chan struct{}
	childCancelled chan struct{}
	// childRelease ends a blocked child request at cleanup, so a run that was not
	// cancelled fails the test instead of hanging the server shutdown.
	childRelease chan struct{}
}

type providerRequest struct {
	Model string
	Tools []string
	Input []json.RawMessage
	Text  string
}

func (p *subagentProvider) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected provider request", http.StatusNotFound)
			return
		}
		var body struct {
			Model string            `json:"model"`
			Tools []json.RawMessage `json:"tools"`
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid Responses request", http.StatusBadRequest)
			return
		}
		record := providerRequest{Model: body.Model, Input: body.Input}
		for _, tool := range body.Tools {
			var named struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(tool, &named)
			record.Tools = append(record.Tools, named.Name)
		}
		for _, item := range body.Input {
			record.Text += string(item) + "\n"
		}
		p.mu.Lock()
		p.requests = append(p.requests, record)
		fail := body.Model == "child-model" && p.childFails != 0
		status := p.childFails
		block := body.Model == "child-model" && p.childBlocks
		p.mu.Unlock()

		if block {
			select {
			case p.childEntered <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
				select {
				case p.childCancelled <- struct{}{}:
				default:
				}
			case <-p.childRelease:
			}
			return
		}

		if fail {
			http.Error(w, `{"error":{"message":"child provider down","type":"server_error"}}`, status)
			return
		}
		var events []string
		switch {
		case body.Model == "parent-model" && !strings.Contains(record.Text, "function_call_output") && p.parentCalls > 1:
			events = manyCallEvents(p.parentCalls)
		case body.Model == "parent-model" && !strings.Contains(record.Text, "function_call_output"):
			events = functionCallEvents("call-sub", "subagent", `{"task":"find the answer"}`)
		case body.Model == "parent-model":
			events = messageEvents("parent saw: "+lastOutput(record.Text), 31)
		case !strings.Contains(record.Text, "function_call_output"):
			// A child's first round: its own history holds no tool result yet. Judging by the
			// request's own history, not a global count, keeps several children apart.
			events = functionCallEvents("call-read", "read", `{"path":"notes.txt"}`)
		default:
			events = messageEvents("the answer is 42", 17)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(event), &head)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, event)
		}
	})
}

func (p *subagentProvider) forModel(model string) []providerRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	var matched []providerRequest
	for _, request := range p.requests {
		if request.Model == model {
			matched = append(matched, request)
		}
	}
	return matched
}

func functionCallEvents(callID, name, arguments string) []string {
	encoded, _ := json.Marshal(arguments)
	item := fmt.Sprintf(`{"type":"function_call","call_id":%q,"name":%q,"arguments":%s}`, callID, name, encoded)
	return []string{
		`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
		`{"type":"response.completed","response":{"id":"resp-call","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	}
}

// manyCallEvents is one response holding several subagent calls, each with its own task.
func manyCallEvents(count int) []string {
	var events []string
	for index := range count {
		arguments, _ := json.Marshal(fmt.Sprintf(`{"task":"task number %d"}`, index))
		item := fmt.Sprintf(`{"type":"function_call","call_id":"call-sub-%d","name":"subagent","arguments":%s}`, index, arguments)
		events = append(events, fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`, index, item))
	}
	return append(events, `{"type":"response.completed","response":{"id":"resp-many","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`)
}

func messageEvents(text string, totalTokens int) []string {
	encoded, _ := json.Marshal(text)
	item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + string(encoded) + `}]}`
	return []string{
		`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
		fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-final","status":"completed","output":[],"usage":{"input_tokens":%d,"output_tokens":1,"total_tokens":%d}}}`, totalTokens-1, totalTokens),
	}
}

// lastOutput returns the text of the last function_call_output in a request dump.
func lastOutput(dump string) string {
	index := strings.LastIndex(dump, `"type":"function_call_output"`)
	if index < 0 {
		return ""
	}
	return dump[index:]
}

type subagentFixture struct {
	workspace  string
	sessionDir string
	apiURL     string
	provider   *subagentProvider
}

// newSubagentFixture builds a session with the read tool and the subagent tool, and a
// parent model and a child model that both point at the fake provider.
func newSubagentFixture(t *testing.T, agentSetting map[string]any) subagentFixture {
	t.Helper()
	workspace := t.TempDir()
	sessionDir := filepath.Join(workspace, "sessions", "demo")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := &subagentProvider{}
	server := httptest.NewServer(provider.handler(t))
	t.Cleanup(server.Close)

	manifest := func(file string) {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "sample", "tools", "native", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workspace, file), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest("read.json")
	manifest("subagent.json")
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("the secret is 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "setting.json"), []byte(`{"session_roots":[{"id":"local","path":"sessions"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	subagent := map[string]any{"agents": map[string]any{"explore": agentSetting}}
	setting, err := json.Marshal(map[string]any{
		"cwd": workspace, "max_tool_rounds": 4,
		"tool_definition_paths": []string{"read.json", "subagent.json"},
		"tools": map[string]any{"native": map[string]any{
			"read":     map[string]any{"paths": map[string]any{"allow": []string{workspace}, "deny": []string{}}},
			"subagent": subagent,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sessionDir, "setting.json"), setting)
	for file, model := range map[string]string{"model.json": "parent-model", "child.json": "child-model"} {
		body, _ := json.Marshal(map[string]any{"provider": "openai", "model": model, "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": server.URL + "/v1"})
		writeFile(t, filepath.Join(sessionDir, file), body)
	}

	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir:  workspace,
		Environment: map[string]string{"AGENT_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"},
	})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	api := httptest.NewServer(service.Handler)
	t.Cleanup(func() {
		service.Close()
		api.Close()
	})
	return subagentFixture{workspace: workspace, sessionDir: sessionDir, apiURL: api.URL, provider: provider}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f subagentFixture) run(t *testing.T) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, f.apiURL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"ask the explorer"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("run status = %d, body=%s", response.StatusCode, body)
	}
	return string(body)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func TestHTTPRunHandsATaskToASubagentWithItsOwnModelAndContext(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "instructions": "You are the explorer.", "description": "Finds facts."})
	body := f.run(t)
	if !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}

	parent := f.provider.forModel("parent-model")
	child := f.provider.forModel("child-model")
	if len(parent) != 2 || len(child) != 2 {
		t.Fatalf("provider requests: parent %d (want 2), child %d (want 2)", len(parent), len(child))
	}

	// The parent sees the subagent tool, with the agent list in its description.
	if strings.Join(parent[0].Tools, ",") != "read,subagent" {
		t.Fatalf("parent tools = %v", parent[0].Tools)
	}
	// The child sees read and nothing else: no subagent, so it cannot start another child.
	if strings.Join(child[0].Tools, ",") != "read" {
		t.Fatalf("child tools = %v, want read only", child[0].Tools)
	}
	// The child's context is its instructions and the task, not the parent's conversation.
	if !strings.Contains(child[0].Text, "You are the explorer.") || !strings.Contains(child[0].Text, "find the answer") {
		t.Fatalf("child's first request lacks its instructions or task:\n%s", child[0].Text)
	}
	if strings.Contains(child[0].Text, "ask the explorer") {
		t.Fatalf("the child saw the parent's user message:\n%s", child[0].Text)
	}
	// The child's second round carries its own tool result (the file it read).
	if !strings.Contains(child[1].Text, "the secret is 42") {
		t.Fatalf("the child's tool result did not reach its second round:\n%s", child[1].Text)
	}
	// The parent gets the answer, a usage line, and none of the child's intermediate work.
	last := parent[1].Text
	for _, want := range []string{"the answer is 42", "agent: explore", "rounds: 2", "tokens: 32"} {
		if !strings.Contains(last, want) {
			t.Errorf("the parent's tool result lacks %q:\n%s", want, last)
		}
	}
	if strings.Contains(last, "the secret is 42") {
		t.Fatalf("the child's tool output leaked into the parent's context:\n%s", last)
	}
	if !strings.Contains(body, "parent saw:") {
		t.Fatalf("the parent's final message is missing from the stream:\n%s", body)
	}
}

func TestSubagentKeepsItsTranscriptApartFromTheSessionTranscript(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	f.run(t)

	// The session transcript holds the parent's records only: input, tool call, tool
	// output, final message. The child's work is not in it.
	lines := readLines(t, filepath.Join(f.sessionDir, "messages.jsonl"))
	if len(lines) != 4 {
		t.Fatalf("session messages.jsonl has %d records, want 4 (input, call, output, final):\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, line := range lines {
		if strings.Contains(line, "the secret is 42") && !strings.Contains(line, "function_call_output") {
			t.Fatalf("a child record is in the session transcript: %s", line)
		}
	}

	// The child's transcript is in the configured storage directory, one folder per call.
	folders, err := filepath.Glob(filepath.Join(f.workspace, ".subagents", "*", "messages.jsonl"))
	if err != nil || len(folders) != 1 {
		t.Fatalf("child transcripts = %v, %v; want exactly one under .subagents", folders, err)
	}
	if !strings.Contains(filepath.Base(filepath.Dir(folders[0])), "call-sub") {
		t.Fatalf("the child folder %q is not named after the parent's call", folders[0])
	}
	child := readLines(t, folders[0])
	if len(child) != 4 {
		t.Fatalf("child transcript has %d records, want 4 (task, read call, read output, answer)", len(child))
	}
	joined := strings.Join(child, "\n")
	if !strings.Contains(joined, "find the answer") || !strings.Contains(joined, "the answer is 42") {
		t.Fatalf("child transcript lacks its task or answer:\n%s", joined)
	}
}

func TestSubagentWithoutAModelFileUsesTheCallingSessionsModel(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{})
	f.run(t)
	// With no model of its own, the child asks the same model as the parent, so the
	// fake provider cannot tell it apart and answers the parent script. The point is
	// that the child ran at all, with the parent's model and a fresh context.
	all := f.provider.forModel("parent-model")
	var childRequests int
	for _, request := range all {
		if !strings.Contains(request.Text, "ask the explorer") {
			childRequests++
		}
	}
	if childRequests == 0 {
		t.Fatal("no request with a fresh context reached the parent's model, so the child did not run with it")
	}
}

func TestSubagentProviderFailureReachesTheParentAsAToolResultNotARunFailure(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	f.provider.childFails = http.StatusBadRequest // not retried by the client
	body := f.run(t)
	if !strings.Contains(body, "event: completed") {
		t.Fatalf("a failed child failed the whole run:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	if len(parent) != 2 {
		t.Fatalf("the parent made %d requests, want 2 (it must get the failure and go on)", len(parent))
	}
	for _, want := range []string{"error_code: subagent_failed", "Agent explore failed"} {
		if !strings.Contains(parent[1].Text, want) {
			t.Errorf("the parent's tool result lacks %q:\n%s", want, parent[1].Text)
		}
	}
}

func TestStoppingTheRunCancelsARunningSubagentPromptly(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	f.provider.childBlocks = true
	f.provider.childEntered = make(chan struct{}, 1)
	f.provider.childCancelled = make(chan struct{}, 1)
	f.provider.childRelease = make(chan struct{})
	t.Cleanup(func() { close(f.provider.childRelease) })

	finished := make(chan string, 1)
	go func() { finished <- f.run(t) }()

	select {
	case <-f.provider.childEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the child never reached the provider")
	}
	stopRequest, err := http.NewRequest(http.MethodPost, f.apiURL+"/api/sessions/local:demo/run/stop", nil)
	if err != nil {
		t.Fatal(err)
	}
	stopRequest.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	stopped := time.Now()
	response, err := http.DefaultClient.Do(stopRequest)
	if err != nil {
		t.Fatal(err)
	}
	stopBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(stopBody), `"stopped":true`) {
		t.Fatalf("stop answered %s", stopBody)
	}

	select {
	case <-f.provider.childCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the child's provider request was not cancelled after the run was stopped")
	}
	select {
	case body := <-finished:
		if strings.Contains(body, "event: completed") && strings.Contains(body, "parent saw:") {
			t.Fatalf("a stopped run carried on to a final answer:\n%s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end after it was stopped")
	}
	if elapsed := time.Since(stopped); elapsed > 4*time.Second {
		t.Fatalf("stopping took %v", elapsed)
	}
	if parent := f.provider.forModel("parent-model"); len(parent) != 1 {
		t.Fatalf("the parent made %d requests, want 1 (it must not continue after a stop)", len(parent))
	}
}

func TestSeveralSubagentCallsInOneResponseEachGetTheirOwnContextAndTranscript(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	f.provider.parentCalls = 3
	body := f.run(t)
	if !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}

	// Every child asks the provider on its own: three tasks, each seen by exactly one child
	// as its first request, and none of them containing another child's task.
	child := f.provider.forModel("child-model")
	if len(child) != 6 {
		t.Fatalf("child requests = %d, want 6 (read call and answer for each of 3 children)", len(child))
	}
	for index := range 3 {
		task := fmt.Sprintf("task number %d", index)
		holders := 0
		for _, request := range child {
			if strings.Contains(request.Text, task) {
				holders++
				for other := range 3 {
					if other != index && strings.Contains(request.Text, fmt.Sprintf("task number %d", other)) {
						t.Fatalf("a child saw two tasks (%d and %d):\n%s", index, other, request.Text)
					}
				}
			}
		}
		if holders != 2 {
			t.Errorf("%s reached the provider in %d requests, want 2 (its first round and its second)", task, holders)
		}
	}

	// One transcript folder per call, each named after its own call id.
	folders, err := filepath.Glob(filepath.Join(f.workspace, ".subagents", "*", "messages.jsonl"))
	if err != nil || len(folders) != 3 {
		t.Fatalf("child transcripts = %v, %v; want 3", folders, err)
	}
	seen := map[string]bool{}
	for _, path := range folders {
		seen[filepath.Base(filepath.Dir(path))[:len("call-sub-0")]] = true
	}
	for _, want := range []string{"call-sub-0", "call-sub-1", "call-sub-2"} {
		if !seen[want] {
			t.Errorf("no child folder for %s: %v", want, seen)
		}
	}

	// The parent got all three results back, matched to the right calls.
	parent := f.provider.forModel("parent-model")
	if len(parent) != 2 {
		t.Fatalf("parent requests = %d, want 2", len(parent))
	}
	for index := range 3 {
		if !strings.Contains(parent[1].Text, fmt.Sprintf("call-sub-%d", index)) {
			t.Errorf("the parent's second request has no result for call-sub-%d", index)
		}
	}
	if got := strings.Count(parent[1].Text, "the answer is 42"); got != 3 {
		t.Errorf("the parent received %d child answers, want 3", got)
	}
}
