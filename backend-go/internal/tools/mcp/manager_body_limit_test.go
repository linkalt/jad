package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

type stubRoundTripper func(*http.Request) (*http.Response, error)

func (s stubRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return s(request)
}

func bodyLimitTestClient(t *testing.T, limit int, contentType string, body string) *http.Client {
	t.Helper()
	return &http.Client{Transport: bodyLimitRoundTripper{
		base: stubRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{contentType}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
		limit: int64(limit),
	}}
}

// A JSON answer larger than the bound fails the read instead of being buffered whole:
// the returned data never exceeds the limit, whatever the server writes.
func TestHTTPResponseBodyLimitRefusesAnOversizedJSONBody(t *testing.T) {
	const limit = 1024
	const bodySize = 64 * 1024
	client := bodyLimitTestClient(t, limit, "application/json", strings.Repeat("x", bodySize))
	response, err := client.Get("http://mcp.example.test/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(response.Body)
	if readErr == nil {
		t.Fatalf("read %d bytes; want the %d-byte limit to refuse the body", len(data), limit)
	}
	if !strings.Contains(readErr.Error(), "max_result_bytes limit of 1024 bytes") {
		t.Fatalf("read error = %v; want the limit named", readErr)
	}
	if len(data) > limit {
		t.Fatalf("buffered %d bytes; want at most %d", len(data), limit)
	}
}

// An event stream body is long lived, so it is left to the per-event bound: the wrapper
// must not cut a stream that is larger than one event may be.
func TestHTTPResponseBodyLimitLeavesAnEventStreamToItsEventBound(t *testing.T) {
	const limit = 1024
	body := "event: message\ndata: " + strings.Repeat("x", 64*1024) + "\n\n"
	client := bodyLimitTestClient(t, limit, "text/event-stream; charset=utf-8", body)
	response, err := client.Get("http://mcp.example.test/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		t.Fatalf("read of an event stream failed: %v", readErr)
	}
	if len(data) != len(body) {
		t.Fatalf("read %d bytes of an event stream; want %d", len(data), len(body))
	}
}

// bodyLimitTestServer serves the given SDK server for every request, except that a
// non-nil answer function writes the tools/call response, so a test controls the one
// response body it cares about.
func bodyLimitTestServer(t *testing.T, server *mcpSDK.Server, answer func(http.ResponseWriter, json.RawMessage)) *httptest.Server {
	t.Helper()
	inner := mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return server }, &mcpSDK.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if answer != nil && bytes.Contains(body, []byte(`"method":"tools/call"`)) {
			answer(w, bodyLimitTestRequestID(body))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		httpServer.CloseClientConnections()
		httpServer.Close()
	})
	return httpServer
}

// bodyLimitTestRequestID echoes the id of the request being answered, so a refused body is
// the only thing that can fail a call.
func bodyLimitTestRequestID(body []byte) json.RawMessage {
	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(body, &request) != nil || len(request.ID) == 0 {
		return json.RawMessage("1")
	}
	return request.ID
}

func bodyLimitTestServerReference(t *testing.T, id, url string, maxResultBytes int) sessionfs.MCPServerReference {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"transport":        map[string]any{"type": "streamable_http", "url": url},
		"timeout_ms":       10_000,
		"max_result_bytes": maxResultBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), id+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return sessionfs.MCPServerReference{ID: id, ConfigPath: path, Enabled: true}
}

func bodyLimitTestTool(t *testing.T, text string) (*mcpSDK.Server, string) {
	t.Helper()
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "limit", Version: "1.0.0"}, nil)
	server.AddTool(&mcpSDK.Tool{
		Name: "big", Description: "answers with the given text",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(context.Context, *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: text}}}, nil
	})
	return server, "big"
}

// A JSON answer larger than max_result_bytes is refused with an error that names the
// limit, and the oversized payload is never carried into the tool result.
func TestManagerRefusesAnOversizedStreamableHTTPResponseBody(t *testing.T) {
	const maxResultBytes = 4096
	server, toolName := bodyLimitTestTool(t, "unused")
	httpServer := bodyLimitTestServer(t, server, func(w http.ResponseWriter, id json.RawMessage) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(id)+`,"result":{"content":[{"type":"text","text":"`)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 64*1024))
		_, _ = io.WriteString(w, `"}]}}`)
	})

	reference := bodyLimitTestServerReference(t, "limit", httpServer.URL, maxResultBytes)
	manager := NewManager(nil)
	t.Cleanup(func() { _ = manager.Close() })
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var exposed tools.Tool
	for _, candidate := range loaded {
		if candidate.Definition.ID == "mcp:limit:"+toolName {
			exposed = candidate
		}
	}
	if exposed.Definition.ID == "" {
		t.Fatalf("LoadTools() did not return %s: %+v", toolName, loaded)
	}
	output, err := exposed.ExecuteOutput(context.Background(), map[string]any{}, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), `\"status\":\"error\"`) {
		t.Fatalf("tool result = %s; want an error result", output)
	}
	if len(output) >= maxResultBytes {
		t.Fatalf("tool result is %d bytes; want it bounded", len(output))
	}

	current, err := manager.getConnection(context.Background(), reference, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := current.callTool(callCtx, &mcpSDK.CallToolParams{Name: toolName, Arguments: map[string]any{}}); err == nil {
		t.Fatal("callTool() accepted an oversized response body")
	} else if !strings.Contains(err.Error(), "max_result_bytes limit of 4096 bytes") {
		t.Fatalf("callTool() error = %v; want the limit named", err)
	}
}

// A single event stream response larger than the bound is refused as a malformed event
// instead of being buffered.
func TestManagerRefusesAnOversizedStreamableHTTPEvent(t *testing.T) {
	const maxResultBytes = 4096
	server, toolName := bodyLimitTestTool(t, "unused")
	httpServer := bodyLimitTestServer(t, server, func(w http.ResponseWriter, id json.RawMessage) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":"+string(id)+",\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 64*1024))
		_, _ = io.WriteString(w, "\"}]}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	})

	reference := bodyLimitTestServerReference(t, "limit", httpServer.URL, maxResultBytes)
	manager := NewManager(nil)
	t.Cleanup(func() { _ = manager.Close() })
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var exposed tools.Tool
	for _, candidate := range loaded {
		if candidate.Definition.ID == "mcp:limit:"+toolName {
			exposed = candidate
		}
	}
	if exposed.Definition.ID == "" {
		t.Fatalf("LoadTools() did not return %s: %+v", toolName, loaded)
	}
	output, err := exposed.ExecuteOutput(context.Background(), map[string]any{}, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), `\"status\":\"error\"`) {
		t.Fatalf("tool result = %s; want an error result", output)
	}

	current, err := manager.getConnection(context.Background(), reference, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := current.callTool(callCtx, &mcpSDK.CallToolParams{Name: toolName, Arguments: map[string]any{}}); err == nil {
		t.Fatal("callTool() accepted an oversized event")
	} else if !strings.Contains(err.Error(), "4096 bytes") {
		t.Fatalf("callTool() error = %v; want the %d-byte event bound", err, maxResultBytes)
	}
}

// A response that fits under the bound is delivered unchanged.
func TestManagerAcceptsAStreamableHTTPResponseBodyUnderTheBound(t *testing.T) {
	const maxResultBytes = 4096
	text := strings.Repeat("y", 3500)
	server, toolName := bodyLimitTestTool(t, text)
	httpServer := bodyLimitTestServer(t, server, nil)

	reference := bodyLimitTestServerReference(t, "limit", httpServer.URL, maxResultBytes)
	manager := NewManager(nil)
	t.Cleanup(func() { _ = manager.Close() })
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var exposed tools.Tool
	for _, candidate := range loaded {
		if candidate.Definition.ID == "mcp:limit:"+toolName {
			exposed = candidate
		}
	}
	if exposed.Definition.ID == "" {
		t.Fatalf("LoadTools() did not return %s: %+v", toolName, loaded)
	}
	output, err := exposed.ExecuteOutput(context.Background(), map[string]any{}, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), strings.Repeat("y", 3000)) {
		t.Fatalf("tool result (%d bytes) lost the answer that fits under the bound", len(output))
	}
}
