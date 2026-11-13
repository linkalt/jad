package app

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// profileLazyFixture is a session with two profiles: dev/coder, the caller, and review/reviewer,
// the target. Each has its own toolset and its own tools.lazy. Both toolsets attach one in-process
// MCP server with lookup, extra01 and extra02, so "mcp:fixture:*" selects lazy tools in either
// profile. script answers each model's first request with a call, in order, from its own list.
type profileLazyFixture struct {
	layout    layouttest.Fixture
	service   *Service
	api       *httptest.Server
	mu        sync.Mutex
	calls     *atomic.Int32
	seen      []string
	scripts   map[string][]string
	callerDir string
	targetDir string
}

func newProfileLazyFixture(t *testing.T, callerLazy, targetLazy []string, targetWithoutLazy bool) *profileLazyFixture {
	t.Helper()
	f := &profileLazyFixture{calls: &atomic.Int32{}, scripts: map[string][]string{}}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var request struct {
			Model string `json:"model"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		_ = json.Unmarshal(raw, &request)
		names := make([]string, 0, len(request.Tools))
		for _, tool := range request.Tools {
			names = append(names, tool.Name)
		}
		f.mu.Lock()
		f.seen = append(f.seen, request.Model+": "+strings.Join(names, ","))
		next := ""
		if queue := f.scripts[request.Model]; len(queue) > 0 && !strings.Contains(string(raw), "function_call_output") {
			next = queue[0]
			f.scripts[request.Model] = queue[1:]
		}
		f.mu.Unlock()
		item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`
		if next != "" {
			item = next
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + item + "}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"model\":\"" + request.Model + "\",\"output\":[]}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(provider.Close)

	mcpServer := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "fixture", Version: "1.0.0"}, nil)
	for _, name := range []string{"lookup", "extra01", "extra02"} {
		mcpServer.AddTool(&mcpSDK.Tool{
			Name:        name,
			Description: "looks a word up",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"word":{"type":"string"}},"required":["word"],"additionalProperties":false}`),
		}, func(_ context.Context, request *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
			f.calls.Add(1)
			return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "MCP answered"}}}, nil
		})
	}
	mcpHTTP := httptest.NewServer(mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return mcpServer }, nil))
	t.Cleanup(mcpHTTP.Close)

	toolPath := func(file string) string { return subagentSampleTool(t, file) }
	mcpUse := map[string]any{"deny_tools": []string{}}
	targetTools := map[string]any{"allow": []string{"load_tool", "mcp:fixture:*"}, "lazy": targetLazy}
	if targetWithoutLazy {
		targetTools = map[string]any{"allow": []string{"mcp:fixture:*"}}
	}
	targetProfile := map[string]any{"models": []string{"openai/target"}, "model_default": "openai/target", "toolset": "coding/target", "tools": targetTools}
	spec := layouttest.Spec{
		SessionID:           "demo",
		CWD:                 t.TempDir(),
		Session:             map[string]any{"scope": map[string]any{"profiles": []string{"dev/coder", "review/reviewer"}}},
		ToolDefinitionPaths: []string{toolPath("profile_message.json"), toolPath("load_tool.json")},
		Models: map[string]map[string]any{
			"openai/caller": {"provider": "openai", "model": "caller-model", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"},
			"openai/target": {"provider": "openai", "model": "target-model", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"},
		},
		Profiles: map[string]map[string]any{
			"dev/coder": {"models": []string{"openai/caller"}, "model_default": "openai/caller", "toolset": "coding/caller",
				"tools": map[string]any{"allow": []string{"profile_message", "load_tool", "mcp:fixture:*"}, "lazy": callerLazy}},
			"review/reviewer": targetProfile,
		},
		MCP:       map[string]map[string]any{"m/fixture": {"deny_tools": []string{}}},
		MCPConfig: map[string]map[string]any{"m/fixture": {"schema_version": 1, "transport": map[string]any{"type": "streamable_http", "url": mcpHTTP.URL}, "timeout_ms": 10_000, "max_result_bytes": 4096}},
	}
	f.layout = layouttest.Write(t, spec)
	// The target gets load_tool only with its own tools.lazy: a lazy target without the key is refused.
	targetNative := map[string]any{}
	if len(targetLazy) > 0 {
		targetNative["load_tool"] = map[string]any{}
	}
	for id, file := range map[string]map[string]any{
		"coding/caller": {"native": map[string]any{"profile_message": map[string]any{}, "load_tool": map[string]any{}}, "mcp": map[string]any{"m/fixture": mcpUse}},
		"coding/target": {"native": targetNative, "mcp": map[string]any{"m/fixture": mcpUse}},
	} {
		body, _ := json.Marshal(file)
		writeFile(t, filepath.Join(f.layout.Root, "tools", filepath.FromSlash(id)+".json"), body)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir:      f.layout.Root,
		RootSettingPath: f.layout.RootSetting,
		Environment:     map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"},
	})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if f.service, err = NewService(config); err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(f.service.Close)
	f.callerDir = filepath.Join(f.layout.StateDir, "demo", "dev", "coder")
	f.targetDir = filepath.Join(f.layout.StateDir, "demo", "review", "reviewer")
	return f
}

// seenRequests lists what each model was sent, one entry per request, for the tests above.
func (f *profileLazyFixture) seenRequests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// seenTool reports whether a request to model carried the named tool.
func seenTool(requests []string, model, tool string) bool {
	for _, request := range requests {
		modelName, names, _ := strings.Cut(request, ": ")
		if modelName == model && slices.Contains(strings.Split(names, ","), tool) {
			return true
		}
	}
	return false
}

// callerContext is the context a caller's profile_message runs with: the caller's transcript path,
// which the caller's run would set.
func (f *profileLazyFixture) callerContext(t *testing.T) context.Context {
	t.Helper()
	resolved, err := f.service.resolver.Resolve(context.Background(), "local:demo", "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	bound, ok := resolved.Runner.(boundRunner)
	if !ok {
		t.Fatalf("runner is %T, want boundRunner", resolved.Runner)
	}
	return context.WithValue(context.Background(), callerMessagesPathKey{}, bound.config.MessagesPath)
}

// functionCallItem is one Responses output item that calls the named tool.
func functionCallItem(name, arguments string) string {
	item, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call-1", "name": name, "arguments": arguments})
	return string(item)
}

func dirFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestProfileMessageTargetRunsItsLazyToolThroughItsOwnLoadTool(t *testing.T) {
	f := newProfileLazyFixture(t, []string{"mcp:fixture:*"}, []string{"mcp:fixture:*"}, false)
	f.scripts["target-model"] = []string{functionCallItem("load_tool", `{"run":"mcp:fixture:lookup","arguments":{"word":"x"}}`)}
	if _, err := (profileMessenger{resolver: f.service.resolver}).Message(f.callerContext(t), "local:demo\x00dev/coder", "review/reviewer", "hi", 0, false); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Errorf("the target's run reached the MCP server %d times, want 1", f.calls.Load())
	}
	requireNoLazyFileAt(t, filepath.Join(f.targetDir, "lazy_tools.json"))
	requireNoLazyFileAt(t, filepath.Join(f.callerDir, "lazy_tools.json"))
	if !seenTool(f.seenRequests(), "target-model", "load_tool") {
		t.Error("the target was not sent load_tool")
	}
	// The target's tools array is the same in each of its requests and never lists a lazy MCP tool.
	targetTools := map[string]bool{}
	for _, request := range f.seenRequests() {
		if model, names, ok := strings.Cut(request, ": "); ok && model == "target-model" {
			targetTools[names] = true
			if strings.Contains(names, "mcp_fixture_") {
				t.Errorf("the target was sent a lazy MCP tool: %s", names)
			}
		}
	}
	if len(targetTools) != 1 {
		t.Errorf("the target's tools array differed between its requests: %v", slices.Collect(maps.Keys(targetTools)))
	}
}

func TestProfileMessageRunsStayInTheirOwnProfile(t *testing.T) {
	f := newProfileLazyFixture(t, []string{"mcp:fixture:*"}, []string{"mcp:fixture:lookup"}, false)
	f.scripts["target-model"] = []string{functionCallItem("load_tool", `{"run":"mcp:fixture:lookup","arguments":{"word":"x"}}`)}
	if _, err := (profileMessenger{resolver: f.service.resolver}).Message(f.callerContext(t), "local:demo\x00dev/coder", "review/reviewer", "hi", 0, false); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Errorf("MCP calls = %d, want 1: the target's lookup", f.calls.Load())
	}
	requireNoLazyFileAt(t, filepath.Join(f.targetDir, "lazy_tools.json"))
	requireNoLazyFileAt(t, filepath.Join(f.callerDir, "lazy_tools.json"))
}

// A target without tools.lazy gets no load_tool and no lazy file. Its tools array is the same
// before and after a message from a caller that has lazy tools, so the caller's load changes
// nothing the target is sent.
func TestProfileMessageTargetWithoutLazyGetsNoLoadTool(t *testing.T) {
	f := newProfileLazyFixture(t, []string{"mcp:fixture:*"}, nil, true)
	f.scripts["caller-model"] = []string{functionCallItem("load_tool", `{"names":["mcp:fixture:*"]}`)}
	f.scripts["target-model"] = []string{functionCallItem("load_tool", `{"names":["mcp:fixture:*"]}`)}
	if _, err := (profileMessenger{resolver: f.service.resolver}).Message(f.callerContext(t), "local:demo\x00dev/coder", "review/reviewer", "hi", 0, false); err != nil {
		t.Fatal(err)
	}
	if seenTool(f.seenRequests(), "target-model", "load_tool") {
		t.Error("the target without tools.lazy was sent load_tool")
	}
	requireNoLazyFileAt(t, filepath.Join(f.targetDir, "lazy_tools.json"))
	// The target's tools array in each of its requests must be the same string.
	targetTools := map[string]bool{}
	for _, request := range f.seenRequests() {
		if model, names, ok := strings.Cut(request, ": "); ok && model == "target-model" {
			targetTools[names] = true
		}
	}
	if len(targetTools) != 1 {
		t.Errorf("the target's tools array differed between its requests: %v", slices.Collect(maps.Keys(targetTools)))
	}
}
