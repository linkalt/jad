package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

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
	parentCalls int
	// childFirstCall, when set, is the (tool name, arguments) the child makes in its
	// first round instead of reading notes.txt.
	childFirstCall [2]string
	childEntered   chan struct{}
	childCancelled chan struct{}
	// detach scripts the parent to start the child with wait_ms 0, so the call returns
	// a run id at once, and then to collect the answer with that id. childHold keeps
	// the child's first request waiting until the parent has seen the handoff.
	detach       bool
	childHold    chan struct{}
	holdReleased sync.Once
	// childRelease ends a blocked child request at cleanup, so a run that was not
	// cancelled fails the test instead of hanging the server shutdown.
	childRelease chan struct{}
}

type providerRequest struct {
	Model string
	Tools []string
	Input []json.RawMessage
	Text  string
	// Descriptions maps a tool name to the description the model was shown.
	Descriptions map[string]string
	// Authorization is the header the client sent, so a test can tell which
	// credential reached which server.
	Authorization string
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
		record := providerRequest{Model: body.Model, Input: body.Input, Authorization: r.Header.Get("Authorization")}
		for _, tool := range body.Tools {
			var named struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			_ = json.Unmarshal(tool, &named)
			record.Tools = append(record.Tools, named.Name)
			if record.Descriptions == nil {
				record.Descriptions = map[string]string{}
			}
			record.Descriptions[named.Name] = named.Description
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

		if body.Model == "child-model" && p.childHold != nil {
			select {
			case <-p.childHold:
			case <-r.Context().Done():
				return
			}
		}

		if fail {
			http.Error(w, `{"error":{"message":"child provider down","type":"server_error"}}`, status)
			return
		}
		var events []string
		switch {
		case body.Model == "parent-model" && p.detach && !strings.Contains(record.Text, "function_call_output"):
			events = functionCallEvents("call-sub", "subagent", `{"task":"find the answer","wait_ms":0}`)
		case body.Model == "parent-model" && p.detach && !strings.Contains(record.Text, "the answer is 42"):
			// The handoff came back. Let the child go, then collect with the id from it.
			id := regexp.MustCompile(`sub_[0-9a-f]{8}`).FindString(lastOutput(record.Text))
			p.holdReleased.Do(func() { close(p.childHold) })
			events = functionCallEvents("call-collect", "subagent", fmt.Sprintf(`{"id":%q,"wait_ms":10000}`, id))
		case body.Model == "parent-model" && !strings.Contains(record.Text, "function_call_output") && p.parentCalls > 1:
			events = manyCallEvents(p.parentCalls)
		case body.Model == "parent-model" && !strings.Contains(record.Text, "function_call_output"):
			events = functionCallEvents("call-sub", "subagent", `{"task":"find the answer"}`)
		case body.Model == "parent-model":
			events = messageEvents("parent saw: "+lastOutput(record.Text), 31)
		case !strings.Contains(record.Text, "function_call_output"):
			// A child's first round: its own history holds no tool result yet. Judging by the
			// request's own history, not a global count, keeps several children apart.
			if p.childFirstCall[0] != "" {
				events = functionCallEvents("call-first", p.childFirstCall[0], p.childFirstCall[1])
			} else {
				events = functionCallEvents("call-read", "read", `{"path":"notes.txt"}`)
			}
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
	// childProvider is the child's own server when the fixture was built with
	// newSplitSubagentFixture; it is nil when both models share provider.
	childProvider *subagentProvider
}

const (
	parentProviderKey = "parent-provider-key"
	childProviderKey  = "child-provider-key"
)

// newSubagentFixture builds a session with the read tool and the subagent tool, and a
// parent model and a child model that both point at the fake provider.
func newSubagentFixture(t *testing.T, agentSetting map[string]any) subagentFixture {
	t.Helper()
	return buildSubagentFixture(t, agentSetting, false, nil)
}

// newSubagentFixtureWithFiles is the same session with extra files written into the
// workspace before the configuration loads, for settings that read files.
func newSubagentFixtureWithFiles(t *testing.T, agentSetting map[string]any, files map[string]string) subagentFixture {
	t.Helper()
	return buildSubagentFixture(t, agentSetting, false, files)
}

// newSplitSubagentFixture is the same session, but the child's model file names a
// second server and a second key variable, so a test can see whether the child used
// its own provider or inherited the parent's.
func newSplitSubagentFixture(t *testing.T, agentSetting map[string]any) subagentFixture {
	t.Helper()
	return buildSubagentFixture(t, agentSetting, true, nil)
}

// mcpFixture adds one in-process MCP server with a "lookup" tool to the session.
type mcpFixture struct {
	approval string
	calls    *atomic.Int32
}

func buildSubagentFixture(t *testing.T, agentSetting map[string]any, splitChild bool, files map[string]string) subagentFixture {
	return buildSubagentFixtureWithMCP(t, agentSetting, splitChild, files, nil)
}

func buildSubagentFixtureWithMCP(t *testing.T, agentSetting map[string]any, splitChild bool, files map[string]string, mcp *mcpFixture) subagentFixture {
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
	for name, text := range files {
		writeFile(t, filepath.Join(workspace, name), []byte(text))
	}
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("the secret is 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "setting.json"), []byte(`{"session_roots":[{"id":"local","path":"sessions"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mcpServers := []any{}
	if mcp != nil {
		mcpServer := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "fixture", Version: "1.0.0"}, nil)
		mcpServer.AddTool(&mcpSDK.Tool{
			Name:        "lookup",
			Description: "looks a word up",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"word":{"type":"string"}},"required":["word"],"additionalProperties":false}`),
		}, func(_ context.Context, request *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
			mcp.calls.Add(1)
			var arguments map[string]string
			_ = json.Unmarshal(request.Params.Arguments, &arguments)
			return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "MCP says " + arguments["word"] + " is 99"}}}, nil
		})
		mcpHTTP := httptest.NewServer(mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return mcpServer }, nil))
		t.Cleanup(mcpHTTP.Close)
		mcpConfig, _ := json.Marshal(map[string]any{
			"transport": map[string]any{"type": "streamable_http", "url": mcpHTTP.URL}, "timeout_ms": 10_000, "max_result_bytes": 4096,
		})
		mcpPath := filepath.Join(workspace, "mcp-server.json")
		writeFile(t, mcpPath, mcpConfig)
		entry := map[string]any{"id": "fixture", "config_path": mcpPath, "enabled": true, "deny_tools": []string{}}
		if mcp.approval != "" {
			entry["approval"] = mcp.approval
		}
		mcpServers = []any{entry}
	}
	subagent := map[string]any{"agents": map[string]any{"explore": agentSetting}}
	if budget, ok := agentSetting["__list_max_bytes"]; ok {
		// The test sets the description budget; the agent itself keeps its other settings.
		delete(agentSetting, "__list_max_bytes")
		subagent["list_max_bytes"] = budget
	}
	if dir, ok := agentSetting["__agents_dir"].(string); ok {
		// The test supplies the agents as folders; nothing is defined inline.
		subagent = map[string]any{"agents_dir": []string{dir}}
	}
	setting, err := json.Marshal(map[string]any{
		"cwd": workspace, "max_tool_rounds": 4,
		"tool_definition_paths": []string{"read.json", "subagent.json"},
		"mcp_servers":           mcpServers,
		"tools": map[string]any{"native": map[string]any{
			"read":     map[string]any{"paths": map[string]any{"allow": []string{workspace}, "deny": []string{}}},
			"subagent": subagent,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sessionDir, "setting.json"), setting)
	environment := map[string]string{"AGENT_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"}
	parentModel := map[string]any{"provider": "openai", "model": "parent-model", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": server.URL + "/v1"}
	childModel := map[string]any{"provider": "openai", "model": "child-model", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": server.URL + "/v1"}
	var childProvider *subagentProvider
	if splitChild {
		// A second server, a second provider name and a second key variable: nothing in
		// the child's file matches the parent's, so any inherited value shows up.
		childProvider = &subagentProvider{}
		childServer := httptest.NewServer(childProvider.handler(t))
		t.Cleanup(childServer.Close)
		parentModel["api_key_env"] = "PARENT_PROVIDER_KEY"
		childModel["provider"] = "other-vendor"
		childModel["api_key_env"] = "CHILD_PROVIDER_KEY"
		childModel["base_url"] = childServer.URL + "/v1"
		environment["PARENT_PROVIDER_KEY"] = parentProviderKey
		environment["CHILD_PROVIDER_KEY"] = childProviderKey
	}
	for file, model := range map[string]map[string]any{"model.json": parentModel, "child.json": childModel} {
		body, _ := json.Marshal(model)
		writeFile(t, filepath.Join(sessionDir, file), body)
		if dir, ok := agentSetting["__agents_dir"].(string); ok && file == "child.json" {
			// An agent folder names its model relative to itself.
			writeFile(t, filepath.Join(workspace, dir, "explore", file), body)
		}
	}

	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir:  workspace,
		Environment: environment,
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
	return subagentFixture{workspace: workspace, sessionDir: sessionDir, apiURL: api.URL, provider: provider, childProvider: childProvider}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
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

func TestSubagentUsesItsOwnProviderServerAndKeyNotTheParents(t *testing.T) {
	f := newSplitSubagentFixture(t, map[string]any{"model": "child.json"})
	body := f.run(t)
	if !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}

	parent := f.provider.requests
	child := f.childProvider.requests
	// The parent's server saw the parent's two rounds and nothing else; the child's
	// server saw the child's two rounds and nothing else.
	if len(parent) != 2 || len(child) != 2 {
		t.Fatalf("requests: parent server %d (want 2), child server %d (want 2)", len(parent), len(child))
	}
	for _, request := range parent {
		if request.Model != "parent-model" || request.Authorization != "Bearer "+parentProviderKey {
			t.Errorf("parent server got model %q with %q, want parent-model with the parent key", request.Model, request.Authorization)
		}
	}
	for _, request := range child {
		if request.Model != "child-model" || request.Authorization != "Bearer "+childProviderKey {
			t.Errorf("child server got model %q with %q, want child-model with the child key", request.Model, request.Authorization)
		}
	}
	// A key never crosses over: the parent's key is not on the child's server and the
	// child's key is not on the parent's.
	for _, request := range child {
		if strings.Contains(request.Authorization, parentProviderKey) {
			t.Fatalf("the parent's key reached the child's server")
		}
	}
	for _, request := range parent {
		if strings.Contains(request.Authorization, childProviderKey) {
			t.Fatalf("the child's key reached the parent's server")
		}
	}
}

func TestSubagentPromptIsItsInstructionsPlusDocumentsAndNeverTheParents(t *testing.T) {
	f := newSubagentFixtureWithFiles(t,
		map[string]any{"model": "child.json", "instructions": "You are the explorer.", "prompt_documents": []string{"child-rules.md"}},
		map[string]string{"child-rules.md": "CHILD RULE: report file paths only."})
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	child := f.provider.forModel("child-model")
	if len(parent) != 2 || len(child) != 2 {
		t.Fatalf("provider requests: parent %d (want 2), child %d (want 2)", len(parent), len(child))
	}
	// Both texts are in the child's request, in order, in one prompt item.
	first := strings.Index(child[0].Text, "You are the explorer.")
	second := strings.Index(child[0].Text, "CHILD RULE: report file paths only.")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("the child's request lacks its instructions then its document (at %d, %d):\n%s", first, second, child[0].Text)
	}
	if !strings.Contains(child[0].Text, "You are the explorer.\\n\\nCHILD RULE") {
		t.Fatalf("instructions and document are not joined by a blank line:\n%s", child[0].Text)
	}
	// The child's document is not in the parent's context, and the child's second
	// round still carries it (the prompt is part of every request).
	for _, request := range parent {
		if strings.Contains(request.Text, "CHILD RULE") || strings.Contains(request.Text, "You are the explorer.") {
			t.Fatalf("the child's prompt leaked into a parent request:\n%s", request.Text)
		}
	}
	if !strings.Contains(child[1].Text, "CHILD RULE: report file paths only.") {
		t.Fatalf("the child's second round lost its prompt:\n%s", child[1].Text)
	}
}

func TestSubagentLoadedFromAFolderRunsWithTheFoldersModelAndPrompt(t *testing.T) {
	// The folder's model file is found next to agent.json, and its prompt is
	// prompt.md followed by the listed document; the parent's models and prompt are not used.
	f := newSubagentFixtureWithFiles(t, map[string]any{"__agents_dir": "agent-files"}, map[string]string{
		"agent-files/explore/agent.json": `{"description":"Finds facts.","model":"child.json","prompt_documents":["format.md"]}`,
		"agent-files/explore/prompt.md":  "FOLDER PROMPT: you are the explorer.",
		"agent-files/explore/format.md":  "FOLDER FORMAT: answer in one line.",
	})

	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	child := f.provider.forModel("child-model")
	if len(parent) != 2 || len(child) != 2 {
		t.Fatalf("provider requests: parent %d (want 2), child %d (want 2)", len(parent), len(child))
	}
	if strings.Join(child[0].Tools, ",") != "read" {
		t.Fatalf("child tools = %v, want read only", child[0].Tools)
	}
	prompt := child[0].Text
	first := strings.Index(prompt, "FOLDER PROMPT: you are the explorer.")
	second := strings.Index(prompt, "FOLDER FORMAT: answer in one line.")
	if first < 0 || second < first {
		t.Fatalf("the child's request lacks prompt.md then format.md (at %d, %d):\n%s", first, second, prompt)
	}
	for _, request := range parent {
		if strings.Contains(request.Text, "FOLDER PROMPT") || strings.Contains(request.Text, "FOLDER FORMAT") {
			t.Fatalf("the folder's prompt leaked into a parent request:\n%s", request.Text)
		}
	}
	// The agent list shown to the model comes from the folder's agent.json.
	if description := parent[0].Descriptions["subagent"]; !strings.Contains(description, "\n- explore: Finds facts.") {
		t.Fatalf("the tool description lacks the folder agent: %q", description)
	}
}

func TestSubagentCallsAnMCPToolItsListNamesThroughTheSharedConnection(t *testing.T) {
	calls := &atomic.Int32{}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"read", "mcp:fixture:lookup"}}, false, nil, &mcpFixture{calls: calls})
	f.provider.childFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"answer"}`}

	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	child := f.provider.forModel("child-model")
	if len(parent) != 2 || len(child) != 2 {
		t.Fatalf("provider requests: parent %d (want 2), child %d (want 2)", len(parent), len(child))
	}
	// The parent sees its own tool list; the MCP tool is not hidden from it.
	if !strings.Contains(strings.Join(parent[0].Tools, ","), mcpToolNameFor("fixture", "lookup")) {
		t.Fatalf("parent tools = %v", parent[0].Tools)
	}
	// The child sees read and the one MCP tool its list named, and no subagent tool.
	if strings.Join(child[0].Tools, ",") != "read,"+mcpToolNameFor("fixture", "lookup") {
		t.Fatalf("child tools = %v", child[0].Tools)
	}
	if calls.Load() != 1 {
		t.Fatalf("the MCP server saw %d calls, want 1", calls.Load())
	}
	// The child's second round carries what the MCP server returned.
	if !strings.Contains(child[1].Text, "MCP says answer is 99") {
		t.Fatalf("the MCP result did not reach the child's second round:\n%s", child[1].Text)
	}
}

func TestSubagentWithoutAnMCPListGetsNoMCPTool(t *testing.T) {
	calls := &atomic.Int32{}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json"}, false, nil, &mcpFixture{calls: calls})
	f.run(t)
	child := f.provider.forModel("child-model")
	if len(child) == 0 || strings.Join(child[0].Tools, ",") != "read" {
		t.Fatalf("child tools = %v, want read only", child)
	}
	if calls.Load() != 0 {
		t.Fatalf("the MCP server saw %d calls", calls.Load())
	}
}

// sessionToolsStatus asks for the session's tool list, which builds the catalog the way
// a run does and reports a configuration error with its real text.
func (f subagentFixture) sessionTools(t *testing.T) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, f.apiURL+"/api/sessions/local:demo/tools", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return response.StatusCode, string(body)
}

func TestSubagentMCPToolFromAServerThatAsksIsRefusedWhenTheSessionLoads(t *testing.T) {
	calls := &atomic.Int32{}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"mcp:fixture:lookup"}}, false, nil, &mcpFixture{approval: "ask", calls: calls})
	status, body := f.sessionTools(t)
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "whose server asks for approval") {
		t.Fatalf("status %d, body %s; want 422 naming the approval problem", status, body)
	}
	if calls.Load() != 0 || len(f.provider.requests) != 0 {
		t.Fatalf("a refused session still ran: %d MCP calls, %d provider requests", calls.Load(), len(f.provider.requests))
	}
}

func TestSubagentNamingAnMCPToolThatDoesNotExistFailsTheSessionWithTheLoadedList(t *testing.T) {
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"mcp:fixture:nope"}}, false, nil, &mcpFixture{calls: &atomic.Int32{}})
	status, body := f.sessionTools(t)
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "which is not a loaded MCP tool (loaded: mcp:fixture:lookup)") {
		t.Fatalf("status %d, body %s", status, body)
	}
}

// mcpToolNameFor is the name the model sees for an MCP tool: the harness derives it
// from the server and tool name and appends a short hash.
func mcpToolNameFor(server, tool string) string {
	sum := sha256.Sum256([]byte(server + "\x00" + tool))
	return "mcp_" + server + "_" + tool + "_" + hex.EncodeToString(sum[:])[:10]
}

func TestSubagentMasterRequestCarriesOnlyACountWhenTheListDoesNotFit(t *testing.T) {
	descriptionOf := func(budget any) string {
		f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts in the code and reports file paths.", "__list_max_bytes": budget})
		f.run(t)
		parent := f.provider.forModel("parent-model")
		if len(parent) == 0 {
			t.Fatal("the parent made no request")
		}
		return parent[0].Descriptions["subagent"]
	}
	// A budget that holds the full list: unchanged behaviour.
	if d := descriptionOf(4096); !strings.Contains(d, "\nAgents:\n- explore: Finds facts in the code and reports file paths.") {
		t.Fatalf("full list missing:\n%s", d)
	}
	// A budget of zero: only the count and how to ask, and no agent description reaches the model.
	d := descriptionOf(0)
	if strings.Contains(d, "Finds facts") || strings.Contains(d, "explore") || !strings.Contains(d, "1 agent configured. Call without a task to list them with descriptions.") {
		t.Fatalf("zero budget still listed agents:\n%s", d)
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

func TestHTTPRunSlowSubagentHandsBackAnIdAndTheLaterCallCollectsTheAnswer(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	f.provider.detach = true
	f.provider.childHold = make(chan struct{})
	t.Cleanup(func() { f.provider.holdReleased.Do(func() { close(f.provider.childHold) }) })

	body := f.run(t)
	if !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	if len(parent) != 3 {
		t.Fatalf("parent requests = %d, want 3 (task, collect, final answer)", len(parent))
	}
	if !strings.Contains(parent[1].Text, "still running") || !strings.Contains(parent[1].Text, "Do not start the task again") {
		t.Errorf("the parent's second request lacks the handoff text:\n%s", parent[1].Text)
	}
	if strings.Contains(parent[1].Text, "error_code") {
		t.Errorf("the handoff was reported as an error:\n%s", parent[1].Text)
	}
	if !strings.Contains(parent[2].Text, "Agent explore finished") || !strings.Contains(parent[2].Text, "the answer is 42") {
		t.Errorf("the collected answer did not reach the parent:\n%s", parent[2].Text)
	}
	// The collect call used the first child; it did not start another one.
	if child := f.provider.forModel("child-model"); len(child) != 2 {
		t.Errorf("child requests = %d, want 2 (one child: read, then answer)", len(child))
	}
	folders, err := filepath.Glob(filepath.Join(f.workspace, ".subagents", "*", "messages.jsonl"))
	if err != nil || len(folders) != 1 {
		t.Errorf("child transcripts = %v, %v; want exactly 1", folders, err)
	}
}
