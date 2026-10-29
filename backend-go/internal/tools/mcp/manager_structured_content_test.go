package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// structuredContentResult runs one MCP tool call whose server answers with exactly the
// given content array and structured content, and returns the parts of the tool result.
func structuredContentResult(t *testing.T, contentJSON, structuredJSON string, maxResultBytes int) []outputPart {
	t.Helper()
	server, toolName := bodyLimitTestTool(t, "unused")
	httpServer := bodyLimitTestServer(t, server, func(w http.ResponseWriter, id json.RawMessage) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(id)+`,"result":{"content":`+contentJSON+`,"structuredContent":`+structuredJSON+`}}`)
	})
	reference := bodyLimitTestServerReference(t, "structured", httpServer.URL, maxResultBytes)
	manager := NewManager(nil)
	t.Cleanup(func() { _ = manager.Close() })
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir())
	if err != nil {
		t.Fatalf("LoadTools() error = %v", err)
	}
	var exposed tools.Tool
	for _, candidate := range loaded {
		if candidate.Definition.ID == "mcp:structured:"+toolName {
			exposed = candidate
		}
	}
	if exposed.Definition.ID == "" {
		t.Fatalf("LoadTools() did not return %s: %+v", toolName, loaded)
	}
	output, err := exposed.ExecuteOutput(context.Background(), map[string]any{}, tools.ExecutionContext{})
	if err != nil {
		t.Fatalf("%s output error = %v", toolName, err)
	}
	var parts []outputPart
	if err := json.Unmarshal(output, &parts); err != nil {
		t.Fatalf("tool result %s is not a part list: %v", output, err)
	}
	return parts
}

func structuredContentText(t *testing.T, parts []outputPart) string {
	t.Helper()
	for _, part := range parts {
		if part.Text != nil && strings.HasPrefix(*part.Text, "Structured content:\n") {
			return strings.TrimPrefix(*part.Text, "Structured content:\n")
		}
	}
	t.Fatalf("tool result parts hold no structured content: %+v", parts)
	return ""
}

// A structured content literal above float64's range used to fail the whole call, and an
// integer beyond 2^53 was silently rounded on the way through.
func TestManagerKeepsStructuredContentNumberLiterals(t *testing.T) {
	parts := structuredContentResult(t, `[{"type":"text","text":"ok"}]`, `{"m":1e400,"id":9007199254740993}`, 1<<20)
	for _, part := range parts {
		if part.Text != nil && strings.Contains(*part.Text, `"status":"error"`) {
			t.Fatalf("tool result reported an error: %+v", parts)
		}
	}
	if got, want := structuredContentText(t, parts), `{"m":null,"id":9007199254740993}`; got != want {
		t.Fatalf("structured content = %s, want %s", got, want)
	}
}
