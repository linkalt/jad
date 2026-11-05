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

// watchFixtureNoAckEnvironment, set to 1, makes the watch fixture drop its subscription
// acknowledgements: a listen-protocol server that never acknowledges.
const watchFixtureNoAckEnvironment = "JAD_MCP_WATCH_FIXTURE_NO_ACK"

// TestMCPWatchFixtureProcess is the stdio server of the subscription test: one resource,
// subscriptions on, and a "touch" tool that reports the resource changed and adds another
// resource (a list change). It does nothing unless started as the fixture.
func TestMCPWatchFixtureProcess(t *testing.T) {
	if os.Getenv(watchFixtureEnvironment) != "1" {
		return
	}
	server, _ := newWatchServer(true)
	if os.Getenv(watchFixtureNoAckEnvironment) == "1" {
		server.AddSendingMiddleware(func(next mcpSDK.MethodHandler) mcpSDK.MethodHandler {
			return func(ctx context.Context, method string, request mcpSDK.Request) (mcpSDK.Result, error) {
				if method == methodSubscriptionsAcknowledged {
					return nil, nil
				}
				return next(ctx, method, request)
			}
		})
	}
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
		"changes_note":        true,
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
	// A second subscribe to the same resource sends nothing and waits for no acknowledgement.
	again := time.Now()
	if got := callFeature(t, subscribeTool, map[string]any{"uri": "docs://plan"}); !strings.Contains(got, `\"subscribed\":true`) || time.Since(again) > 2*time.Second {
		t.Fatalf("second subscribe = %s after %s", got, time.Since(again))
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

// A new connection makes the subscriptions again in one request: with a server that
// acknowledges they all work again, and with one that never does they are all dropped after
// one timeout_ms, not one per subscription.
func TestResubscribeWaitsForTheAcknowledgementsTogether(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "watch.json")
	writeConfig := func(noAck string, timeoutMS int) {
		config, _ := json.Marshal(map[string]any{
			"transport":           map[string]any{"type": "stdio", "command": mustExecutable(t), "args": []string{"-test.run=^TestMCPWatchFixtureProcess$"}, "cwd": directory, "env": map[string]string{watchFixtureEnvironment: "1", watchFixtureNoAckEnvironment: noAck}},
			"timeout_ms":          timeoutMS,
			"resource_events_max": 10,
		})
		if err := os.WriteFile(configPath, config, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("0", 2_000)
	servers := []sessionfs.MCPServerReference{{ID: "docs", ConfigPath: configPath, Enabled: true, Resources: true}}
	manager := NewManager(currentEnvironment())
	defer manager.Close()
	if _, err := manager.LoadTools(context.Background(), servers, directory); err != nil {
		t.Fatal(err)
	}
	uris := []string{"docs://a", "docs://b", "docs://c", "docs://plan"}
	for _, uri := range uris {
		if err := manager.SubscribeUserResource(context.Background(), servers, directory, "docs", uri, true); err != nil {
			t.Fatalf("subscribe %s: %v", uri, err)
		}
	}
	// A changed config is a new connection: an acknowledging server keeps every subscription.
	writeConfig("0", 2_001)
	loaded, err := manager.LoadTools(context.Background(), servers, directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, subscriptions := manager.ResourceEvents(0); len(subscriptions) != len(uris) {
		t.Fatalf("subscriptions after a reconnect = %+v", subscriptions)
	}
	for index := range loaded {
		if loaded[index].Definition.ID == "mcp:docs:touch" {
			if _, err := loaded[index].ExecuteOutput(context.Background(), map[string]any{}, tools.ExecutionContext{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	found := false
	for _, event := range waitForEvents(t, manager, 2) {
		found = found || (event.Kind == ResourceEventUpdated && event.URI == "docs://plan")
	}
	if !found {
		t.Fatal("no update of docs://plan after the reconnect")
	}
	// A server that never acknowledges: the four subscriptions cost one timeout, together.
	writeConfig("1", 1_000)
	started := time.Now()
	if _, err := manager.LoadTools(context.Background(), servers, directory); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("reconnecting with %d unacknowledged subscriptions took %s, want about one timeout_ms", len(uris), took)
	}
	if _, _, subscriptions := manager.ResourceEvents(0); len(subscriptions) != 0 {
		t.Fatalf("unacknowledged subscriptions were kept: %+v", subscriptions)
	}
}

// Two subscribes to one resource at the same time both succeed at once and leave the
// subscription working: the second must not send a listen the SDK drops, then withdraw the
// first one's when its acknowledgement never comes.
func TestConcurrentSubscribesToOneResourceKeepTheSubscription(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "watch.json")
	config, _ := json.Marshal(map[string]any{
		"transport":           map[string]any{"type": "stdio", "command": mustExecutable(t), "args": []string{"-test.run=^TestMCPWatchFixtureProcess$"}, "cwd": directory, "env": map[string]string{watchFixtureEnvironment: "1"}},
		"timeout_ms":          2_000,
		"resource_events_max": 10,
	})
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	servers := []sessionfs.MCPServerReference{{ID: "docs", ConfigPath: configPath, Enabled: true, Resources: true}}
	manager := NewManager(currentEnvironment())
	defer manager.Close()
	loaded, err := manager.LoadTools(context.Background(), servers, directory)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			errs <- manager.SubscribeUserResource(context.Background(), servers, directory, "docs", "docs://plan", true)
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent subscribe: %v", err)
		}
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
	for _, event := range waitForEvents(t, manager, 2) {
		if event.Kind == ResourceEventUpdated && event.URI == "docs://plan" {
			return
		}
	}
	t.Fatalf("no update of docs://plan after the concurrent subscribes: %+v", waitForEvents(t, manager, 2))
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

// A run is told only of the servers whose server file turns changes_note on (off by
// default); every notification is still marked told and kept for the events list.
func TestTakeResourceChangesTellsOnlyServersWithChangesNote(t *testing.T) {
	manager := NewManager(nil)
	defer manager.Close()
	manager.events.setChangesNote("quiet", false)
	manager.events.setChangesNote("loud", true)
	manager.events.record("quiet", ResourceEventListChanged, "", 10)
	manager.events.record("loud", ResourceEventToolsChanged, "", 10)
	manager.events.record("unknown", ResourceEventPromptsChanged, "", 10)
	told := manager.TakeResourceChanges()
	if len(told) != 1 || told[0].Server != "loud" {
		t.Fatalf("told = %+v", told)
	}
	if again := manager.TakeResourceChanges(); again != nil {
		t.Fatalf("told again = %+v", again)
	}
	if listed, _, _ := manager.ResourceEvents(0); len(listed) != 3 {
		t.Fatalf("listed = %+v", listed)
	}
	manager.events.record("quiet", ResourceEventListChanged, "", 10)
	if told := manager.TakeResourceChanges(); told != nil {
		t.Fatalf("a server with changes_note off was told: %+v", told)
	}
}
