package mcp

import (
	"context"
	"strings"
	"testing"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
)

func TestUserPromptsListAndFillOnlyServersWithPrompts(t *testing.T) {
	server := featureServer(t)
	server.AddPrompt(&mcpSDK.Prompt{Name: "standup", Title: "Stand-up"}, func(context.Context, *mcpSDK.GetPromptRequest) (*mcpSDK.GetPromptResult, error) {
		return &mcpSDK.GetPromptResult{Messages: []*mcpSDK.PromptMessage{
			{Role: "user", Content: &mcpSDK.TextContent{Text: "What did I do yesterday?"}},
			{Role: "user", Content: &mcpSDK.EmbeddedResource{Resource: &mcpSDK.ResourceContents{URI: "notes://y", Text: "notes"}}},
			{Role: "user", Content: &mcpSDK.ImageContent{MIMEType: "image/png", Data: []byte{1}}},
		}}, nil
	})
	docs := bodyLimitTestServerReference(t, "docs", bodyLimitTestServer(t, server, nil).URL, 65_536)
	docs.Prompts = true
	// The same server without prompts: true, and one turned off, are left out.
	plain := bodyLimitTestServerReference(t, "plain", bodyLimitTestServer(t, featureServer(t), nil).URL, 65_536)
	off := docs
	off.ID, off.Enabled = "off", false
	broken := bodyLimitTestServerReference(t, "broken", "http://127.0.0.1:1/mcp", 65_536)
	broken.Prompts = true
	servers := []sessionfs.MCPServerReference{docs, plain, off, broken}

	manager := NewManager(nil)
	defer manager.Close()
	listed, err := manager.ListUserPrompts(context.Background(), servers, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Prompts) != 2 || listed.Prompts[0].Server != "docs" || listed.Prompts[0].Name != "review" || listed.Prompts[1].Title != "Stand-up" {
		t.Fatalf("prompts = %+v", listed.Prompts)
	}
	if arguments := listed.Prompts[0].Arguments; len(arguments) != 1 || arguments[0].Name != "path" || !arguments[0].Required {
		t.Fatalf("arguments = %+v", arguments)
	}
	if listed.Prompts[1].Arguments == nil {
		t.Fatal("a prompt without arguments must list an empty array, not null")
	}
	if len(listed.Failures) != 1 || listed.Failures[0].Server != "broken" || !strings.Contains(listed.Failures[0].Error, "failed to connect MCP server broken") {
		t.Fatalf("failures = %+v", listed.Failures)
	}

	// An assistant message in the prompt labels every message with its role.
	filled, err := manager.GetUserPrompt(context.Background(), servers, t.TempDir(), "docs", "review", map[string]string{"path": "main.go"})
	if err != nil || filled.Text != "[user]\nReview main.go\n\n[assistant]\nReading it." || filled.Description != "Review" {
		t.Fatalf("review = %+v, %v", filled, err)
	}
	// Only user messages: their texts, a resource's text, and a note for an image.
	filled, err = manager.GetUserPrompt(context.Background(), servers, t.TempDir(), "docs", "standup", nil)
	if err != nil || filled.Text != "What did I do yesterday?\n\nnotes\n\n[image (image/png) not inserted]" {
		t.Fatalf("standup = %q, %v", filled.Text, err)
	}

	for _, id := range []string{"plain", "off", "missing"} {
		_, err := manager.GetUserPrompt(context.Background(), servers, t.TempDir(), id, "review", nil)
		var promptErr *UserPromptError
		if !asUserPromptError(err, &promptErr) || !promptErr.NotFound {
			t.Errorf("%s: err = %v, want not found", id, err)
		}
	}
	_, err = manager.GetUserPrompt(context.Background(), servers, t.TempDir(), "docs", "nope", nil)
	var promptErr *UserPromptError
	if !asUserPromptError(err, &promptErr) || promptErr.NotFound || !strings.Contains(promptErr.Message, "the server refused:") {
		t.Fatalf("unknown prompt: %v", err)
	}
}

// A prompt larger than max_result_bytes is refused, whichever limit sees it first: the
// answer's transport limit, or the check of the text.
func TestUserPromptLargerThanMaxResultBytesIsRefused(t *testing.T) {
	server, _ := bodyLimitTestTool(t, "tool")
	server.AddPrompt(&mcpSDK.Prompt{Name: "big"}, func(context.Context, *mcpSDK.GetPromptRequest) (*mcpSDK.GetPromptResult, error) {
		return &mcpSDK.GetPromptResult{Messages: []*mcpSDK.PromptMessage{{Role: "user", Content: &mcpSDK.TextContent{Text: strings.Repeat("x", 5000)}}}}, nil
	})
	reference := bodyLimitTestServerReference(t, "big", bodyLimitTestServer(t, server, nil).URL, 4096)
	reference.Prompts = true
	manager := NewManager(nil)
	defer manager.Close()
	_, err := manager.GetUserPrompt(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir(), "big", "big", nil)
	if err == nil || !strings.Contains(err.Error(), "larger than the server's max_result_bytes (4096)") {
		t.Fatalf("err = %v", err)
	}
}

func TestUserPromptsOfAServerWithoutPromptsFail(t *testing.T) {
	server, _ := bodyLimitTestTool(t, "tool")
	reference := bodyLimitTestServerReference(t, "plain", bodyLimitTestServer(t, server, nil).URL, 65_536)
	reference.Prompts = true
	manager := NewManager(nil)
	defer manager.Close()
	listed, err := manager.ListUserPrompts(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir())
	if err != nil || len(listed.Prompts) != 0 || len(listed.Failures) != 1 || !strings.Contains(listed.Failures[0].Error, "does not offer prompts") {
		t.Fatalf("listed = %+v, %v", listed, err)
	}
}

func asUserPromptError(err error, target **UserPromptError) bool {
	if err == nil {
		return false
	}
	value, ok := err.(*UserPromptError)
	*target = value
	return ok
}
