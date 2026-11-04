package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
)

func TestUserResourcesListAndReadOnlyServersWithResources(t *testing.T) {
	docs := bodyLimitTestServerReference(t, "docs", bodyLimitTestServer(t, featureServer(t), nil).URL, 65_536)
	docs.Resources = true
	plain := bodyLimitTestServerReference(t, "plain", bodyLimitTestServer(t, featureServer(t), nil).URL, 65_536)
	off := docs
	off.ID, off.Enabled = "off", false
	broken := bodyLimitTestServerReference(t, "broken", "http://127.0.0.1:1/mcp", 65_536)
	broken.Resources = true
	servers := []sessionfs.MCPServerReference{docs, plain, off, broken}

	manager := NewManager(nil)
	defer manager.Close()
	listed, err := manager.ListUserResources(context.Background(), servers, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, resource := range listed.Resources {
		names = append(names, resource.Server+"/"+resource.Name)
	}
	if strings.Join(names, ",") != "docs/guide,docs/logo,docs/page" {
		t.Fatalf("resources = %v", names)
	}
	guide, page := listed.Resources[0], listed.Resources[2]
	if guide.URI != "docs://guide" || guide.Title != "Guide" || guide.MIMEType != "text/markdown" || guide.URITemplate != "" {
		t.Fatalf("guide = %+v", guide)
	}
	if page.URI != "" || page.URITemplate != "docs://pages/{name}" || strings.Join(page.Variables, ",") != "name" {
		t.Fatalf("page = %+v", page)
	}
	if len(listed.Failures) != 1 || listed.Failures[0].Server != "broken" {
		t.Fatalf("failures = %+v", listed.Failures)
	}

	read, err := manager.ReadUserResource(context.Background(), servers, t.TempDir(), UserResourceRead{Server: "docs", URI: "docs://guide"})
	if err != nil || read.Text != "# Guide\nread me" || read.MIMEType != "text/markdown" || read.URI != "docs://guide" {
		t.Fatalf("guide = %+v, %v", read, err)
	}
	read, err = manager.ReadUserResource(context.Background(), servers, t.TempDir(), UserResourceRead{Server: "docs", URITemplate: "docs://pages/{name}", Variables: map[string]string{"name": "intro"}})
	if err != nil || read.Text != "page intro" || read.URI != "docs://pages/intro" {
		t.Fatalf("page = %+v, %v", read, err)
	}

	refused := func(request UserResourceRead) *UserPromptError {
		t.Helper()
		_, err := manager.ReadUserResource(context.Background(), servers, t.TempDir(), request)
		var userErr *UserPromptError
		if !asUserPromptError(err, &userErr) {
			t.Fatalf("%+v: err = %v, want a UserPromptError", request, err)
		}
		return userErr
	}
	for _, id := range []string{"plain", "off", "missing"} {
		if err := refused(UserResourceRead{Server: id, URI: "docs://guide"}); !err.NotFound {
			t.Errorf("%s: %+v, want not found", id, err)
		}
	}
	for _, request := range []UserResourceRead{
		{Server: "docs"},
		{Server: "docs", URI: "docs://guide", URITemplate: "docs://pages/{name}"},
		{Server: "docs", URI: "docs://guide", Variables: map[string]string{"x": "y"}},
		{Server: "docs", URITemplate: "docs://pages/{name}", Variables: map[string]string{"other": "y"}},
		{Server: "docs", URITemplate: "docs://pages/{name"},
	} {
		if err := refused(request); !err.Invalid {
			t.Errorf("%+v: %+v, want invalid", request, err)
		}
	}
	if err := refused(UserResourceRead{Server: "docs", URI: "docs://logo"}); err.NotFound || err.Invalid || !strings.Contains(err.Message, "has no text to insert") {
		t.Errorf("binary resource: %+v", err)
	}
	if err := refused(UserResourceRead{Server: "docs", URI: "docs://none"}); err.NotFound || !strings.Contains(err.Message, "the server refused:") {
		t.Errorf("unknown resource: %+v", err)
	}
}

func TestUserResourcesOfAServerWithoutResourcesFailAndLimitsApply(t *testing.T) {
	server, _ := bodyLimitTestTool(t, "tool")
	reference := bodyLimitTestServerReference(t, "plain", bodyLimitTestServer(t, server, nil).URL, 65_536)
	reference.Resources = true
	manager := NewManager(nil)
	defer manager.Close()
	listed, err := manager.ListUserResources(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir())
	if err != nil || len(listed.Resources) != 0 || len(listed.Failures) != 1 || !strings.Contains(listed.Failures[0].Error, "does not offer resources") {
		t.Fatalf("listed = %+v, %v", listed, err)
	}

	big, _ := bodyLimitTestTool(t, "tool")
	big.AddResource(&mcpSDK.Resource{URI: "big://x", Name: "x"}, func(_ context.Context, request *mcpSDK.ReadResourceRequest) (*mcpSDK.ReadResourceResult, error) {
		return &mcpSDK.ReadResourceResult{Contents: []*mcpSDK.ResourceContents{{URI: request.Params.URI, Text: strings.Repeat("x", 5000)}}}, nil
	})
	for index := 0; index < 3; index++ {
		uri := "big://more" + string(rune('a'+index))
		big.AddResource(&mcpSDK.Resource{URI: uri, Name: uri}, func(context.Context, *mcpSDK.ReadResourceRequest) (*mcpSDK.ReadResourceResult, error) {
			return &mcpSDK.ReadResourceResult{}, nil
		})
	}
	limited := bodyLimitTestServerReference(t, "big", bodyLimitTestServer(t, big, nil).URL, 4096)
	limited.Resources = true
	_, err = manager.ReadUserResource(context.Background(), []sessionfs.MCPServerReference{limited}, t.TempDir(), UserResourceRead{Server: "big", URI: "big://x"})
	if err == nil || !strings.Contains(err.Error(), "max_result_bytes (4096)") {
		t.Fatalf("big read: %v", err)
	}
	data, err := json.Marshal(map[string]any{
		"transport":  map[string]any{"type": "streamable_http", "url": bodyLimitTestServer(t, big, nil).URL},
		"timeout_ms": 10_000, "max_tools": 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "few.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	few := sessionfs.MCPServerReference{ID: "few", ConfigPath: path, Enabled: true, Resources: true}
	listed, err = manager.ListUserResources(context.Background(), []sessionfs.MCPServerReference{few}, t.TempDir())
	if err != nil || len(listed.Failures) != 1 || !strings.Contains(listed.Failures[0].Error, "more than max_tools (2) resources and templates") {
		t.Fatalf("max_tools: %+v, %v", listed, err)
	}
}
