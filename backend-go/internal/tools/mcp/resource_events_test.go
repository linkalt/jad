package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// newWatchServer is an MCP server with one resource; subscriptions are on when subscribable is set.
func newWatchServer(subscribable bool) (*mcpSDK.Server, error) {
	var options *mcpSDK.ServerOptions
	if subscribable {
		options = &mcpSDK.ServerOptions{
			SubscribeHandler:   func(context.Context, *mcpSDK.SubscribeRequest) error { return nil },
			UnsubscribeHandler: func(context.Context, *mcpSDK.UnsubscribeRequest) error { return nil },
		}
	}
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "watch", Version: "1.0.0"}, options)
	server.AddResource(&mcpSDK.Resource{URI: "docs://plan", Name: "plan"}, func(_ context.Context, request *mcpSDK.ReadResourceRequest) (*mcpSDK.ReadResourceResult, error) {
		return &mcpSDK.ReadResourceResult{Contents: []*mcpSDK.ResourceContents{{URI: request.Params.URI, Text: "v1"}}}, nil
	})
	return server, nil
}

// watchServer serves newWatchServer over streamable HTTP.
func watchServer(t *testing.T, subscribable bool) (*mcpSDK.Server, string) {
	t.Helper()
	server, _ := newWatchServer(subscribable)
	httpServer := httptest.NewServer(mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return server }, nil))
	t.Cleanup(func() {
		httpServer.CloseClientConnections()
		httpServer.Close()
	})
	return server, httpServer.URL
}

func waitForEvents(t *testing.T, manager *Manager, want int) []ResourceEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, _, _ := manager.ResourceEvents(0)
		if len(events) >= want || time.Now().After(deadline) {
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const watchFixtureEnvironment = "JAD_MCP_WATCH_FIXTURE"

// TestMCPWatchFixtureProcess is the stdio server of the subscription test: one resource,
// subscriptions on, and a "touch" tool that reports the resource changed and adds another
// resource (a list change). It does nothing unless started as the fixture.
func TestMCPWatchFixtureProcess(t *testing.T) {
	if os.Getenv(watchFixtureEnvironment) != "1" {
		return
	}
	server, _ := newWatchServer(true)
	server.AddTool(&mcpSDK.Tool{Name: "touch", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, _ *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		_ = server.ResourceUpdated(ctx, &mcpSDK.ResourceUpdatedNotificationParams{URI: "docs://plan"})
		server.AddResource(&mcpSDK.Resource{URI: "docs://new", Name: "new"}, func(_ context.Context, request *mcpSDK.ReadResourceRequest) (*mcpSDK.ReadResourceResult, error) {
			return &mcpSDK.ReadResourceResult{Contents: []*mcpSDK.ResourceContents{{URI: request.Params.URI, Text: "n"}}}, nil
		})
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "touched"}}}, nil
	})
	_ = server.Run(context.Background(), &mcpSDK.StdioTransport{})
	os.Exit(0)
}

// A subscribed resource that changes and a resource list that changes are recorded, listed
// after a seq, told to the next run once, and the model gets tools to watch resources.
func TestResourceSubscriptionsRecordChangesAndTellTheNextRunOnce(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "watch.json")
	config, _ := json.Marshal(map[string]any{
		"transport":           map[string]any{"type": "stdio", "command": mustExecutable(t), "args": []string{"-test.run=^TestMCPWatchFixtureProcess$"}, "cwd": directory, "env": map[string]string{watchFixtureEnvironment: "1"}},
		"timeout_ms":          10_000,
		"resource_events_max": 10,
	})
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	reference := sessionfs.MCPServerReference{ID: "docs", ConfigPath: configPath, Enabled: true, Resources: true}
	manager := NewManager(currentEnvironment())
	defer manager.Close()
	servers := []sessionfs.MCPServerReference{reference}
	loaded, err := manager.LoadTools(context.Background(), servers, directory)
	if err != nil {
		t.Fatal(err)
	}
	subscribeTool := featureTool(t, loaded, "mcp:docs:resources/subscribe")
	featureTool(t, loaded, "mcp:docs:resources/unsubscribe")

	if got := callFeature(t, subscribeTool, map[string]any{"uri": "docs://plan"}); !strings.Contains(got, `\"subscribed\":true`) {
		t.Fatalf("subscribe tool = %s", got)
	}
	if _, _, subscriptions := manager.ResourceEvents(0); len(subscriptions) != 1 || subscriptions[0] != (ResourceSubscription{Server: "docs", URI: "docs://plan"}) {
		t.Fatalf("subscriptions = %+v", subscriptions)
	}
	var touch *tools.Tool
	for index := range loaded {
		if loaded[index].Definition.ID == "mcp:docs:touch" {
			touch = &loaded[index]
		}
	}
	if touch == nil {
		t.Fatal("no touch tool")
	}
	if _, err := touch.ExecuteOutput(context.Background(), map[string]any{}, tools.ExecutionContext{}); err != nil {
		t.Fatal(err)
	}
	events := waitForEvents(t, manager, 2)
	kinds := map[string]string{}
	for _, event := range events {
		kinds[event.Kind] = event.URI
	}
	if len(events) != 2 || kinds[ResourceEventUpdated] != "docs://plan" || kinds[ResourceEventListChanged] != "" {
		t.Fatalf("events = %+v", events)
	}
	if after, latest, _ := manager.ResourceEvents(events[0].Seq); len(after) != 1 || latest != events[1].Seq {
		t.Fatalf("events after the first = %+v latest %d", after, latest)
	}

	told := manager.TakeResourceChanges()
	note := ResourceChangesNote(told)
	if len(told) != 2 || !strings.Contains(note, "server docs: docs://plan changed") || !strings.Contains(note, "server docs: its resource list changed") {
		t.Fatalf("note = %q", note)
	}
	if again := manager.TakeResourceChanges(); len(again) != 0 || ResourceChangesNote(again) != "" {
		t.Fatalf("told twice: %+v", again)
	}
	if listed, _, _ := manager.ResourceEvents(0); len(listed) != 2 {
		t.Fatalf("telling the run dropped the events from the list: %+v", listed)
	}

	if err := manager.SubscribeUserResource(context.Background(), servers, directory, "docs", "docs://plan", false); err != nil {
		t.Fatal(err)
	}
	if _, _, subscriptions := manager.ResourceEvents(0); len(subscriptions) != 0 {
		t.Fatalf("subscriptions after unsubscribe = %+v", subscriptions)
	}
}

// A server without subscriptions offers no watch tools, and a subscribe request is refused;
// so does a streamable_http server that offers them, whose notifications could not arrive.
func TestResourceSubscriptionsNeedAServerThatOffersThem(t *testing.T) {
	_, url := watchServer(t, true)
	reference := bodyLimitTestServerReference(t, "docs", url, 65_536)
	reference.Resources = true
	manager := NewManager(nil)
	defer manager.Close()
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range loaded {
		if strings.Contains(tool.Definition.ID, "subscribe") {
			t.Fatalf("a streamable_http server got %s", tool.Definition.ID)
		}
	}
	err = manager.SubscribeUserResource(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir(), "docs", "docs://plan", true)
	var httpErr *UserPromptError
	if !asUserPromptError(err, &httpErr) || !strings.Contains(httpErr.Message, "stdio servers only") {
		t.Fatalf("streamable_http subscribe: %v", err)
	}
	testUnsubscribableServer(t)
}

func testUnsubscribableServer(t *testing.T) {
	_, url := watchServer(t, false)
	reference := bodyLimitTestServerReference(t, "docs", url, 65_536)
	reference.Resources = true
	manager := NewManager(nil)
	defer manager.Close()
	servers := []sessionfs.MCPServerReference{reference}
	loaded, err := manager.LoadTools(context.Background(), servers, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range loaded {
		if strings.Contains(tool.Definition.ID, "subscribe") {
			t.Fatalf("a server without subscriptions got %s", tool.Definition.ID)
		}
	}
	err = manager.SubscribeUserResource(context.Background(), servers, t.TempDir(), "docs", "docs://plan", true)
	var userErr *UserPromptError
	if !asUserPromptError(err, &userErr) || userErr.NotFound || !strings.Contains(userErr.Message, "does not offer resource subscriptions") {
		t.Fatalf("err = %v", err)
	}
	if err := manager.SubscribeUserResource(context.Background(), servers, t.TempDir(), "other", "x", true); !asUserPromptError(err, &userErr) || !userErr.NotFound {
		t.Fatalf("unknown server: %v", err)
	}
}

func TestResourceEventsKeepTheNewestPerServer(t *testing.T) {
	var events resourceEvents
	for index := 0; index < 5; index++ {
		events.record("a", ResourceEventUpdated, "u", 3)
	}
	events.record("b", ResourceEventListChanged, "", 3)
	kept, latest := events.since(0)
	if len(kept) != 4 || kept[0].Seq != 3 || latest != 6 || kept[3].Server != "b" {
		t.Fatalf("kept = %+v latest %d", kept, latest)
	}
}
