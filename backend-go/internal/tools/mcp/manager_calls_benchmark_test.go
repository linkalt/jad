package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// BenchmarkParallelToolCalls is one provider response with N calls of the same MCP
// server's tool, run together the way runToolBatch runs them. Every HTTP request takes
// 5 ms. If calls overlapped, ns/op would stay near one call; connection.callMu makes it grow with N.
func BenchmarkParallelToolCalls(b *testing.B) {
	const latency = 5 * time.Millisecond
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "bench", Version: "1.0.0"}, nil)
	server.AddTool(&mcpSDK.Tool{
		Name: "tool", Description: "benchmark tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}, func(context.Context, *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "ok"}}}, nil
	})
	inner := mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return server }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(latency)
		inner.ServeHTTP(w, r)
	}))
	b.Cleanup(httpServer.Close)
	configPath := filepath.Join(b.TempDir(), "bench.json")
	data, _ := json.Marshal(map[string]any{"transport": map[string]any{"type": "streamable_http", "url": httpServer.URL}, "timeout_ms": 10_000, "max_result_bytes": 2048})
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		b.Fatal(err)
	}
	servers := []sessionfs.MCPServerReference{{ID: "s0", ConfigPath: configPath, Enabled: true}}
	manager := NewManager(nil)
	b.Cleanup(func() { _ = manager.Close() })
	loaded, err := manager.LoadTools(context.Background(), servers, b.TempDir())
	if err != nil || len(loaded) != 1 {
		b.Fatalf("LoadTools() = %d tools, err=%v", len(loaded), err)
	}
	tool := loaded[0]
	for _, calls := range []int{1, 4, 8} {
		b.Run(fmt.Sprintf("%d-calls-5ms", calls), func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				var group sync.WaitGroup
				group.Add(calls)
				for call := 0; call < calls; call++ {
					go func() {
						defer group.Done()
						if _, err := tool.ExecuteOutput(context.Background(), map[string]any{"q": "x"}, tools.ExecutionContext{}); err != nil {
							b.Error(err)
						}
					}()
				}
				group.Wait()
			}
		})
	}
}

// BenchmarkConcurrentLoadTools is N goroutines calling LoadTools on one manager (a
// SessionTools poll beside a run start, or two profiles of one session) against one
// server with 5 ms per request. Manager.mu is held over the list round trip, so ns/op
// per caller grows with N.
func BenchmarkConcurrentLoadTools(b *testing.B) {
	const latency = 5 * time.Millisecond
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "bench", Version: "1.0.0"}, nil)
	server.AddTool(&mcpSDK.Tool{
		Name: "tool", Description: "benchmark tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}, func(context.Context, *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "ok"}}}, nil
	})
	inner := mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return server }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(latency)
		inner.ServeHTTP(w, r)
	}))
	b.Cleanup(httpServer.Close)
	configPath := filepath.Join(b.TempDir(), "bench.json")
	data, _ := json.Marshal(map[string]any{"transport": map[string]any{"type": "streamable_http", "url": httpServer.URL}, "timeout_ms": 10_000, "max_result_bytes": 2048})
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		b.Fatal(err)
	}
	servers := []sessionfs.MCPServerReference{{ID: "s0", ConfigPath: configPath, Enabled: true}}
	cwd := b.TempDir()
	manager := NewManager(nil)
	b.Cleanup(func() { _ = manager.Close() })
	if _, err := manager.LoadTools(context.Background(), servers, cwd); err != nil {
		b.Fatal(err)
	}
	for _, callers := range []int{1, 4, 8} {
		b.Run(fmt.Sprintf("%d-callers-5ms", callers), func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				var group sync.WaitGroup
				group.Add(callers)
				for caller := 0; caller < callers; caller++ {
					go func() {
						defer group.Done()
						if _, err := manager.LoadTools(context.Background(), servers, cwd); err != nil {
							b.Error(err)
						}
					}()
				}
				group.Wait()
			}
		})
	}
}
