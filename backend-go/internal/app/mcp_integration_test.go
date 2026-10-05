package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

func TestHTTPRunExecutesMCPToolAndPassesMultimodalOutputToNextRound(t *testing.T) {
	workspace := t.TempDir()
	sessionDir := filepath.Join(workspace, "sessions", "demo")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mcpServer := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "fixture", Version: "1.0.0"}, nil)
	mcpServer.AddTool(&mcpSDK.Tool{
		Name:        "echo",
		Description: "returns text, image, and file content",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
	}, func(_ context.Context, request *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		var arguments map[string]any
		if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
			return nil, err
		}
		query, _ := arguments["query"].(string)
		return &mcpSDK.CallToolResult{
			Content: []mcpSDK.Content{
				&mcpSDK.TextContent{Text: "MCP echo: " + query},
				&mcpSDK.ImageContent{MIMEType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
				&mcpSDK.EmbeddedResource{Resource: &mcpSDK.ResourceContents{URI: "file:///tmp/report.pdf", MIMEType: "application/pdf", Blob: []byte("%PDF-1.4")}},
			},
			StructuredContent: json.RawMessage(`{"second":2,"first":1}`),
		}, nil
	})
	mcpHandler := mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return mcpServer }, nil)
	mcpHTTP := httptest.NewServer(mcpHandler)
	defer mcpHTTP.Close()
	mcpConfigPath := filepath.Join(workspace, "mcp-server.json")
	mcpConfig, err := json.Marshal(map[string]any{
		"transport":        map[string]any{"type": "streamable_http", "url": mcpHTTP.URL},
		"timeout_ms":       10_000,
		"max_result_bytes": 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mcpConfigPath, mcpConfig, 0o600); err != nil {
		t.Fatal(err)
	}

	var providerCalls atomic.Int32
	var nextRequestHadMCPOutput atomic.Bool
	var providerFailure atomic.Value
	providerError := func(writer http.ResponseWriter, message string, status int) {
		providerFailure.Store(message)
		http.Error(writer, message, status)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			providerError(w, "unexpected provider request", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer local-provider-key" {
			providerError(w, "unexpected provider credentials", http.StatusUnauthorized)
			return
		}
		var request struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			providerError(w, "invalid Responses request", http.StatusBadRequest)
			return
		}
		var events []string
		switch providerCalls.Add(1) {
		case 1:
			if len(request.Tools) != 1 || !strings.HasPrefix(request.Tools[0].Name, "mcp_fixture_echo_") {
				providerError(w, "MCP tool definition missing", http.StatusBadRequest)
				return
			}
			callItem, err := json.Marshal(map[string]any{
				"type": "function_call", "call_id": "call-mcp-echo", "name": request.Tools[0].Name,
				"arguments": `{"query":"hello from provider"}`,
			})
			if err != nil {
				providerError(w, "could not encode function call", http.StatusInternalServerError)
				return
			}
			eventItem, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": json.RawMessage(callItem)})
			completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp-mcp-call", "status": "completed", "model": "gpt-test", "output": []json.RawMessage{callItem}}})
			events = []string{string(eventItem), string(completed)}
		case 2:
			for _, rawItem := range request.Input {
				var item struct {
					Type   string          `json:"type"`
					CallID string          `json:"call_id"`
					Output json.RawMessage `json:"output"`
				}
				if json.Unmarshal(rawItem, &item) != nil || item.Type != "function_call_output" || item.CallID != "call-mcp-echo" {
					continue
				}
				var parts []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ImageURL string `json:"image_url"`
					FileData string `json:"file_data"`
					Filename string `json:"filename"`
				}
				if json.Unmarshal(item.Output, &parts) != nil || len(parts) != 4 || parts[0].Type != "input_text" || parts[0].Text != "MCP echo: hello from provider" || parts[1].Type != "input_image" || !strings.HasPrefix(parts[1].ImageURL, "data:image/png;base64,") || parts[2].Type != "input_file" || parts[2].Filename != "report.pdf" || parts[2].FileData == "" || parts[3].Type != "input_text" || parts[3].Text != `Structured content:
{"second":2,"first":1}` {
					providerError(w, "MCP multimodal or ordered structured output missing from next provider request", http.StatusBadRequest)
					return
				}
				nextRequestHadMCPOutput.Store(true)
				break
			}
			if !nextRequestHadMCPOutput.Load() {
				providerError(w, "MCP function output missing from next provider request", http.StatusBadRequest)
				return
			}
			message, _ := json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "MCP result received"}}})
			messageDone, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": json.RawMessage(message)})
			completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp-mcp-final", "status": "completed", "model": "gpt-test", "output": []json.RawMessage{message}}})
			events = []string{string(messageDone), string(completed)}
		default:
			providerError(w, "unexpected extra provider round", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range append(events, "[DONE]") {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	defer provider.Close()

	rootSetting := `{"session_roots":[{"id":"local","path":"sessions"}]}`
	if err := os.WriteFile(filepath.Join(workspace, "setting.json"), []byte(rootSetting), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionSetting, err := json.Marshal(map[string]any{
		"cwd":             workspace,
		"max_tool_rounds": 2,
		"mcp_servers":     []any{map[string]any{"id": "fixture", "config_path": mcpConfigPath, "enabled": true, "deny_tools": []string{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), sessionSetting, 0o600); err != nil {
		t.Fatal(err)
	}
	modelSetting := fmt.Sprintf(`{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_OPENAI_KEY","base_url":%q}`, provider.URL+"/v1")
	if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), []byte(modelSetting), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir: workspace,
		Environment: map[string]string{
			"AGENT_API_TOKEN":    integrationAPIToken,
			"FIXTURE_OPENAI_KEY": "local-provider-key",
		},
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	api := httptest.NewServer(service.Handler)
	defer func() {
		service.Close()
		api.Close()
	}()

	toolsResponse := getAPI(t, api.URL, "/api/sessions/local:demo/tools", integrationAPIToken, "")
	if toolsResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(toolsResponse.Body)
		toolsResponse.Body.Close()
		t.Fatalf("session tools status=%d body=%s", toolsResponse.StatusCode, body)
	}
	var effectiveTools struct {
		Tools []struct {
			ID     string `json:"id"`
			Source string `json:"source"`
			Server string `json:"server"`
			Name   string `json:"name"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(toolsResponse.Body).Decode(&effectiveTools); err != nil {
		toolsResponse.Body.Close()
		t.Fatalf("decode session tools: %v", err)
	}
	toolsResponse.Body.Close()
	if len(effectiveTools.Tools) != 1 || effectiveTools.Tools[0].Source != "mcp" || effectiveTools.Tools[0].Server != "fixture" || !strings.HasPrefix(effectiveTools.Tools[0].Name, "mcp_fixture_echo_") {
		t.Fatalf("effective tools = %+v", effectiveTools.Tools)
	}

	request, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"use the MCP echo tool"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "MCP result received") || !strings.Contains(string(body), `"type":"tool_result"`) {
		providerFailureReason, _ := providerFailure.Load().(string)
		t.Fatalf("run status=%d provider_calls=%d provider_failure=%q body=%s", response.StatusCode, providerCalls.Load(), providerFailureReason, body)
	}
	if providerCalls.Load() != 2 || !nextRequestHadMCPOutput.Load() {
		t.Fatalf("provider calls=%d, next request had MCP output=%v", providerCalls.Load(), nextRequestHadMCPOutput.Load())
	}
	transcriptBytes, err := os.ReadFile(filepath.Join(sessionDir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	transcript := string(transcriptBytes)
	structuredStart := strings.Index(transcript, "Structured content")
	structuredTail := ""
	if structuredStart >= 0 {
		structuredTail = transcript[structuredStart:]
	}
	secondKey := strings.Index(structuredTail, "second")
	firstKey := strings.Index(structuredTail, "first")
	if !strings.Contains(transcript, `"call_id":"call-mcp-echo"`) || !strings.Contains(transcript, "MCP echo: hello from provider") || !strings.Contains(transcript, "input_image") || !strings.Contains(transcript, "input_file") || secondKey < 0 || firstKey <= secondKey {
		t.Fatalf("messages.jsonl is missing MCP call, multimodal output, or structured-content key order: %s", transcriptBytes)
	}
}
