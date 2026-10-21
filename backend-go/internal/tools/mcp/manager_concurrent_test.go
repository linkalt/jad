package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
)

type listServerOptions struct {
	delay     time.Duration
	hang      bool
	timeoutMS int
}

type listServerStats struct {
	initializes atomic.Int64
	inflight    *atomic.Int64
	peak        *atomic.Int64
}

func newListServer(t *testing.T, id string, options listServerOptions, shared *listServerStats) (sessionfs.MCPServerReference, *listServerStats) {
	t.Helper()
	stats := &listServerStats{}
	if shared != nil {
		stats.inflight, stats.peak = shared.inflight, shared.peak
	}
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: id, Version: "1.0.0"}, nil)
	server.AddTool(&mcpSDK.Tool{
		Name: "tool_" + id, Description: "tool of " + id,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "ok"}}}, nil
	})
	inner := mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return server }, nil)
	release := make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte(`"method":"initialize"`)) {
				stats.initializes.Add(1)
			}
		}
		if stats.inflight != nil && r.Method == http.MethodPost {
			now := stats.inflight.Add(1)
			defer stats.inflight.Add(-1)
			for {
				peak := stats.peak.Load()
				if now <= peak || stats.peak.CompareAndSwap(peak, now) {
					break
				}
			}
		}
		if options.hang && r.Method == http.MethodPost {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		time.Sleep(options.delay)
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		close(release)
		httpServer.CloseClientConnections()
		httpServer.Close()
	})
	return serverReference(t, id, httpServer.URL, options.timeoutMS), stats
}

func serverReference(t *testing.T, id, url string, timeoutMS int) sessionfs.MCPServerReference {
	t.Helper()
	if timeoutMS == 0 {
		timeoutMS = 10_000
	}
	data, _ := json.Marshal(map[string]any{"transport": map[string]any{"type": "streamable_http", "url": url}, "timeout_ms": timeoutMS, "max_result_bytes": 2048})
	path := filepath.Join(t.TempDir(), id+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return sessionfs.MCPServerReference{ID: id, ConfigPath: path, Enabled: true}
}

func unreachableServer(t *testing.T, id string) sessionfs.MCPServerReference {
	t.Helper()
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	return serverReference(t, id, url, 2000)
}

func toolServerIDs(loaded []string) string { return strings.Join(loaded, ",") }

func TestLoadToolsKeepsConfiguredServerOrder(t *testing.T) {
	delays := []time.Duration{80 * time.Millisecond, 40 * time.Millisecond, 0, 60 * time.Millisecond}
	servers := make([]sessionfs.MCPServerReference, 0, len(delays))
	for index, delay := range delays {
		server, _ := newListServer(t, fmt.Sprintf("s%d", index), listServerOptions{delay: delay}, nil)
		servers = append(servers, server)
	}
	manager := NewManager(nil)
	defer manager.Close()
	for round := 0; round < 3; round++ {
		loaded, err := manager.LoadTools(context.Background(), servers, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, tool := range loaded {
			got = append(got, tool.Definition.Server)
		}
		if toolServerIDs(got) != "s0,s1,s2,s3" {
			t.Fatalf("round %d: tool server order = %s, want s0,s1,s2,s3", round, toolServerIDs(got))
		}
	}
}

func TestLoadToolsReportsTheFirstFailingServerInConfiguredOrder(t *testing.T) {
	slowOK, _ := newListServer(t, "s0", listServerOptions{delay: 60 * time.Millisecond}, nil)
	servers := []sessionfs.MCPServerReference{slowOK, unreachableServer(t, "s1"), unreachableServer(t, "s2")}
	manager := NewManager(nil)
	defer manager.Close()
	_, err := manager.LoadTools(context.Background(), servers, t.TempDir())
	if err == nil {
		t.Fatal("LoadTools() error = nil")
	}
	if !strings.Contains(err.Error(), "failed to connect MCP server s1 (") || strings.Contains(err.Error(), "s2") {
		t.Fatalf("error = %q, want the connect failure of s1 only", err)
	}
	manager.mu.Lock()
	_, kept := manager.connections["s0"]
	manager.mu.Unlock()
	if !kept {
		t.Fatal("the healthy server's connection was dropped")
	}
}

func TestLoadToolsSlowServerIsBoundedByItsOwnTimeout(t *testing.T) {
	hung, _ := newListServer(t, "s0", listServerOptions{hang: true, timeoutMS: 300}, nil)
	fast, _ := newListServer(t, "s1", listServerOptions{}, nil)
	manager := NewManager(nil)
	defer manager.Close()
	started := time.Now()
	_, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{hung, fast}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "failed to connect MCP server s0 (McpError)") {
		t.Fatalf("error = %v, want s0 connect timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("LoadTools took %v, want the 300ms timeout plus the SDK close grace", elapsed)
	}
	manager.mu.Lock()
	_, healthy := manager.connections["s1"]
	manager.mu.Unlock()
	if !healthy {
		t.Fatal("the fast server was not connected while the slow one timed out")
	}
}

func TestLoadToolsHonoursContextCancellation(t *testing.T) {
	a, _ := newListServer(t, "s0", listServerOptions{hang: true}, nil)
	b, _ := newListServer(t, "s1", listServerOptions{hang: true}, nil)
	manager := NewManager(nil)
	defer manager.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(80*time.Millisecond, cancel)
	started := time.Now()
	_, err := manager.LoadTools(ctx, []sessionfs.MCPServerReference{a, b}, t.TempDir())
	if err == nil {
		t.Fatal("LoadTools() error = nil after cancel")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("LoadTools took %v after cancel", elapsed)
	}
	if _, err := manager.LoadTools(ctx, nil, t.TempDir()); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestLoadToolsListsServersConcurrentlyWithinTheBound(t *testing.T) {
	shared := &listServerStats{inflight: new(atomic.Int64), peak: new(atomic.Int64)}
	servers := make([]sessionfs.MCPServerReference, 0, 12)
	for index := 0; index < 12; index++ {
		server, _ := newListServer(t, fmt.Sprintf("s%d", index), listServerOptions{delay: 30 * time.Millisecond}, shared)
		servers = append(servers, server)
	}
	manager := NewManager(nil)
	defer manager.Close()
	if _, err := manager.LoadTools(context.Background(), servers, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	peak := shared.peak.Load()
	if peak < 2 || peak > maxConcurrentServerLoads {
		t.Fatalf("peak concurrent requests = %d, want 2..%d", peak, 8)
	}
}

func TestLoadToolsConcurrentCallersShareOneConnectionPerServer(t *testing.T) {
	var allStats []*listServerStats
	servers := make([]sessionfs.MCPServerReference, 0, 3)
	for index := 0; index < 3; index++ {
		server, stats := newListServer(t, fmt.Sprintf("s%d", index), listServerOptions{delay: 10 * time.Millisecond}, nil)
		servers = append(servers, server)
		allStats = append(allStats, stats)
	}
	manager := NewManager(nil)
	cwd := t.TempDir()
	var group sync.WaitGroup
	for caller := 0; caller < 8; caller++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for round := 0; round < 3; round++ {
				loaded, err := manager.LoadTools(context.Background(), servers, cwd)
				if err != nil || len(loaded) != 3 {
					t.Errorf("LoadTools() = %d tools, err %v", len(loaded), err)
					return
				}
			}
		}()
	}
	group.Wait()
	for index, stats := range allStats {
		if got := stats.initializes.Load(); got != 1 {
			t.Errorf("server s%d saw %d initialize requests, want 1", index, got)
		}
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.LoadTools(context.Background(), servers, cwd); err == nil {
		t.Fatal("LoadTools() after Close succeeded")
	}
}

func TestLoadToolsLeavesNoGoroutinesBehind(t *testing.T) {
	servers := make([]sessionfs.MCPServerReference, 0, 5)
	for index := 0; index < 4; index++ {
		server, _ := newListServer(t, fmt.Sprintf("s%d", index), listServerOptions{delay: 5 * time.Millisecond}, nil)
		servers = append(servers, server)
	}
	servers = append(servers, unreachableServer(t, "bad"))
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	before := runtime.NumGoroutine()
	for round := 0; round < 5; round++ {
		manager := NewManager(nil)
		if _, err := manager.LoadTools(context.Background(), servers, t.TempDir()); err == nil {
			t.Fatal("expected the unreachable server to fail")
		}
		if _, err := manager.LoadTools(context.Background(), servers[:4], t.TempDir()); err != nil {
			t.Fatal(err)
		}
		_ = manager.Close()
	}
	var after int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		if after = runtime.NumGoroutine(); after <= before+2 {
			return
		}
	}
	t.Fatalf("goroutines before=%d after=%d", before, after)
}
