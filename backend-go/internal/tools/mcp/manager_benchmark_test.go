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

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
)

func benchmarkMCPServer(b *testing.B, toolCount int) []sessionfs.MCPServerReference {
	b.Helper()
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "bench", Version: "1.0.0"}, nil)
	for index := 0; index < toolCount; index++ {
		server.AddTool(&mcpSDK.Tool{
			Name: fmt.Sprintf("tool_%d", index), Description: "benchmark tool",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"additionalProperties":false}`),
		}, func(context.Context, *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
			return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "ok"}}}, nil
		})
	}
	handler := mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	b.Cleanup(httpServer.Close)
	configPath := filepath.Join(b.TempDir(), "bench.json")
	data, _ := json.Marshal(map[string]any{"transport": map[string]any{"type": "streamable_http", "url": httpServer.URL}, "timeout_ms": 10_000, "max_result_bytes": 2048})
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		b.Fatal(err)
	}
	return []sessionfs.MCPServerReference{{ID: "bench", ConfigPath: configPath, Enabled: true}}
}

// BenchmarkLoadTools: "warm" refreshes an already connected, unchanged server (the per-run
// cost); "cold" opens a new connection each time.
func BenchmarkLoadTools(b *testing.B) {
	for _, toolCount := range []int{5, 50} {
		b.Run(fmt.Sprintf("warm/%d-tools", toolCount), func(b *testing.B) {
			servers := benchmarkMCPServer(b, toolCount)
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
				if err != nil || len(loaded) != toolCount {
					b.Fatalf("loaded %d tools, err=%v", len(loaded), err)
				}
			}
		})
		b.Run(fmt.Sprintf("cold/%d-tools", toolCount), func(b *testing.B) {
			servers := benchmarkMCPServer(b, toolCount)
			cwd := b.TempDir()
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				manager := NewManager(nil)
				loaded, err := manager.LoadTools(context.Background(), servers, cwd)
				if err != nil || len(loaded) != toolCount {
					b.Fatalf("loaded %d tools, err=%v", len(loaded), err)
				}
				b.StopTimer()
				_ = manager.Close()
				b.StartTimer()
			}
		})
	}
}
