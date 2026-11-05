package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	commands := filepath.Join(fixture.Root, ".agent", "commands", "code")
	if err := os.MkdirAll(commands, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(commands, "explain.md"), []byte("---\ndescription: Explain a file\n---\nExplain $1 to a $2 reader.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = runJad(t, "", "run", "--session", session, "--format", "json", "/code:explain", "util.go", `"new team"`)
	result = headless.Result{}
	if err != nil || json.Unmarshal([]byte(stdout), &result) != nil || result.Command != "code:explain" {
		t.Fatalf("custom command: %q %v", stdout, err)
	}
	if !strings.Contains(transcript(), quoted("Explain util.go to a new team reader.")) {
		t.Fatalf("the custom command's text is not in the transcript:\n%s", transcript())
	}
	stdout, _, err = runJad(t, "", "run", "--session", session, "--format", "json", "/code:unknown", "x")
	result = headless.Result{}
	if err != nil || json.Unmarshal([]byte(stdout), &result) != nil || result.Command != "" || !strings.Contains(transcript(), `"/code:unknown x"`) {
		t.Fatalf("unknown word: %q %v", stdout, err)
	}

	// A prompt piped on standard input is data: a document that starts with /reset is sent
	// as text, and the session keeps its history.
	stdout, _, err = runJad(t, "/reset everything above\n", "run", "--session", session)
	if err != nil || stdout != "ok\n" || !strings.Contains(transcript(), quoted("/reset everything above\n")) || !strings.Contains(transcript(), "Say hello") {
		t.Fatalf("piped /reset: %q %v\n%s", stdout, err, transcript())
	}
	stdout, _, err = runJad(t, "/compact", "run", "--session", session, "-")
	if err != nil || stdout != "ok\n" || !strings.Contains(transcript(), `"/compact"`) {
		t.Fatalf("piped /compact with -: %q %v", stdout, err)
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

// docsServer is an MCP server with one resource and one resource template.
func docsServer(t *testing.T) string {
	t.Helper()
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "docs", Version: "1.0.0"}, nil)
	server.AddResource(&mcpSDK.Resource{URI: "docs://guide", Name: "Style guide"}, func(_ context.Context, request *mcpSDK.ReadResourceRequest) (*mcpSDK.ReadResourceResult, error) {
		return &mcpSDK.ReadResourceResult{Contents: []*mcpSDK.ResourceContents{{URI: request.Params.URI, Text: "Use tabs."}}}, nil
	})
	server.AddResource(&mcpSDK.Resource{URI: "docs://logo.png", Name: "logo", MIMEType: "image/png"}, func(_ context.Context, request *mcpSDK.ReadResourceRequest) (*mcpSDK.ReadResourceResult, error) {
		png, _ := base64.StdEncoding.DecodeString(onePixelPNG)
		return &mcpSDK.ReadResourceResult{Contents: []*mcpSDK.ResourceContents{{URI: request.Params.URI, MIMEType: "image/png", Blob: png}}}, nil
	})
	server.AddResourceTemplate(&mcpSDK.ResourceTemplate{URITemplate: "docs://pages/{name}", Name: "page"}, func(_ context.Context, request *mcpSDK.ReadResourceRequest) (*mcpSDK.ReadResourceResult, error) {
		return &mcpSDK.ReadResourceResult{Contents: []*mcpSDK.ResourceContents{{URI: request.Params.URI, Text: "Page " + strings.TrimPrefix(request.Params.URI, "docs://pages/") + "."}}}, nil
	})
	handler := mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return server }, &mcpSDK.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer.URL
}

func TestRunResourceCommands(t *testing.T) {
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
		MCP:       map[string]map[string]any{"chat/docs": {"resources": true}},
		MCPConfig: map[string]map[string]any{"chat/docs": {"schema_version": 1, "transport": map[string]any{"type": "streamable_http", "url": docsServer(t)}}},
	})
	t.Setenv("FIXTURE_KEY", "k")
	session := "local:" + fixture.SessionID
	transcript := func() string {
		t.Helper()
		data, _ := os.ReadFile(filepath.Join(fixture.SessionStateDir, "dev", "coder", "messages.jsonl"))
		return string(data)
	}
	stdout, stderr, err := runJad(t, "", "run", "--session", session, "--format", "json", "/resource:docs:style-guide", "apply", "it")
	var result headless.Result
	if err != nil || json.Unmarshal([]byte(stdout), &result) != nil || result.Command != "resource:docs:style-guide" {
		t.Fatalf("resource: %q %q %v", stdout, stderr, err)
	}
	if !strings.Contains(transcript(), `Use tabs.\n\napply it`) {
		t.Fatalf("the resource text is not in the transcript:\n%s", transcript())
	}
	if _, _, err := runJad(t, "", "run", "--session", session, "/resource:docs:page", "name=intro"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(transcript(), `"Page intro."`) {
		t.Fatalf("the template's text is not in the transcript:\n%s", transcript())
	}
	for name, args := range map[string][]string{
		"unknown resource":  {"run", "--session", session, "/resource:docs:nope"},
		"missing variables": {"run", "--session", session, "/resource:docs:page"},
		"unknown uri":       {"run", "--session", session, "/resource:docs:docs://none"},
	} {
		if _, _, err := runJad(t, "", args...); exitCode(err) != exitFailed {
			t.Errorf("%s: err = %v, want exit %d", name, err, exitFailed)
		}
	}
}

// onePixelPNG is a valid 1x1 PNG, base64.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// An image resource is sent as an image part of the message, with the text after the command.
func TestRunResourceCommandAttachesAnImage(t *testing.T) {
	var mu sync.Mutex
	var last []byte
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = body
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"a dot\"}]}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"model\":\"m\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7},\"output\":[]}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)
	fixture := useFixture(t, layouttest.Spec{
		Models:    map[string]map[string]any{"openai/default": {"provider": "openai", "model": "m", "api_key_env": "FIXTURE_KEY", "base_url": provider.URL + "/v1"}},
		Server:    map[string]any{"log_level": "off"},
		MCP:       map[string]map[string]any{"chat/docs": {"resources": true}},
		MCPConfig: map[string]map[string]any{"chat/docs": {"schema_version": 1, "transport": map[string]any{"type": "streamable_http", "url": docsServer(t)}}},
	})
	t.Setenv("FIXTURE_KEY", "k")
	stdout, stderr, err := runJad(t, "", "run", "--session", "local:"+fixture.SessionID, "/resource:docs:logo", "describe", "it")
	if err != nil || strings.TrimSpace(stdout) != "a dot" {
		t.Fatalf("run: %q %q %v", stdout, stderr, err)
	}
	mu.Lock()
	defer mu.Unlock()
	var request struct {
		Input []struct {
			Role    string `json:"role"`
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL string `json:"image_url"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(last, &request); err != nil {
		t.Fatalf("%v: %s", err, last)
	}
	user := request.Input[len(request.Input)-1]
	if user.Role != "user" || len(user.Content) != 2 || user.Content[0].Text != "describe it" ||
		user.Content[1].Type != "input_image" || !strings.HasPrefix(user.Content[1].ImageURL, "data:image/png;base64,") {
		t.Fatalf("the message is not the text and the image: %+v", user)
	}
}

// Only a terminal counts as typed input; a pipe, a file and any other reader are data.
func TestIsTerminalRefusesPipesFilesAndReaders(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	file, err := os.CreateTemp(t.TempDir(), "prompt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for name, input := range map[string]io.Reader{"pipe": reader, "file": file, "reader": strings.NewReader("/reset")} {
		if isTerminal(input) {
			t.Errorf("%s counts as a terminal", name)
		}
	}
}
