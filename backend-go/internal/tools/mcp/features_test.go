package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// featureServer offers one tool, two resources, a template and a prompt.
func featureServer(t *testing.T) *mcpSDK.Server {
	server, _ := bodyLimitTestTool(t, "tool")
	server.AddResource(&mcpSDK.Resource{URI: "docs://guide", Name: "guide", Title: "Guide", MIMEType: "text/markdown"}, func(_ context.Context, request *mcpSDK.ReadResourceRequest) (*mcpSDK.ReadResourceResult, error) {
		return &mcpSDK.ReadResourceResult{Contents: []*mcpSDK.ResourceContents{{URI: request.Params.URI, MIMEType: "text/markdown", Text: "# Guide\nread me"}}}, nil
	})
	server.AddResource(&mcpSDK.Resource{URI: "docs://logo", Name: "logo", MIMEType: "image/png"}, func(_ context.Context, request *mcpSDK.ReadResourceRequest) (*mcpSDK.ReadResourceResult, error) {
		return &mcpSDK.ReadResourceResult{Contents: []*mcpSDK.ResourceContents{{URI: request.Params.URI, MIMEType: "image/png", Blob: []byte{0x89, 'P', 'N', 'G'}}}}, nil
	})
	server.AddResourceTemplate(&mcpSDK.ResourceTemplate{URITemplate: "docs://pages/{name}", Name: "page"}, func(_ context.Context, request *mcpSDK.ReadResourceRequest) (*mcpSDK.ReadResourceResult, error) {
		return &mcpSDK.ReadResourceResult{Contents: []*mcpSDK.ResourceContents{{URI: request.Params.URI, Text: "page " + strings.TrimPrefix(request.Params.URI, "docs://pages/")}}}, nil
	})
	server.AddPrompt(&mcpSDK.Prompt{Name: "review", Description: "Review a file", Arguments: []*mcpSDK.PromptArgument{{Name: "path", Required: true}}}, func(_ context.Context, request *mcpSDK.GetPromptRequest) (*mcpSDK.GetPromptResult, error) {
		return &mcpSDK.GetPromptResult{Description: "Review", Messages: []*mcpSDK.PromptMessage{
			{Role: "user", Content: &mcpSDK.TextContent{Text: "Review " + request.Params.Arguments["path"]}},
			{Role: "assistant", Content: &mcpSDK.TextContent{Text: "Reading it."}},
		}}, nil
	})
	return server
}

func featureTool(t *testing.T, loaded []tools.Tool, id string) tools.Tool {
	t.Helper()
	for _, tool := range loaded {
		if tool.Definition.ID == id {
			return tool
		}
	}
	t.Fatalf("no tool %s among %d tools", id, len(loaded))
	return tools.Tool{}
}

func accepts(t *testing.T, tool tools.Tool, arguments map[string]any) bool {
	t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	issues, err := tool.Validator.Validate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return len(issues) == 0
}

func callFeature(t *testing.T, tool tools.Tool, arguments map[string]any) string {
	t.Helper()
	if !accepts(t, tool, arguments) {
		t.Fatalf("%s rejected %v", tool.Definition.ID, arguments)
	}
	output, err := tool.ExecuteOutput(context.Background(), arguments, tools.ExecutionContext{})
	if err != nil {
		t.Fatalf("%s: %v", tool.Definition.ID, err)
	}
	return string(output)
}

func TestManagerAddsResourceAndPromptToolsWhenTheToolsetAsks(t *testing.T) {
	httpServer := bodyLimitTestServer(t, featureServer(t), nil)
	reference := bodyLimitTestServerReference(t, "docs", httpServer.URL, 65_536)
	manager := NewManager(nil)
	defer manager.Close()

	plain, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir())
	if err != nil || len(plain) != 1 {
		t.Fatalf("without resources or prompts: %d tools, %v; want the server's one tool", len(plain), err)
	}

	reference.Resources, reference.Prompts = true, true
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 5 {
		t.Fatalf("loaded %d tools, want the tool and four feature tools", len(loaded))
	}
	names := map[string]bool{}
	for _, tool := range loaded {
		if names[tool.Definition.Name] || !strings.HasPrefix(tool.Definition.Name, "mcp_docs_") {
			t.Fatalf("tool name %q is repeated or not an MCP name", tool.Definition.Name)
		}
		names[tool.Definition.Name] = true
		if tool.Definition.Server != "docs" || tool.Definition.Source != "mcp" {
			t.Fatalf("tool %s: server %q source %q", tool.Definition.ID, tool.Definition.Server, tool.Definition.Source)
		}
	}

	list := featureTool(t, loaded, "mcp:docs:resources/list")
	if got := callFeature(t, list, map[string]any{}); !strings.Contains(got, `docs://guide`) || !strings.Contains(got, `docs://logo`) || !strings.Contains(got, `\"kind\":\"resources\"`) {
		t.Fatalf("resource list = %s", got)
	}
	if got := callFeature(t, list, map[string]any{"kind": "templates"}); !strings.Contains(got, `docs://pages/{name}`) {
		t.Fatalf("template list = %s", got)
	}
	if accepts(t, list, map[string]any{"kind": "files"}) {
		t.Fatal("an unknown kind was accepted")
	}

	read := featureTool(t, loaded, "mcp:docs:resources/read")
	if got := callFeature(t, read, map[string]any{"uri": "docs://guide"}); !strings.Contains(got, `read me`) {
		t.Fatalf("text resource = %s", got)
	}
	if got := callFeature(t, read, map[string]any{"uri": "docs://logo"}); !strings.Contains(got, `"type":"input_image"`) || !strings.Contains(got, `data:image/png;base64,`) {
		t.Fatalf("image resource = %s", got)
	}
	if got := callFeature(t, read, map[string]any{"uri": "docs://pages/intro"}); !strings.Contains(got, `page intro`) {
		t.Fatalf("templated resource = %s", got)
	}
	if got := callFeature(t, read, map[string]any{"uri": "docs://missing"}); !strings.Contains(got, `\"status\":\"error\"`) || !strings.Contains(got, `resources/read`) {
		t.Fatalf("missing resource = %s", got)
	}

	prompts := featureTool(t, loaded, "mcp:docs:prompts/list")
	if got := callFeature(t, prompts, map[string]any{}); !strings.Contains(got, `\"name\":\"review\"`) || !strings.Contains(got, `\"required\":true`) {
		t.Fatalf("prompt list = %s", got)
	}
	get := featureTool(t, loaded, "mcp:docs:prompts/get")
	got := callFeature(t, get, map[string]any{"name": "review", "arguments": map[string]any{"path": "main.go"}})
	if !strings.Contains(got, `[user]`) || !strings.Contains(got, `Review main.go`) || !strings.Contains(got, `[assistant]`) {
		t.Fatalf("prompt = %s", got)
	}
	if accepts(t, get, map[string]any{"name": "review", "arguments": map[string]any{"path": 1}}) {
		t.Fatal("a non-string prompt argument was accepted")
	}
}

func TestManagerRefusesAFeatureTheServerDoesNotOffer(t *testing.T) {
	server, _ := bodyLimitTestTool(t, "tool")
	httpServer := bodyLimitTestServer(t, server, nil)
	for _, feature := range []string{"resources", "prompts"} {
		reference := bodyLimitTestServerReference(t, "plain", httpServer.URL, 65_536)
		reference.Resources = feature == "resources"
		reference.Prompts = feature == "prompts"
		manager := NewManager(nil)
		_, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir())
		manager.Close()
		if err == nil || !strings.Contains(err.Error(), "does not offer "+feature) {
			t.Fatalf("%s on a server without them: %v", feature, err)
		}
	}
}

// The resource listing is a result like any other: it is cut to max_result_bytes. Quotes
// in the descriptions are escaped twice in the listing (once in its JSON, once in the
// part), so the listing outgrows the HTTP answer that carried it and the cut is reached.
func TestResourceListIsBoundedByMaxResultBytes(t *testing.T) {
	server := featureServer(t)
	for index := range 20 {
		uri := "docs://bulk/" + string(rune('a'+index))
		server.AddResource(&mcpSDK.Resource{URI: uri, Name: uri, Description: strings.Repeat(`"`, 200)}, nil)
	}
	httpServer := bodyLimitTestServer(t, server, nil)
	const limit = 12_000
	reference := bodyLimitTestServerReference(t, "docs", httpServer.URL, limit)
	reference.Resources = true
	manager := NewManager(nil)
	defer manager.Close()
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := callFeature(t, featureTool(t, loaded, "mcp:docs:resources/list"), map[string]any{})
	if len(got) > limit || !strings.Contains(got, "max_result_bytes") {
		t.Fatalf("listing of %d bytes was not cut to the limit: %.200s", len(got), got)
	}
	var parts []map[string]any
	if json.Unmarshal([]byte(got), &parts) != nil {
		t.Fatalf("listing is not a part array: %.200s", got)
	}
}
