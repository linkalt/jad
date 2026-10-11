package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/sessionfs/layouttest"
	"example.com/agent-harness/backend-go/internal/tools/native"
)

func TestHTTPRunReadImageSendsTheImageToTheNextProviderRound(t *testing.T) {
	workspace := t.TempDir()
	var picture bytes.Buffer
	canvas := image.NewRGBA(image.Rect(0, 0, 12, 12))
	for x := 0; x < 12; x++ {
		canvas.Set(x, x, color.RGBA{R: 200, A: 255})
	}
	if err := png.Encode(&picture, canvas); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), picture.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	tool, err := native.NewReadImageTool(sessionfs.NativeReadImageSettings{MaxImageBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(struct {
		SchemaVersion int   `json:"schema_version"`
		Tools         []any `json:"tools"`
	}{SchemaVersion: 1, Tools: []any{tool.Definition}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "read_image.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	var providerCalls atomic.Int32
	var imageInSecondRequest atomic.Value
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []json.RawMessage `json:"tools"`
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid Responses request", http.StatusBadRequest)
			return
		}
		var events []string
		switch providerCalls.Add(1) {
		case 1:
			if len(request.Tools) != 1 || !strings.Contains(string(request.Tools[0]), `"name":"read_image"`) {
				http.Error(w, "native read_image definition missing", http.StatusBadRequest)
				return
			}
			item := `{"type":"function_call","call_id":"call-img","name":"read_image","arguments":"{\"path\":\"shot.png\"}"}`
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
				`{"type":"response.completed","response":{"id":"resp-tool","status":"completed","model":"gpt-test","output":[]}}`,
			}
		case 2:
			for _, item := range request.Input {
				var result struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Output []struct {
						Type     string `json:"type"`
						ImageURL string `json:"image_url"`
					} `json:"output"`
				}
				if json.Unmarshal(item, &result) == nil && result.Type == "function_call_output" && result.CallID == "call-img" && len(result.Output) == 1 && result.Output[0].Type == "input_image" {
					imageInSecondRequest.Store(result.Output[0].ImageURL)
				}
			}
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I see a diagonal line"}]}}`,
				`{"type":"response.completed","response":{"id":"resp-final","status":"completed","model":"gpt-test","output":[]}}`,
			}
		default:
			http.Error(w, "unexpected extra provider round", http.StatusBadRequest)
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

	config, fixture := layoutConfig(t, layouttest.Spec{
		CWD:                 workspace,
		ToolDefinitionPaths: []string{filepath.Join(workspace, "read_image.json")},
		Settings:            map[string]any{"max_tool_rounds": 2},
		Native:              map[string]any{"read_image": map[string]any{}},
		Models:              map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}},
	}, map[string]string{"AGENT_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"})
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	api := httptest.NewServer(service.Handler)
	defer func() {
		service.Close()
		api.Close()
	}()

	request, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"what is in shot.png"}`))
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
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "event: completed") || !strings.Contains(string(body), "diagonal line") {
		t.Fatalf("run status = %d, body=%s", response.StatusCode, body)
	}
	if calls := providerCalls.Load(); calls != 2 {
		t.Fatalf("provider requests = %d, want 2", calls)
	}
	sent, _ := imageInSecondRequest.Load().(string)
	payload, found := strings.CutPrefix(sent, "data:image/png;base64,")
	if !found {
		t.Fatalf("the second request carried no png input_image: %.80q", sent)
	}
	if decoded, err := base64.StdEncoding.DecodeString(payload); err != nil || !bytes.Equal(decoded, picture.Bytes()) {
		t.Fatalf("the image sent to the provider differs from shot.png (err=%v)", err)
	}

	transcriptBytes, err := os.ReadFile(filepath.Join(profileState(fixture), "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(transcriptBytes)), "\n"); len(lines) != 4 || !strings.Contains(lines[2], `"type":"function_call_output"`) || !strings.Contains(lines[2], `"type":"input_image"`) {
		t.Fatalf("messages.jsonl = %q, want input, tool call, tool output with an input_image, final response", lines)
	}

	// With image_store off, the UI reaches the image through the message-bound route.
	status, _, snapshotBody := getWithToken(t, api.URL+"/api/sessions/local:demo", integrationAPIToken)
	if status != http.StatusOK {
		t.Fatalf("snapshot status %d: %s", status, snapshotBody)
	}
	var snapshot struct {
		Messages []struct {
			ID     string `json:"id"`
			Cursor *int64 `json:"cursor"`
			Parts  []struct {
				CallID       string `json:"callId"`
				InlineImages int    `json:"inlineImages"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(snapshotBody, &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, message := range snapshot.Messages {
		for _, part := range message.Parts {
			if part.InlineImages == 0 {
				continue
			}
			if part.CallID != "call-img" || part.InlineImages != 1 || message.Cursor == nil {
				t.Fatalf("tool result = %+v, cursor %v; want call-img, one image, a cursor", part, message.Cursor)
			}
			route := "/api/sessions/local:demo/messages/" + url.PathEscape(message.ID) + "/images/call-img/0?cursor=" + strconv.FormatInt(*message.Cursor, 10)
			status, header, served := getWithToken(t, api.URL+route, integrationAPIToken)
			if status != http.StatusOK || header.Get("Content-Type") != "image/png" || !bytes.Equal(served, picture.Bytes()) {
				t.Fatalf("image route: status %d, type %q, %d bytes (want %d)", status, header.Get("Content-Type"), len(served), picture.Len())
			}
			return
		}
	}
	t.Fatalf("no tool result with an inline image in the snapshot: %s", snapshotBody)
}
