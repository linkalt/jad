package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// discoveryServer starts an in-process MCP server that serves count tools, two per
// tools/list page (the SDK's default page size is 1000, so nothing paginates without
// this), and returns a config path for it. extra is merged into the server file.
func discoveryServer(t *testing.T, count int, schema string, extra map[string]any) (string, string) {
	t.Helper()
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "discovery-fixture", Version: "1.0.0"}, &mcpSDK.ServerOptions{PageSize: 2})
	for index := range count {
		server.AddTool(&mcpSDK.Tool{
			Name:        fmt.Sprintf("tool-%d", index),
			Description: fmt.Sprintf("fixture tool %d", index),
			InputSchema: json.RawMessage(schema),
		}, func(_ context.Context, _ *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
			return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "ok"}}}, nil
		})
	}
	handler := mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	config := map[string]any{"transport": map[string]any{"type": "streamable_http", "url": httpServer.URL}, "timeout_ms": 10_000}
	for key, value := range extra {
		config[key] = value
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "discovery.json")
	if err := os.WriteFile(configPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, filepath.Dir(configPath)
}

func loadDiscoveryTools(t *testing.T, configPath, cwd string) ([]tools.Tool, error) {
	t.Helper()
	manager := NewManager(nil)
	t.Cleanup(func() { _ = manager.Close() })
	return manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{{ID: "discovery", ConfigPath: configPath, Enabled: true}}, cwd)
}

// A server that pages its tools/list with PageSize 2 must still expose every tool: the
// cursor is followed until it is empty.
func TestManagerFollowsToolsListPagination(t *testing.T) {
	configPath, cwd := discoveryServer(t, 5, `{"type":"object","additionalProperties":false}`, nil)
	loaded, err := loadDiscoveryTools(t, configPath, cwd)
	if err != nil {
		t.Fatalf("LoadTools(paginated tools/list) error = %v", err)
	}
	if len(loaded) != 5 {
		names := make([]string, 0, len(loaded))
		for _, tool := range loaded {
			names = append(names, tool.Definition.Name)
		}
		t.Fatalf("loaded %d of 5 paged tools: %v", len(loaded), names)
	}
	// The page order is the server's order, so the tool that only the third page holds
	// must be present, and the catalog must accept the definitions.
	for _, tool := range loaded {
		if tool.Definition.ID == "mcp:discovery:tool-4" {
			if _, err := tools.NewCatalog(loaded); err != nil {
				t.Fatalf("NewCatalog(paged tools) error = %v", err)
			}
			return
		}
	}
	t.Fatalf("the last page of the paged tools/list was not loaded")
}

// Exceeding max_tools is a configuration error naming the setting, not a silently
// shortened catalog.
func TestManagerRefusesMoreToolsThanMaxTools(t *testing.T) {
	configPath, cwd := discoveryServer(t, 5, `{"type":"object","additionalProperties":false}`, map[string]any{"max_tools": 3})
	loaded, err := loadDiscoveryTools(t, configPath, cwd)
	if err == nil || !strings.Contains(err.Error(), "max_tools (3)") || !strings.Contains(err.Error(), "discovery") {
		t.Fatalf("LoadTools(max_tools 3, 5 tools) = %d tools, error %v; want an error naming max_tools and the server", len(loaded), err)
	}
	if loaded != nil {
		t.Fatalf("a server over max_tools returned tools anyway: %d", len(loaded))
	}
}

func TestManagerRefusesAnOversizedToolSchema(t *testing.T) {
	schema := `{"type":"object","properties":{"text":{"type":"string","description":"` + strings.Repeat("x", minMaxSchemaBytes) + `"}},"additionalProperties":false}`
	if len(schema) <= minMaxSchemaBytes {
		t.Fatalf("fixture schema is %d bytes, too small for the test", len(schema))
	}
	configPath, cwd := discoveryServer(t, 2, schema, map[string]any{"max_schema_bytes": minMaxSchemaBytes})
	loaded, err := loadDiscoveryTools(t, configPath, cwd)
	if err == nil || !strings.Contains(err.Error(), "max_schema_bytes (512)") || !strings.Contains(err.Error(), "discovery/tool-0") {
		t.Fatalf("LoadTools(oversized schema) = %d tools, error %v; want an error naming the tool and max_schema_bytes", len(loaded), err)
	}
}

func TestManagerRefusesAnOversizedToolDescription(t *testing.T) {
	configPath, cwd := discoveryServer(t, 2, `{"type":"object","additionalProperties":false}`, map[string]any{"max_description_bytes": 8})
	loaded, err := loadDiscoveryTools(t, configPath, cwd)
	if err == nil || !strings.Contains(err.Error(), "max_description_bytes (8)") || !strings.Contains(err.Error(), "discovery/tool-0") {
		t.Fatalf("LoadTools(oversized description) = %d tools, error %v; want an error naming the tool and max_description_bytes", len(loaded), err)
	}
}

// pageStub answers tools/list from a script and records the cursors it was asked for.
type pageStub struct {
	pages    []toolListPage
	cursors  []string
	requests int
}

func (s *pageStub) listPage(_ context.Context, cursor string) (toolListPage, error) {
	s.cursors = append(s.cursors, cursor)
	if s.requests >= len(s.pages) {
		return toolListPage{}, fmt.Errorf("unexpected page %d", s.requests+1)
	}
	page := s.pages[s.requests]
	s.requests++
	return page, nil
}

func namedTools(count int) []*mcpSDK.Tool {
	remote := make([]*mcpSDK.Tool, 0, count)
	for index := range count {
		remote = append(remote, &mcpSDK.Tool{Name: fmt.Sprintf("tool-%d", index)})
	}
	return remote
}

func TestCollectToolPagesFollowsEveryCursor(t *testing.T) {
	stub := &pageStub{pages: []toolListPage{
		{tools: namedTools(2), nextCursor: "page-2"},
		{tools: namedTools(1), nextCursor: "page-3"},
		{tools: namedTools(1)},
	}}
	collected, err := collectToolPages(context.Background(), 8, stub.listPage)
	if err != nil {
		t.Fatalf("collectToolPages() error = %v", err)
	}
	if len(collected) != 4 || stub.requests != 3 {
		t.Fatalf("collectToolPages() = %d tools from %d requests, want 4 from 3", len(collected), stub.requests)
	}
	if want := []string{"", "page-2", "page-3"}; strings.Join(stub.cursors, ",") != strings.Join(want, ",") {
		t.Fatalf("cursors = %v, want %v", stub.cursors, want)
	}
}

func TestCollectToolPagesRefusesALoopingCursor(t *testing.T) {
	stub := &pageStub{pages: []toolListPage{
		{tools: namedTools(1), nextCursor: "again"},
		{tools: namedTools(1), nextCursor: "again"},
		{tools: namedTools(1), nextCursor: "again"},
	}}
	_, err := collectToolPages(context.Background(), 100, stub.listPage)
	var limit *toolListLimitError
	if !errors.As(err, &limit) || !strings.Contains(limit.Error(), "repeated the tools/list cursor") {
		t.Fatalf("collectToolPages(looping cursor) error = %v, want a repeated-cursor limit error", err)
	}
	if stub.requests != 2 {
		t.Fatalf("requests = %d, want the loop refused on the second page", stub.requests)
	}
}

func TestCollectToolPagesStopsAtThePageCap(t *testing.T) {
	// Every page is empty and still asks for another one: the page bound derived from
	// max_tools is what ends the walk.
	stub := &pageStub{pages: []toolListPage{
		{nextCursor: "a"}, {nextCursor: "b"}, {nextCursor: "c"}, {nextCursor: "d"}, {nextCursor: "e"},
	}}
	_, err := collectToolPages(context.Background(), 2, stub.listPage)
	var limit *toolListLimitError
	if !errors.As(err, &limit) || !strings.Contains(limit.Error(), "max_tools (2) pages") {
		t.Fatalf("collectToolPages(empty pages) error = %v, want the page-cap limit error", err)
	}
	if stub.requests != 3 {
		t.Fatalf("requests = %d, want max_tools+1 = 3 pages", stub.requests)
	}
}

func TestCollectToolPagesRefusesMoreToolsThanMaxTools(t *testing.T) {
	stub := &pageStub{pages: []toolListPage{
		{tools: namedTools(2), nextCursor: "page-2"},
		{tools: namedTools(2), nextCursor: "page-3"},
	}}
	_, err := collectToolPages(context.Background(), 3, stub.listPage)
	var limit *toolListLimitError
	if !errors.As(err, &limit) || !strings.Contains(limit.Error(), "max_tools (3)") {
		t.Fatalf("collectToolPages(4 tools, max 3) error = %v, want the tool-count limit error", err)
	}
}
