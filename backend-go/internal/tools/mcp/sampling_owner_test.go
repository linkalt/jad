package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const legacySamplingFixtureEnvironment = "JAD_MCP_LEGACY_SAMPLING_FIXTURE"

// TestMCPLegacySamplingFixtureProcess is a stdio server on protocol 2025-06-18, written by
// hand so it can send sampling/createMessage on its own, as servers of the older protocols do
// (the SDK's server refuses to on 2026-07-28, where sampling is an input request in a result).
// It also sends elicitation/create on its own when a tool's kind argument is "elicit". Its tools:
// sample asks during the call; arm returns at once and asks later, when the file trigger
// appears, writing what it got to out; hold writes trigger and returns once out appears.
// Reading doc://sample samples during the read, doc://elicit elicits. It does nothing unless started as the
// fixture.
func TestMCPLegacySamplingFixtureProcess(t *testing.T) {
	if os.Getenv(legacySamplingFixtureEnvironment) != "1" {
		return
	}
	server := &legacySamplingServer{out: bufio.NewWriter(os.Stdout), pending: map[string]chan legacyMessage{}}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for scanner.Scan() {
		var message legacyMessage
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			continue
		}
		switch {
		case message.Method != "" && message.ID != nil:
			go server.handle(message)
		case message.Method == "" && message.ID != nil:
			server.mu.Lock()
			reply := server.pending[string(message.ID)]
			delete(server.pending, string(message.ID))
			server.mu.Unlock()
			if reply != nil {
				reply <- message
			}
		}
	}
	os.Exit(0)
}

type legacyMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type legacySamplingServer struct {
	mu      sync.Mutex
	out     *bufio.Writer
	next    int
	pending map[string]chan legacyMessage
}

func (s *legacySamplingServer) send(message any) {
	encoded, _ := json.Marshal(message)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(append(encoded, '\n'))
	_ = s.out.Flush()
}

// ask sends one request of its own, sampling/createMessage or (kind "elicit") elicitation/create,
// and reports the answer or the refusal.
func (s *legacySamplingServer) ask(kind string) string {
	s.mu.Lock()
	s.next++
	id := fmt.Sprintf(`"s%d"`, s.next)
	reply := make(chan legacyMessage, 1)
	s.pending[id] = reply
	s.mu.Unlock()
	if kind == "elicit" {
		s.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": "elicitation/create", "params": map[string]any{
			"message": "Which color?", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{"color": map[string]any{"type": "string", "default": "red"}}},
		}})
	} else {
		s.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": "sampling/createMessage", "params": map[string]any{
			"maxTokens": 50, "messages": []any{map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "say hi"}}},
		}})
	}
	answer := <-reply
	if answer.Error != nil {
		return "refused: " + answer.Error.Message
	}
	if kind == "elicit" {
		var result struct {
			Action  string          `json:"action"`
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(answer.Result, &result)
		return "elicited " + result.Action + " " + string(result.Content)
	}
	var result struct {
		Content json.RawMessage `json:"content"`
		Model   string          `json:"model"`
	}
	_ = json.Unmarshal(answer.Result, &result)
	var content struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(result.Content, &content)
	return "sampled " + content.Text + " by " + result.Model
}

func (s *legacySamplingServer) handle(request legacyMessage) {
	text := func(value string) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": value}}}
	}
	var result any
	switch request.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}, "resources": map[string]any{}}, "serverInfo": map[string]any{"name": "legacy", "version": "1.0.0"}}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		var listed []any
		for _, name := range []string{"sample", "arm", "hold"} {
			listed = append(listed, map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}})
		}
		result = map[string]any{"tools": listed}
	case "tools/call":
		var params struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		_ = json.Unmarshal(request.Params, &params)
		switch params.Name {
		case "sample":
			result = text(s.ask(params.Arguments["kind"]))
		case "arm":
			go func() {
				legacyWaitFile(params.Arguments["trigger"])
				legacyWriteFile(params.Arguments["out"], s.ask(params.Arguments["kind"]))
			}()
			result = text("armed")
		case "hold":
			legacyWriteFile(params.Arguments["trigger"], "go")
			legacyWaitFile(params.Arguments["out"])
			result = text("held")
		}
	case "resources/list":
		result = map[string]any{"resources": []any{map[string]any{"uri": "doc://sample", "name": "sample"}, map[string]any{"uri": "doc://elicit", "name": "elicit"}}}
	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(request.Params, &params)
		kind := strings.TrimPrefix(params.URI, "doc://")
		result = map[string]any{"contents": []any{map[string]any{"uri": params.URI, "mimeType": "text/plain", "text": s.ask(kind)}}}
	}
	if result == nil {
		s.send(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": "unknown method " + request.Method}})
		return
	}
	s.send(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
}

func legacyWaitFile(path string) {
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
}

// legacyWriteFile writes through a rename, so a reader never sees half of it.
func legacyWriteFile(path, text string) {
	_ = os.WriteFile(path+".tmp", []byte(text), 0o600)
	_ = os.Rename(path+".tmp", path)
}

// legacyServer starts the legacy fixture (resources on; sampling and elicitation enabled) on a new
// manager with no sampler.
func legacyServer(t *testing.T, maxRequests int) (*Manager, []sessionfs.MCPServerReference, string) {
	t.Helper()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "legacy.json")
	config, _ := json.Marshal(map[string]any{
		"transport":   map[string]any{"type": "stdio", "command": mustExecutable(t), "args": []string{"-test.run=^TestMCPLegacySamplingFixtureProcess$"}, "cwd": directory, "env": map[string]string{legacySamplingFixtureEnvironment: "1"}},
		"timeout_ms":  10_000,
		"sampling":    map[string]any{"enabled": true, "max_requests": maxRequests},
		"elicitation": map[string]any{"enabled": true, "timeout_ms": 5000},
	})
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(currentEnvironment())
	t.Cleanup(func() { _ = manager.Close() })
	return manager, []sessionfs.MCPServerReference{{ID: "legacy", ConfigPath: configPath, Enabled: true, Resources: true}}, directory
}

// legacyListing lists the fixture's tools (a nil sampler lists with LoadTools), by remote name.
func legacyListing(t *testing.T, manager *Manager, servers []sessionfs.MCPServerReference, directory string, sampler Sampler) map[string]tools.Tool {
	t.Helper()
	var loaded []tools.Tool
	var err error
	if sampler == nil {
		loaded, err = manager.LoadTools(context.Background(), servers, directory)
	} else {
		loaded, err = manager.LoadToolsSampled(context.Background(), servers, directory, sampler)
	}
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]tools.Tool{}
	for _, tool := range loaded {
		byName[strings.TrimPrefix(tool.Definition.ID, "mcp:legacy:")] = tool
	}
	for _, name := range []string{"sample", "arm", "hold", readResourceName} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("no %s tool in %v", name, byName)
		}
	}
	return byName
}

func legacyCall(t *testing.T, tool tools.Tool, arguments map[string]any) string {
	t.Helper()
	output, err := tool.ExecuteOutput(context.Background(), arguments, tools.ExecutionContext{CallID: "c-" + tool.Definition.ID})
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func legacyAwaitFile(t *testing.T, path string) string {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if data, err := os.ReadFile(path); err == nil {
			return string(data)
		}
	}
	t.Fatalf("%s was not written", path)
	return ""
}

func wantText(t *testing.T, what, got string, want ...string) {
	t.Helper()
	for _, part := range want {
		if !strings.Contains(got, part) {
			t.Fatalf("%s = %s, want it to hold %q", what, got, part)
		}
	}
}

// countingSampler is a sampler that counts the requests it answers.
func countingSampler(model string, count *atomic.Int32) Sampler {
	return func(context.Context, SamplingRequest) (string, string, error) {
		count.Add(1)
		return "hi", model, nil
	}
}

// legacyUnreached checks that no model answered and that no refusal used the connection's budget.
func legacyUnreached(t *testing.T, manager *Manager, count *atomic.Int32) {
	t.Helper()
	if got := count.Load(); got != 0 {
		t.Fatalf("a model answered %d unattributed sampling requests", got)
	}
	state := legacyState(t, manager)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.samplings != 0 {
		t.Fatalf("refusals used %d of the budget", state.samplings)
	}
}

// A server on an older protocol sends its sampling request on its own, and nothing in it names
// the request it serves: it is refused, during a run's own tool call and resource read too, and
// even with a sampler set on the manager. The user's read is refused the same way.
func TestLegacySamplingIsRefusedEvenDuringTheRunsOwnCallAndResourceRead(t *testing.T) {
	manager, servers, directory := legacyServer(t, 100)
	var count atomic.Int32
	manager.SetSampler(countingSampler("model-manager", &count))
	run := legacyListing(t, manager, servers, directory, countingSampler("model-a", &count))
	wantText(t, "a run's call", legacyCall(t, run["sample"], nil), "refused: ", errSamplingUnattributed.Error())
	wantText(t, "a run's resource read", legacyCall(t, run[readResourceName], map[string]any{"uri": "doc://sample"}), "refused: ", errSamplingUnattributed.Error())

	read, err := manager.ReadUserResource(context.Background(), servers, directory, UserResourceRead{Server: "legacy", URI: "doc://sample"})
	if err != nil {
		t.Fatal(err)
	}
	wantText(t, "the user's resource read", read.Text, "refused: ", errSamplingUnattributed.Error())
	legacyUnreached(t, manager, &count)
}

// A sampling request the server sends after the run's call ended (between calls, after the
// run) is refused with the clear error before any model or the budget: with max_requests 1, two
// refusals leave the budget whole.
func TestLegacySamplingOutsideACallIsRefusedWithoutUsingTheBudget(t *testing.T) {
	manager, servers, directory := legacyServer(t, 1)
	var count atomic.Int32
	run := legacyListing(t, manager, servers, directory, countingSampler("model-a", &count))
	for i := range 2 {
		trigger, out := filepath.Join(directory, fmt.Sprintf("trigger-%d", i)), filepath.Join(directory, fmt.Sprintf("out-%d", i))
		wantText(t, "arm", legacyCall(t, run["arm"], map[string]any{"trigger": trigger, "out": out}), "armed")
		legacyWriteFile(trigger, "go")
		wantText(t, fmt.Sprintf("sampling %d after the call ended", i), legacyAwaitFile(t, out), "refused: ", errSamplingUnattributed.Error())
	}
	legacyUnreached(t, manager, &count)
}

// The misattribution this refusal closes: a request the server sends on its own while a call of
// another run is in progress used to be answered with that run's model and credential. It is
// refused before that run's model, the manager's, or the budget is reached.
func TestLegacySamplingSentOnItsOwnDuringAnUnrelatedCallIsRefused(t *testing.T) {
	manager, servers, directory := legacyServer(t, 1)
	var count atomic.Int32
	manager.SetSampler(countingSampler("model-manager", &count))
	armer := legacyListing(t, manager, servers, directory, countingSampler("model-a", &count))
	holder := legacyListing(t, manager, servers, directory, countingSampler("model-b", &count))
	trigger, out := filepath.Join(directory, "trigger"), filepath.Join(directory, "out")
	wantText(t, "arm", legacyCall(t, armer["arm"], map[string]any{"trigger": trigger, "out": out}), "armed")
	wantText(t, "hold", legacyCall(t, holder["hold"], map[string]any{"trigger": trigger, "out": out}), "held")
	wantText(t, "sampling sent during the other run's call", legacyAwaitFile(t, out), "refused: ", errSamplingUnattributed.Error())
	legacyUnreached(t, manager, &count)
}

// Only a request whose context carries the client's own request mark is answered, whatever call
// the connection has in progress; the model is the call's listing's, else the manager's; the
// refusals (unattributed, no run, a call with no model, other content than text) leave the
// budget alone.
func TestSampleRefusalsLeaveTheBudget(t *testing.T) {
	manager := NewManager(currentEnvironment())
	t.Cleanup(func() { _ = manager.Close() })
	features := clientFeatures{Sampling: true, SamplingMaxTokens: 10, SamplingMaxCalls: 1}
	state := &clientState{}
	ask := func(content mcpSDK.Content) *mcpSDK.CreateMessageRequest {
		return &mcpSDK.CreateMessageRequest{Params: &mcpSDK.CreateMessageParams{Messages: []*mcpSDK.SamplingMessage{{Role: "user", Content: content}}}}
	}
	text := &mcpSDK.TextContent{Text: "say hi"}
	inCall := &activeCall{sampler: namedSampler("model-call")}
	manager.SetSampler(namedSampler("model-manager"))
	if _, err := manager.sample(context.Background(), "s", features, state, ask(text)); !errors.Is(err, errSamplingUnattributed) {
		t.Fatalf("unattributed during a call: %v, want %v", err, errSamplingUnattributed)
	}
	manager.SetSampler(nil)
	if _, err := manager.sample(withInlineRequest(context.Background(), nil), "s", features, state, ask(text)); !errors.Is(err, errSamplingWithoutCall) {
		t.Fatalf("no run's call: %v, want %v", err, errSamplingWithoutCall)
	}
	if _, err := manager.sample(withInlineRequest(context.Background(), &activeCall{}), "s", features, state, ask(text)); !errors.Is(err, errSamplingWithoutModel) {
		t.Fatalf("a call with no model: %v, want %v", err, errSamplingWithoutModel)
	}
	attributed := withInlineRequest(context.Background(), inCall)
	if _, err := manager.sample(attributed, "s", features, state, ask(&mcpSDK.ImageContent{Data: []byte("x"), MIMEType: "image/png"})); err == nil {
		t.Fatal("an image was sampled")
	}
	if state.samplings != 0 {
		t.Fatalf("refusals used %d of the budget", state.samplings)
	}
	if result, err := manager.sample(attributed, "s", features, state, ask(text)); err != nil || result.Model != "model-call" {
		t.Fatalf("with the call's model: %+v %v, want model-call", result, err)
	}
	state.mu.Lock()
	state.samplings = 0
	state.mu.Unlock()
	manager.SetSampler(namedSampler("model-manager"))
	if result, err := manager.sample(withInlineRequest(context.Background(), &activeCall{}), "s", features, state, ask(text)); err != nil || result.Model != "model-manager" {
		t.Fatalf("a call with no model of its own: %+v %v, want the manager's", result, err)
	}
}

func legacyState(t *testing.T, manager *Manager) *clientState {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	current := manager.connections["legacy"]
	if current == nil {
		t.Fatal("no legacy connection")
	}
	return current.state
}
