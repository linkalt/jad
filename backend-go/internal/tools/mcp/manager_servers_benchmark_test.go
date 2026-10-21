package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
)

// BenchmarkLoadToolsServers is the warm per-run LoadTools over N servers that each
// take latency per HTTP request, so the sequential list loop shows as N round trips.
func BenchmarkLoadToolsServers(b *testing.B) {
	const latency = 5 * time.Millisecond
	for _, serverCount := range []int{1, 4, 8} {
		b.Run(fmt.Sprintf("warm/%d-servers-5ms", serverCount), func(b *testing.B) {
			servers := make([]sessionfs.MCPServerReference, 0, serverCount)
			for index := 0; index < serverCount; index++ {
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
				servers = append(servers, sessionfs.MCPServerReference{ID: fmt.Sprintf("s%d", index), ConfigPath: configPath, Enabled: true})
			}
			cwd := b.TempDir()
			manager := NewManager(nil)
			b.Cleanup(func() { _ = manager.Close() })
			if _, err := manager.LoadTools(context.Background(), servers, cwd); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				loaded, err := manager.LoadTools(context.Background(), servers, cwd)
				if err != nil || len(loaded) != serverCount {
					b.Fatalf("loaded %d tools, err=%v", len(loaded), err)
				}
			}
		})
	}
}
