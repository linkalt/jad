package main

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
	"testing"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// greetServer is an MCP server with one prompt, greet(name).
func greetServer(t *testing.T) string {
	t.Helper()
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "greet", Version: "1.0.0"}, nil)
	server.AddPrompt(&mcpSDK.Prompt{Name: "greet", Arguments: []*mcpSDK.PromptArgument{{Name: "name", Required: true}}}, func(_ context.Context, request *mcpSDK.GetPromptRequest) (*mcpSDK.GetPromptResult, error) {
		return &mcpSDK.GetPromptResult{Messages: []*mcpSDK.PromptMessage{{Role: "user", Content: &mcpSDK.TextContent{Text: "Say hello to " + request.Params.Arguments["name"] + "."}}}}, nil
	})
	handler := mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return server }, &mcpSDK.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer.URL
}

func TestRunSlashCommands(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"model\":\"m\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7},\"output\":[]}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)
	fixture := useFixture(t, layouttest.Spec{
		Models:    map[string]map[string]any{"openai/default": {"provider": "openai", "model": "m", "api_key_env": "FIXTURE_KEY", "base_url": provider.URL + "/v1"}},
		Server:    map[string]any{"log_level": "off"},
		MCP:       map[string]map[string]any{"chat/greet": {"prompts": true}},
		MCPConfig: map[string]map[string]any{"chat/greet": {"schema_version": 1, "transport": map[string]any{"type": "streamable_http", "url": greetServer(t)}}},
	})
	t.Setenv("FIXTURE_KEY", "k")
	prompts := filepath.Join(fixture.Root, ".agent", "prompts")
	if err := os.MkdirAll(prompts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prompts, "review.md"), []byte("---\nname: Code review\ndescription: Review code\n---\nReview this code carefully.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := "local:" + fixture.SessionID
	transcript := func() string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(fixture.SessionStateDir, "dev", "coder", "messages.jsonl"))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return string(data)
	}
	quoted := func(text string) string {
		encoded, _ := json.Marshal(text)
		return string(encoded[1 : len(encoded)-1])
	}

	stdout, stderr, err := runJad(t, "", "run", "--session", session, "/prompt:code-review", "main.go")
	if err != nil || stdout != "ok\n" {
		t.Fatalf("library prompt: %q %q %v", stdout, stderr, err)
	}
	if !strings.Contains(transcript(), quoted("Review this code carefully.\n\nmain.go")) {
		t.Fatalf("the prompt text is not in the transcript:\n%s", transcript())
	}

	stdout, _, err = runJad(t, "", "run", "--session", session, "--format", "json", "/mcp:greet:greet", "name=Ann")
	var result headless.Result
	if err != nil || json.Unmarshal([]byte(stdout), &result) != nil || result.Status != headless.StatusCompleted || result.Command != "mcp:greet:greet" {
		t.Fatalf("MCP prompt: %q %v", stdout, err)
	}
	if !strings.Contains(transcript(), "Say hello to Ann.") {
		t.Fatalf("the MCP prompt is not in the transcript:\n%s", transcript())
	}

	stdout, _, err = runJad(t, "", "run", "--session", session, "--no-commands", "/reset")
	if err != nil || stdout != "ok\n" || !strings.Contains(transcript(), `"/reset"`) {
		t.Fatalf("--no-commands: %q %v", stdout, err)
	}

	stdout, _, err = runJad(t, "", "run", "--session", session, "/reset")
	if err != nil || stdout != "The session was reset.\n" || strings.Contains(transcript(), "Say hello") {
		t.Fatalf("reset: %q %v\n%s", stdout, err, transcript())
	}

	for name, args := range map[string][]string{
		"text after compact": {"run", "--session", session, "/compact", "now"},
		"model on an action": {"run", "--session", session, "--model", "openai/default", "/reset"},
	} {
		if _, _, err := runJad(t, "", args...); exitCode(err) != exitUsage {
			t.Errorf("%s: err = %v, want exit %d", name, err, exitUsage)
		}
	}
	for name, args := range map[string][]string{
		"unknown library item": {"run", "--session", session, "/prompt:nope"},
		"missing argument":     {"run", "--session", session, "/mcp:greet:greet"},
	} {
		if _, _, err := runJad(t, "", args...); exitCode(err) != exitFailed {
			t.Errorf("%s: err = %v, want exit %d", name, err, exitFailed)
		}
	}
}
