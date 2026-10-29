package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// waitForGoroutines waits for the goroutine count to settle back to at most before+slack,
// closing idle HTTP connections while it waits.
func waitForGoroutines(t *testing.T, before, slack int) {
	t.Helper()
	var after int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		if after = runtime.NumGoroutine(); after <= before+slack {
			return
		}
	}
	t.Fatalf("goroutines before=%d after=%d", before, after)
}

// A tool call that is still running on the server must not delay Close: the call is
// cancelled and Close returns without waiting for the server's answer.
func TestManagerCloseDoesNotWaitForAnInflightToolCall(t *testing.T) {
	var startOnce sync.Once
	started := make(chan struct{})
	release := make(chan struct{})
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "slow-call", Version: "1.0.0"}, nil)
	server.AddTool(&mcpSDK.Tool{
		Name: "status", Description: "answers immediately",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(context.Context, *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "ok"}}}, nil
	})
	server.AddTool(&mcpSDK.Tool{
		Name: "slow", Description: "blocks until the call is cancelled",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(ctx context.Context, _ *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		startOnce.Do(func() { close(started) })
		select {
		case <-release:
			return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "late"}}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	// Stateless: the session has no id, so Close skips the SDK DELETE round trip whose
	// closeDeleteTimeout would otherwise show up as a wait of its own.
	handler := mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return server }, &mcpSDK.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(func() {
		close(release)
		httpServer.CloseClientConnections()
		httpServer.Close()
	})

	reference := serverReference(t, "slow", httpServer.URL, 20_000)
	manager := NewManager(nil)
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var statusTool, slowTool tools.Tool
	for _, candidate := range loaded {
		switch candidate.Definition.ID {
		case "mcp:slow:status":
			statusTool = candidate
		case "mcp:slow:slow":
			slowTool = candidate
		}
	}
	if statusTool.Definition.ID == "" || slowTool.Definition.ID == "" {
		t.Fatalf("LoadTools() did not return both tools: %+v", loaded)
	}
	// A normal call is unaffected by the shutdown path.
	if output, err := statusTool.ExecuteOutput(context.Background(), map[string]any{}, tools.ExecutionContext{}); err != nil || !strings.Contains(string(output), "ok") {
		t.Fatalf("status tool = %s, %v; want ok", output, err)
	}

	before := runtime.NumGoroutine()
	callDone := make(chan struct{})
	var callOutput json.RawMessage
	go func() {
		defer close(callDone)
		callOutput, _ = slowTool.ExecuteOutput(context.Background(), map[string]any{}, tools.ExecutionContext{})
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the tool call never reached the server")
	}

	closed := make(chan error, 1)
	go func() { closed <- manager.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on an in-flight tool call")
	}
	select {
	case <-callDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight tool call did not return after Close")
	}
	if !strings.Contains(string(callOutput), `\"status\":\"error\"`) {
		t.Fatalf("cancelled call returned %s, want an error result", callOutput)
	}
	waitForGoroutines(t, before, 2)
}

// Close cancels a connect that is still running: it returns promptly, and the aborted
// connect settles at once instead of running into its own 20s timeout, so no session (and
// no stdio child) can be created after Close returned.
func TestManagerCloseCancelsAPendingConnect(t *testing.T) {
	var connectOnce sync.Once
	connectStarted := make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// The first POST of a connect is its handshake; hang it to hold the connect open.
		if r.Method == http.MethodPost && (bytes.Contains(body, []byte(`"method":"initialize"`)) || bytes.Contains(body, []byte(`"method":"server/discover"`))) {
			connectOnce.Do(func() { close(connectStarted) })
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(func() {
		httpServer.CloseClientConnections()
		httpServer.Close()
	})

	cwd := t.TempDir()
	reference := serverReference(t, "pending", httpServer.URL, 20_000)
	manager := NewManager(nil)
	before := runtime.NumGoroutine()
	loadDone := make(chan error, 1)
	go func() {
		_, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, cwd)
		loadDone <- err
	}()
	select {
	case <-connectStarted:
	case err := <-loadDone:
		t.Fatalf("LoadTools() returned before connecting: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the connect never reached the server")
	}

	closed := make(chan error, 1)
	go func() { closed <- manager.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a pending connect")
	}
	// Without the cancellation the connect stays pending until its own timeout, which is
	// far longer than this bound.
	select {
	case err := <-loadDone:
		if err == nil {
			t.Fatal("LoadTools() succeeded although Close cancelled its connect")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the connect was still pending after Close returned")
	}
	waitForGoroutines(t, before, 2)
}

// Close must not return while a connect is still in flight: the cancelled connect has to
// settle first. The counter stands in for a connect that getConnection registered.
func TestManagerCloseWaitsForPendingConnectsToSettle(t *testing.T) {
	manager := NewManager(nil)
	manager.connects.Add(1)
	closed := make(chan error, 1)
	go func() { closed <- manager.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close() = %v while a connect was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	manager.connects.Done()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the pending connect settled")
	}
}
