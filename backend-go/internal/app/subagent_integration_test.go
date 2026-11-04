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

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
	"jad/backend-go/internal/tools"
)

// subagentProvider is a fake Responses endpoint. It tells the parent run and the child
// run apart by the model name in the request, and records what each of them sent.
type subagentProvider struct {
	mu       sync.Mutex
	requests []providerRequest
	// childFails makes every child request answer with this HTTP status when non-zero.
	childFails int
	// childFailFirst makes the first this-many child requests answer 503; later ones work.
	childFailFirst int
	// childBlocks makes every child request wait until the client goes away. The
	// channels are created by the test before the run starts.
	childBlocks bool
	// parentCalls is how many subagent calls the parent makes in its first response;
	// zero means one.
	parentCalls int
	// childFirstCall, when set, is the (tool name, arguments) the child makes in its
	// first round instead of reading notes.txt.
	childFirstCall [2]string
	// parentFirstCall, when set, is the (tool name, arguments) the parent makes in its
	// first round instead of starting a subagent.
	parentFirstCall [2]string
	// parentSteps, when set, scripts the parent's calls in order: round N makes the Nth step
	// (the results its history already holds count the rounds), and a text answer follows.
	parentSteps []subagentStep
	// childWritesByTask makes each child write its own file, w<N>.txt, where N is the
	// number in its task ("task number N"), so several children never share a path.
	childWritesByTask bool
	childEntered      chan struct{}
	childCancelled    chan struct{}
	// childRounds makes the child ask for notes.txt in its first childRounds requests,
	// whatever its history holds, and answer in the next one. childInputTokens is the
	// input size every child response reports, which is what a compaction trigger reads.
	childRounds      int
	childInputTokens int
	childRequests    int
	// detach scripts the parent to start the child with block_ms 0, so the call returns
	// a run id at once, and then to collect the answer with that id. childHold keeps
	// the child's first request waiting until the parent has seen the handoff.
	detach bool
	// handoffOnly scripts the parent to start the child with block_ms 0 and then end its
	// run with a final answer, leaving the child going.
	handoffOnly  bool
	childHold    chan struct{}
	holdReleased sync.Once
	// childPartial scripts the child to write some text with its first tool call and
	// then hang in its second round, so a timeout cuts it off with partial work.
	childPartial bool
	// collectMarker scripts a parent whose input holds this text to collect the run named
	// in collectID with one subagent call and then report what it got.
	collectMarker string
	collectID     string
	// childRelease ends a blocked child request at cleanup, so a run that was not
	// cancelled fails the test instead of hanging the server shutdown.
	childRelease chan struct{}
}

// subagentStep is one scripted call of the parent's rounds (see parentSteps).
type subagentStep struct {
	name, arguments string
}

type providerRequest struct {
	Model string
	Tools []string
	Input []json.RawMessage
	Text  string
	// Descriptions maps a tool name to the description the model was shown.
	Descriptions map[string]string
	// Parameters maps a tool name to the JSON Schema the model was shown in its parameters.
	Parameters map[string]json.RawMessage
	// Authorization is the header the client sent, so a test can tell which
	// credential reached which server.
	Authorization string
	// ToolTypes lists the type of every tool in the request, function or hosted.
	ToolTypes []string
	// PromptCacheKey is the request's prompt_cache_key, "" when it has none.
	PromptCacheKey string
}

func (p *subagentProvider) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected provider request", http.StatusNotFound)
			return
		}
		var body struct {
			Model          string            `json:"model"`
			Tools          []json.RawMessage `json:"tools"`
			Input          []json.RawMessage `json:"input"`
			PromptCacheKey string            `json:"prompt_cache_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid Responses request", http.StatusBadRequest)
			return
		}
		record := providerRequest{Model: body.Model, Input: body.Input, Authorization: r.Header.Get("Authorization"), PromptCacheKey: body.PromptCacheKey}
		for _, tool := range body.Tools {
			var named struct {
				Type        string          `json:"type"`
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			}
			_ = json.Unmarshal(tool, &named)
			record.ToolTypes = append(record.ToolTypes, named.Type)
			record.Tools = append(record.Tools, named.Name)
			if record.Descriptions == nil {
				record.Descriptions = map[string]string{}
			}
			record.Descriptions[named.Name] = named.Description
			if record.Parameters == nil {
				record.Parameters = map[string]json.RawMessage{}
			}
			record.Parameters[named.Name] = named.Parameters
		}
		for _, item := range body.Input {
			record.Text += string(item) + "\n"
		}
		p.mu.Lock()
		p.requests = append(p.requests, record)
		fail := body.Model == "child-model" && p.childFails != 0
		status := p.childFails
		childRequest := 0
		if body.Model == "child-model" {
			p.childRequests++
			childRequest = p.childRequests
			if childRequest <= p.childFailFirst {
				fail, status = true, http.StatusServiceUnavailable
			}
		}
		block := body.Model == "child-model" && (p.childBlocks || (p.childPartial && strings.Contains(record.Text, "function_call_output")))
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
		case body.Model == "parent-model" && p.collectMarker != "" && strings.Contains(record.Text, p.collectMarker):
			after := record.Text[strings.LastIndex(record.Text, p.collectMarker):]
			if strings.Contains(after, "function_call_output") {
				events = messageEvents("parent saw: "+lastOutput(after), 31)
			} else {
				events = functionCallEvents("call-collect", "async", fmt.Sprintf(`{"action":"status","id":%q}`, p.collectID))
			}
		case body.Model == "parent-model" && p.handoffOnly && !strings.Contains(record.Text, "function_call_output"):
			events = functionCallEvents("call-sub", "subagent", `{"task":"find the answer","block_ms":0}`)
		case body.Model == "parent-model" && p.detach && !strings.Contains(record.Text, "function_call_output"):
			events = functionCallEvents("call-sub", "subagent", `{"task":"find the answer","block_ms":0}`)
		case body.Model == "parent-model" && p.detach && !strings.Contains(record.Text, "the answer is 42"):
			// The handoff came back. Let the child go, then collect with the id from it.
			p.holdReleased.Do(func() { close(p.childHold) })
			events = functionCallEvents("call-collect", "async", `{"action":"wait","wait_ms":10000}`)
		case body.Model == "parent-model" && len(p.parentSteps) > 0:
			if results := strings.Count(record.Text, `"function_call_output"`); results < len(p.parentSteps) {
				step := p.parentSteps[results]
				events = functionCallEvents(fmt.Sprintf("call-step-%d", results+1), step.name, step.arguments)
			} else {
				events = messageEvents("parent done", 9)
			}
		case body.Model == "parent-model" && p.parentFirstCall[0] != "" && !strings.Contains(record.Text, "function_call_output"):
			events = functionCallEvents("call-parent-first", p.parentFirstCall[0], p.parentFirstCall[1])
		case body.Model == "parent-model" && !strings.Contains(record.Text, "function_call_output") && p.parentCalls > 1:
			events = manyCallEvents(p.parentCalls)
		case body.Model == "parent-model" && !strings.Contains(record.Text, "function_call_output"):
			events = functionCallEvents("call-sub", "subagent", `{"task":"find the answer"}`)
		case body.Model == "parent-model":
			events = messageEvents("parent saw: "+lastOutput(record.Text), 31)
		case body.Model == "child-model" && p.childRounds > 0 && childRequest <= p.childRounds:
			events = functionCallEvents(fmt.Sprintf("call-read-%d", childRequest), "read", `{"path":"notes.txt"}`)
			events[len(events)-1] = strings.Replace(events[len(events)-1], `"id":"resp-call"`, fmt.Sprintf(`"id":"resp-child-%d"`, childRequest), 1)
		case !strings.Contains(record.Text, "function_call_output"):
			// A child's first round: its own history holds no tool result yet. Judging by the
			// request's own history, not a global count, keeps several children apart.
			if p.childPartial {
				events = textThenCallEvents("checking notes.txt first", "call-read", "read", `{"path":"notes.txt"}`)
			} else if p.childWritesByTask {
				number := regexp.MustCompile(`task number (\d+)`).FindStringSubmatch(record.Text)
				index := "x"
				if number != nil {
					index = number[1]
				}
				events = functionCallEvents("call-first", "write", fmt.Sprintf(`{"path":"w%s.txt","content":"child %s"}`, index, index))
			} else if p.childFirstCall[0] != "" {
				events = functionCallEvents("call-first", p.childFirstCall[0], p.childFirstCall[1])
			} else {
				events = functionCallEvents("call-read", "read", `{"path":"notes.txt"}`)
			}
		default:
			events = messageEvents("the answer is 42", 17)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			if body.Model == "child-model" && p.childInputTokens > 0 {
				event = strings.Replace(event, `"input_tokens":10,`, fmt.Sprintf(`"input_tokens":%d,`, p.childInputTokens), 1)
				event = strings.Replace(event, `"input_tokens":16,`, fmt.Sprintf(`"input_tokens":%d,`, p.childInputTokens), 1)
			}
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

// textThenCallEvents is one response that holds an assistant message and a tool call,
// in the order a model writes them.
func textThenCallEvents(text, callID, name, arguments string) []string {
	encodedText, _ := json.Marshal(text)
	message := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + string(encodedText) + `}]}`
	encodedArguments, _ := json.Marshal(arguments)
	call := fmt.Sprintf(`{"type":"function_call","call_id":%q,"name":%q,"arguments":%s}`, callID, name, encodedArguments)
	return []string{
		`{"type":"response.output_item.done","output_index":0,"item":` + message + `}`,
		`{"type":"response.output_item.done","output_index":1,"item":` + call + `}`,
		`{"type":"response.completed","response":{"id":"resp-call","status":"completed","output":[` + message + `,` + call + `],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
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
	workspace   string
	sessionFile string
	// sessionDir is the session's state folder, the base of a relative image_store.dir.
	sessionDir string
	// stateDir holds the profile's messages.jsonl and staging; toolDir its tool storage
	// (.subagents, ...); modelsDir the model files; config is the loaded server config.
	stateDir  string
	toolDir   string
	modelsDir string
	config    Config
	apiURL    string
	provider  *subagentProvider
	// childProvider is the child's own server when the fixture was built with
	// newSplitSubagentFixture; it is nil when both models share provider.
	childProvider *subagentProvider
	// restart stops the service, runs between (when not nil) while nothing is running,
	// and starts a new service over the same directories, as a server restart does. It
	// returns the new API address.
	restart func(t *testing.T, between func()) string
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
	// onlyFile writes the server file but does not attach it to the session's toolset;
	// a test attaches it through the toolset of an agent (__toolsets).
	onlyFile bool
	calls    *atomic.Int32
	// image, when set, is returned as an image block beside the text.
	image []byte
	// extra names more tools the server offers, each answering like lookup.
	extra []string
}

// subagentSampleTool returns the absolute path of one sample native tool definition.
func subagentSampleTool(t *testing.T, file string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "sample", "tools", "native", file))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func buildSubagentFixture(t *testing.T, agentSetting map[string]any, splitChild bool, files map[string]string) subagentFixture {
	return buildSubagentFixtureWithMCP(t, agentSetting, splitChild, files, nil)
}

func buildSubagentFixtureWithMCP(t *testing.T, agentSetting map[string]any, splitChild bool, files map[string]string, mcp *mcpFixture) subagentFixture {
	t.Helper()
	workspace := t.TempDir()
	provider := &subagentProvider{}
	server := httptest.NewServer(provider.handler(t))
	t.Cleanup(server.Close)

	sampleTool := func(file string) string { return subagentSampleTool(t, file) }
	definitions := []string{sampleTool("read.json"), sampleTool("subagent.json"), sampleTool("async.json")}
	writeSettings, withWrite := agentSetting["__write"].(map[string]any)
	if withWrite {
		definitions = append(definitions, sampleTool("write.json"))
		delete(agentSetting, "__write")
	}
	bashSettings, withBash := agentSetting["__bash"].(map[string]any)
	if withBash {
		definitions = append(definitions, sampleTool("bash.json"))
		delete(agentSetting, "__bash")
	}
	checkpointJSON, withCheckpoint := agentSetting["__checkpoint"].(string)
	delete(agentSetting, "__checkpoint")
	// __leaf_budget, when set, replaces the process budget with one of that size, so a test
	// can run the nested leaf calls through a budget of one.
	leafBudgetSize, withLeafBudget := agentSetting["__leaf_budget"].(int)
	delete(agentSetting, "__leaf_budget")
	sessionExtra, _ := agentSetting["__session"].(map[string]any)
	delete(agentSetting, "__session")
	// __load_tool enables tools.native.load_tool and its definition, which a profile with
	// tools.lazy needs: the profile is refused at load without it.
	_, withLoadTool := agentSetting["__load_tool"]
	delete(agentSetting, "__load_tool")
	if withLoadTool {
		definitions = append(definitions, sampleTool("load_tool.json"))
	}
	// __toolsets writes extra toolset files (id -> file content) and __session_without
	// removes tools from the session's own toolset, so a test can give a tool to an
	// agent's toolset only.
	extraToolsets, _ := agentSetting["__toolsets"].(map[string]any)
	delete(agentSetting, "__toolsets")
	sessionWithout, _ := agentSetting["__session_without"].([]string)
	delete(agentSetting, "__session_without")
	// __child_model adds fields to the child's model file (for example hosted_tools).
	childModelExtra, _ := agentSetting["__child_model"].(map[string]any)
	delete(agentSetting, "__child_model")
	for name, text := range files {
		writeFile(t, filepath.Join(workspace, name), []byte(text))
	}
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("the secret is 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if agentSetting["model"] == "child.json" {
		agentSetting["model"] = "openai/child"
	}

	spec := layouttest.Spec{SessionID: "demo", CWD: workspace, ToolDefinitionPaths: definitions, ProfileModels: []string{"openai/parent"}}
	// __profile_models replaces the models the profile lists, which a call's model may pick.
	if models, ok := agentSetting["__profile_models"].([]string); ok {
		spec.ProfileModels = models
		delete(agentSetting, "__profile_models")
	}
	var mcpServerFile map[string]any
	if mcp != nil {
		mcpServer := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "fixture", Version: "1.0.0"}, nil)
		for _, name := range append([]string{"lookup"}, mcp.extra...) {
			mcpServer.AddTool(&mcpSDK.Tool{
				Name:        name,
				Description: "looks a word up",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"word":{"type":"string"}},"required":["word"],"additionalProperties":false}`),
			}, func(_ context.Context, request *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
				mcp.calls.Add(1)
				var arguments map[string]string
				_ = json.Unmarshal(request.Params.Arguments, &arguments)
				content := []mcpSDK.Content{&mcpSDK.TextContent{Text: "MCP says " + arguments["word"] + " is 99"}}
				if mcp.image != nil {
					content = append(content, &mcpSDK.ImageContent{MIMEType: "image/png", Data: mcp.image})
				}
				return &mcpSDK.CallToolResult{Content: content}, nil
			})
		}
		mcpHTTP := httptest.NewServer(mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return mcpServer }, nil))
		t.Cleanup(mcpHTTP.Close)
		use := map[string]any{"deny_tools": []string{}}
		serverFile := map[string]any{
			"schema_version": 1,
			"transport":      map[string]any{"type": "streamable_http", "url": mcpHTTP.URL}, "timeout_ms": 10_000, "max_result_bytes": 4096 + 8*len(mcp.image),
		}
		if mcp.onlyFile {
			mcpServerFile = serverFile
		} else {
			spec.MCP = map[string]map[string]any{"m/fixture": use}
			spec.MCPConfig = map[string]map[string]any{"m/fixture": serverFile}
		}
	}
	subagent := map[string]any{"agents": map[string]any{"explore": agentSetting}}
	if budget, ok := agentSetting["__list_max_bytes"]; ok {
		// The test sets the description budget; the agent itself keeps its other settings.
		delete(agentSetting, "__list_max_bytes")
		subagent["list_max_bytes"] = budget
	}
	if extra, ok := agentSetting["__subagent_settings"].(map[string]any); ok {
		// The test sets tools.native.subagent keys beside the agents, such as persist_runs.
		delete(agentSetting, "__subagent_settings")
		for key, value := range extra {
			subagent[key] = value
		}
	}
	if dir, ok := agentSetting["__agents_dir"].(string); ok {
		// The test supplies the agents as folders; nothing is defined inline.
		subagent = map[string]any{"agents_dir": []string{dir}}
	}
	spec.Native = map[string]any{
		"read":     map[string]any{"paths": map[string]any{"allow": []string{workspace}, "deny": []string{}}},
		"subagent": subagent,
		"async":    map[string]any{},
	}
	if withLoadTool {
		spec.Native["load_tool"] = map[string]any{}
	}
	if withWrite {
		writeTool := map[string]any{"paths": map[string]any{"allow": []string{workspace}, "deny": []string{}}}
		for key, value := range writeSettings {
			writeTool[key] = value
		}
		spec.Native["write"] = writeTool
	}
	if withBash {
		spec.Native["bash"] = bashSettings
	}
	for _, name := range sessionWithout {
		delete(spec.Native, name)
	}
	spec.Settings = map[string]any{"max_tool_rounds": 4}
	if withCheckpoint {
		spec.Settings["checkpoint"] = json.RawMessage(checkpointJSON)
	}
	for key, value := range sessionExtra {
		if key == "__profile_extra" {
			spec.ProfileExtra = value.(map[string]any)
			continue
		}
		spec.Settings[key] = value
	}

	environment := map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"}
	parentModel := map[string]any{"provider": "openai", "model": "parent-model", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": server.URL + "/v1"}
	childModel := map[string]any{"provider": "openai", "model": "child-model", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": server.URL + "/v1"}
	for key, value := range childModelExtra {
		childModel[key] = value
	}
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
	spec.Models = map[string]map[string]any{"openai/parent": parentModel, "openai/child": childModel}
	layout := layouttest.Write(t, spec)
	if mcpServerFile != nil {
		body, _ := json.Marshal(mcpServerFile)
		writeFile(t, filepath.Join(layout.Root, "mcp", "m", "fixture.json"), body)
	}
	for id, content := range extraToolsets {
		body, _ := json.Marshal(content)
		writeFile(t, filepath.Join(layout.Root, "tools", filepath.FromSlash(id)+".json"), body)
	}
	if dir, ok := agentSetting["__agents_dir"].(string); ok {
		// An agent folder names its model relative to itself.
		body, _ := json.Marshal(childModel)
		writeFile(t, filepath.Join(workspace, dir, "explore", "child.json"), body)
	}

	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir:      layout.Root,
		RootSettingPath: layout.RootSetting,
		Environment:     environment,
	})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	var service *Service
	var api *httptest.Server
	start := func(t *testing.T) string {
		t.Helper()
		var err error
		if withLeafBudget {
			service, err = newServiceWithBudget(config, tools.NewLeafBudget(leafBudgetSize))
		} else {
			service, err = NewService(config)
		}
		if err != nil {
			t.Fatalf("NewService() error = %v", err)
		}
		api = httptest.NewServer(service.Handler)
		return api.URL
	}
	stop := func() {
		if service != nil {
			service.Close()
			api.Close()
			service = nil
		}
	}
	apiURL := start(t)
	t.Cleanup(stop)
	restart := func(t *testing.T, between func()) string {
		t.Helper()
		stop()
		if between != nil {
			between()
		}
		return start(t)
	}
	profileState := filepath.Join(layout.StateDir, "demo", "dev", "coder")
	return subagentFixture{
		workspace: workspace, sessionFile: layout.SessionFile, sessionDir: layout.SessionStateDir, apiURL: apiURL, provider: provider, childProvider: childProvider, restart: restart,
		stateDir: profileState, toolDir: filepath.Join(workspace, ".jad", "state", "demo", "dev", "coder", "tools"),
		modelsDir: filepath.Join(layout.Root, "models", "openai"), config: config,
	}
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
	return f.runInput(t, "ask the explorer")
}

func (f subagentFixture) runInput(t *testing.T, input string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"input": input})
	request, err := http.NewRequest(http.MethodPost, f.apiURL+"/api/sessions/local:demo/run", strings.NewReader(string(payload)))
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
	if strings.Join(parent[0].Tools, ",") != "read,async,subagent" {
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
	lines := readLines(t, filepath.Join(f.stateDir, "messages.jsonl"))
	if len(lines) != 4 {
		t.Fatalf("session messages.jsonl has %d records, want 4 (input, call, output, final):\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, line := range lines {
		if strings.Contains(line, "the secret is 42") && !strings.Contains(line, "function_call_output") {
			t.Fatalf("a child record is in the session transcript: %s", line)
		}
	}

	// The child's transcript is in the configured storage directory, one folder per call.
	folders, err := filepath.Glob(filepath.Join(f.toolDir, ".subagents", "*", "messages.jsonl"))
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

// A child's client is built like the master's, so a failed opening request is retried by
// provider_retry (the default of two retries here) and the child still answers.
func TestSubagentRetriesAFailedProviderRequestLikeTheMaster(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	f.provider.childFailFirst = 2
	body := f.run(t)
	if !strings.Contains(body, "event: completed") || !strings.Contains(body, "the answer is 42") {
		t.Fatalf("the child did not recover from two 503 answers:\n%s", body)
	}
	if got := f.provider.forModel("child-model"); len(got) != 4 {
		t.Fatalf("the child made %d requests, want 4 (2 failed, 1 tool round, 1 answer)", len(got))
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
	folders, err := filepath.Glob(filepath.Join(f.toolDir, ".subagents", "*", "messages.jsonl"))
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
	if collected := unescapedText(parent[2].Text); !strings.Contains(collected, `"state":"done"`) || !strings.Contains(collected, "the answer is 42") {
		t.Errorf("the collected answer did not reach the parent:\n%s", parent[2].Text)
	}
	// The collect call used the first child; it did not start another one.
	if child := f.provider.forModel("child-model"); len(child) != 2 {
		t.Errorf("child requests = %d, want 2 (one child: read, then answer)", len(child))
	}
	folders, err := filepath.Glob(filepath.Join(f.toolDir, ".subagents", "*", "messages.jsonl"))
	if err != nil || len(folders) != 1 {
		t.Errorf("child transcripts = %v, %v; want exactly 1", folders, err)
	}
}

// handoffAndLeaveRunning runs a parent that starts a child, gets its run id and ends its
// run, while the child is still blocked at the provider. It returns the run id.
func handoffAndLeaveRunning(t *testing.T, f subagentFixture) string {
	t.Helper()
	f.provider.handoffOnly = true
	f.provider.childBlocks = true
	f.provider.childEntered = make(chan struct{}, 1)
	f.provider.childCancelled = make(chan struct{}, 1)
	f.provider.childRelease = make(chan struct{})
	t.Cleanup(func() { close(f.provider.childRelease) })

	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	select {
	case <-f.provider.childEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the child never reached the provider")
	}
	parent := f.provider.forModel("parent-model")
	if len(parent) != 2 || !strings.Contains(parent[1].Text, "still running") {
		t.Fatalf("the parent did not get a handoff:\n%v", parent)
	}
	return runIDOf(t, parent[1].Text)
}

func runIDOf(t *testing.T, text string) string {
	t.Helper()
	id := regexp.MustCompile(`sub_[0-9a-f]{8}`).FindString(text)
	if id == "" {
		t.Fatalf("no run id in:\n%s", text)
	}
	return id
}

func (f subagentFixture) post(t *testing.T, path string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, f.apiURL+"/api/sessions/local:demo"+path, nil)
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
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d, body=%s", path, response.StatusCode, body)
	}
	return string(body)
}

func (f subagentFixture) expectChildCancelled(t *testing.T, why string) {
	t.Helper()
	select {
	case <-f.provider.childCancelled:
	case <-time.After(5 * time.Second):
		t.Fatalf("the handed-off child kept running after %s", why)
	}
}

func TestStoppingTheSessionCancelsAChildHandedOffInAnEarlierRun(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	handoffAndLeaveRunning(t, f)

	// The run is over, so there is nothing for the run to stop; the child still goes on.
	if body := f.post(t, "/run/stop"); !strings.Contains(body, `"stopped":false`) {
		t.Fatalf("stop answered %s, want stopped:false (no run is active)", body)
	}
	f.expectChildCancelled(t, "the user stopped the session")
}

func TestResettingTheSessionCancelsAChildHandedOffInAnEarlierRun(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	handoffAndLeaveRunning(t, f)

	if body := f.post(t, "/reset"); !strings.Contains(body, `"reset":true`) {
		t.Fatalf("reset answered %s", body)
	}
	f.expectChildCancelled(t, "the session was reset")
}

func TestSubagentRunsAreListedInTheStateContextAfterACompaction(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	settingPath := f.sessionFile
	var session map[string]any
	data, err := os.ReadFile(settingPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	setting := session["settings"].(map[string]any)
	setting["compaction"] = map[string]any{"keepbatch": map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}}}
	setting["statecontext"] = map[string]any{"enabled": true, "role": "developer", "triggers": map[string]any{"compaction": map[string]any{"tools": []string{"subagent"}, "max_bytes": 8192}}}
	rewritten, _ := json.Marshal(session)
	writeFile(t, settingPath, rewritten)
	old := strings.Join([]string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"old context"}]}`,
		`{"type":"response.completed","response":{"id":"resp_old","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]}]}}`,
	}, "\n") + "\n"
	writeFile(t, filepath.Join(f.stateDir, "messages.jsonl"), []byte(old))

	id := handoffAndLeaveRunning(t, f)
	if body := f.post(t, "/compact"); !strings.Contains(body, `"compacted":true`) {
		t.Fatalf("compact answered %s", body)
	}

	location, err := sessionfs.Resolve(f.config.Roots, "demo")
	if err != nil {
		t.Fatal(err)
	}
	sessionConfig, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(sessionConfig.StoragePaths.StateContext)
	if err != nil {
		t.Fatalf("no state context snapshot after the compaction: %v", err)
	}
	var snapshot struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"subagent"`, id, `"state": "running"`, `"agent": "explore"`, `"task": "find the answer"`} {
		if !strings.Contains(snapshot.Text, want) {
			t.Errorf("the snapshot lacks %s:\n%s", want, snapshot.Text)
		}
	}
	if strings.Contains(snapshot.Text, "the answer is 42") {
		t.Errorf("an answer was copied into the snapshot:\n%s", snapshot.Text)
	}
}

func TestSubagentTimeoutReachesTheParentWithPartialWorkAndTheTranscript(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "timeout_ms": 1500})
	f.provider.childPartial = true

	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	if len(parent) != 2 {
		t.Fatalf("parent requests = %d, want 2 (task, then the result)", len(parent))
	}
	result := parent[1].Text
	for _, want := range []string{"subagent_timeout", "partial: true", "checking notes.txt first", "rounds: 1"} {
		if !strings.Contains(result, want) {
			t.Errorf("the timeout result lacks %q:\n%s", want, result)
		}
	}
	transcripts, err := filepath.Glob(filepath.Join(f.toolDir, ".subagents", "*", "messages.jsonl"))
	if err != nil || len(transcripts) != 1 {
		t.Fatalf("child transcripts = %v, %v; want exactly 1", transcripts, err)
	}
	if !strings.Contains(result, transcripts[0]) {
		t.Errorf("the result does not name the child's transcript %s:\n%s", transcripts[0], result)
	}
}

// waitForManifestState polls the run manifests under the workspace until one holds the
// state, and returns that run's id.
func waitForManifestState(t *testing.T, f subagentFixture, state string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		files, _ := filepath.Glob(filepath.Join(f.toolDir, ".subagents", ".runs", "sub_*.json"))
		for _, file := range files {
			if data, err := os.ReadFile(file); err == nil && strings.Contains(string(data), `"state":"`+state+`"`) {
				return strings.TrimSuffix(filepath.Base(file), ".json")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no run manifest reached the state %q", state)
	return ""
}

func TestSubagentRunFinishedBeforeARestartCanBeCollectedAfterIt(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	f.provider.handoffOnly = true
	f.provider.childHold = make(chan struct{})
	t.Cleanup(func() { f.provider.holdReleased.Do(func() { close(f.provider.childHold) }) })

	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	id := runIDOf(t, f.provider.forModel("parent-model")[1].Text)
	f.provider.holdReleased.Do(func() { close(f.provider.childHold) })
	if got := waitForManifestState(t, f, "done"); got != id {
		t.Fatalf("the manifest on disk is %s, the handoff named %s", got, id)
	}

	f.apiURL = f.restart(t, nil)
	f.provider.collectMarker, f.provider.collectID = "collect it now", id
	if body := f.runInput(t, "collect it now"); !strings.Contains(body, "event: completed") {
		t.Fatalf("the run after the restart did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	last := unescapedText(parent[len(parent)-1].Text)
	if !strings.Contains(last, `"state":"done"`) || !strings.Contains(last, `"ok":true`) || !strings.Contains(last, "messages.jsonl") {
		t.Errorf("the finished run did not survive the restart as done with its transcript:\n%s", last)
	}
	if child := f.provider.forModel("child-model"); len(child) != 2 {
		t.Errorf("child requests = %d, want 2: collecting must not start another child", len(child))
	}
}

func TestSubagentRunStillGoingAtACrashIsReportedLostWithItsTranscript(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	f.provider.childPartial = true
	id := handoffAndLeaveRunning(t, f)
	manifestPath := filepath.Join(f.toolDir, ".subagents", ".runs", id+".json")
	recordPath := filepath.Join(f.toolDir, ".async", id+".json")

	// A crash leaves the records as the last write made them: running. A graceful close
	// would have ended the run first, so the test puts the crash state back by hand in
	// the run manifest and in the async record, and drops the async index.
	f.apiURL = f.restart(t, func() {
		for _, path := range []string{manifestPath, recordPath} {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			crashed := regexp.MustCompile(`"state":\s*"stopped"`).ReplaceAllString(string(data), `"state":"running"`)
			if crashed == string(data) && !strings.Contains(crashed, `"state":"running"`) {
				t.Fatalf("%s after the close holds neither state:\n%s", path, data)
			}
			crashed = regexp.MustCompile(`"unread":\s*true,?`).ReplaceAllString(crashed, "")
			writeFile(t, path, []byte(crashed))
		}
		_ = os.Remove(filepath.Join(f.toolDir, ".async", "index.json"))
	})
	f.provider.collectMarker, f.provider.collectID = "collect it now", id
	f.runInput(t, "collect it now")
	parent := f.provider.forModel("parent-model")
	last := unescapedText(parent[len(parent)-1].Text)
	for _, want := range []string{`"state":"lost"`, id, "messages.jsonl"} {
		if !strings.Contains(last, want) {
			t.Errorf("the lost run result lacks %q:\n%s", want, last)
		}
	}
	if data, _ := os.ReadFile(manifestPath); !strings.Contains(string(data), `"state":"lost"`) {
		t.Errorf("the manifest was not rewritten as lost:\n%s", data)
	}
}

func TestSubagentRunStillGoingAtAGracefulRestartIsReportedStopped(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	id := handoffAndLeaveRunning(t, f)

	f.apiURL = f.restart(t, nil)
	f.provider.collectMarker, f.provider.collectID = "collect it now", id
	f.runInput(t, "collect it now")
	parent := f.provider.forModel("parent-model")
	last := unescapedText(parent[len(parent)-1].Text)
	if !strings.Contains(last, `"state":"stopped"`) || strings.Contains(last, `"state":"lost"`) {
		t.Errorf("a child cancelled by a graceful shutdown must read as stopped:\n%s", last)
	}
}

// unescapedText undoes the JSON string escaping of a tool output inside a recorded request.
func unescapedText(text string) string { return strings.ReplaceAll(text, `\"`, `"`) }

func countChildFolders(t *testing.T, f subagentFixture) int {
	t.Helper()
	folders, err := filepath.Glob(filepath.Join(f.toolDir, ".subagents", "*", "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return len(folders)
}

func TestSubagentDeleteTranscriptsKeepsOnlyTheFoldersOfKeptRuns(t *testing.T) {
	for _, tc := range []struct {
		name      string
		extra     map[string]any
		folder    int
		manifests int
	}{
		{"off by default", map[string]any{"keep_finished": 1}, 3, 1},
		{"on", map[string]any{"keep_finished": 1, "delete_transcripts": true}, 1, 1},
		{"on without manifests", map[string]any{"keep_finished": 1, "delete_transcripts": true, "persist_runs": false}, 1, 0},
		{"off without manifests", map[string]any{"keep_finished": 1, "persist_runs": false}, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubagentFixture(t, map[string]any{"model": "child.json", "__subagent_settings": tc.extra})
			// One response with three subagent calls: the fake parent answers once it has
			// seen a tool result, so separate runs would start only one child.
			f.provider.parentCalls = 3
			if body := f.run(t); !strings.Contains(body, "event: completed") {
				t.Fatalf("run did not complete:\n%s", body)
			}
			if got := countChildFolders(t, f); got != tc.folder {
				t.Errorf("child transcript folders = %d, want %d", got, tc.folder)
			}
			manifests, _ := filepath.Glob(filepath.Join(f.toolDir, ".subagents", ".runs", "sub_*.json"))
			if len(manifests) != tc.manifests {
				t.Errorf("manifests = %d, want %d", len(manifests), tc.manifests)
			}
		})
	}
}

func TestSubagentSendsTheHostedToolsOfItsOwnModelFileAndTheParentDoesNot(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{
		"model":         "child.json",
		"__child_model": map[string]any{"hosted_tools": []any{map[string]any{"type": "web_search"}}},
	})
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	child := f.provider.forModel("child-model")
	if len(parent) == 0 || len(child) == 0 {
		t.Fatalf("provider requests: parent %d, child %d", len(parent), len(child))
	}
	if got := strings.Join(parent[0].ToolTypes, ","); got != "function,function,function" {
		t.Fatalf("parent tool types = %s, want its two function tools and no hosted tool", got)
	}
	if got := strings.Join(child[0].ToolTypes, ","); got != "function,web_search" {
		t.Fatalf("child tool types = %s, want read then the hosted web_search", got)
	}
}

func TestSubagentWithoutAModelOfItsOwnSendsTheParentsModelFileHostedTools(t *testing.T) {
	// A child that names no model loads the parent's model file, and with it the
	// parent's request fields and hosted_tools. The documentation says so.
	f := newSubagentFixture(t, map[string]any{})
	setHostedTools := func() {
		path := filepath.Join(f.modelsDir, "parent.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var model map[string]any
		if err := json.Unmarshal(data, &model); err != nil {
			t.Fatal(err)
		}
		model["hosted_tools"] = []any{map[string]any{"type": "web_search"}}
		out, _ := json.Marshal(model)
		writeFile(t, path, out)
	}
	setHostedTools()
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	seen := 0
	for _, request := range f.provider.forModel("parent-model") {
		if len(request.ToolTypes) == 0 || request.ToolTypes[len(request.ToolTypes)-1] != "web_search" {
			t.Fatalf("a request of the parent's model lacks the hosted tool: %v", request.ToolTypes)
		}
		seen++
	}
	if seen < 3 {
		t.Fatalf("expected the parent's two rounds and the child's round on the parent model, saw %d", seen)
	}
}

func (f subagentFixture) expectChildKeepsRunning(t *testing.T, what string) {
	t.Helper()
	select {
	case <-f.provider.childCancelled:
		t.Fatalf("the handed-off child was cancelled by %s", what)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestCompactingTheSessionLeavesAChildHandedOffInAnEarlierRunRunning(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	handoffAndLeaveRunning(t, f)
	f.expectChildKeepsRunning(t, "the end of the run")

	request, err := http.NewRequest(http.MethodPost, f.apiURL+"/api/sessions/local:demo/compact", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	f.expectChildKeepsRunning(t, "a compaction")
}

func TestDeletingTheSessionCancelsAChildHandedOffInAnEarlierRun(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	handoffAndLeaveRunning(t, f)

	url := f.apiURL + "/api/catalog/sessions/demo"
	get, _ := http.NewRequest(http.MethodGet, url, nil)
	get.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	current, err := http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, current.Body)
	current.Body.Close()
	del, _ := http.NewRequest(http.MethodDelete, url, nil)
	del.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	del.Header.Set("If-Match", current.Header.Get("ETag"))
	response, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d %s", response.StatusCode, body)
	}
	f.expectChildCancelled(t, "the session was deleted")
}
