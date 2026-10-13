package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const mcpFixtureEnvironment = "JAD_MCP_FIXTURE"

func TestMCPFixtureProcess(t *testing.T) {
	if os.Getenv(mcpFixtureEnvironment) != "1" {
		return
	}
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "fixture", Version: "1.0.0"}, nil)
	server.AddTool(&mcpSDK.Tool{
		Name:        "echo",
		Description: "fixture setting=" + os.Getenv("MCP_TEST_SETTING"),
		InputSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"text":{"type":"string"},"pair":{"type":"array","prefixItems":[{"type":"string"},{"type":"integer"}],"items":false}},"required":["text","pair"],"additionalProperties":false}`),
	}, func(_ context.Context, request *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		var arguments map[string]any
		if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
			return nil, err
		}
		text, _ := arguments["text"].(string)
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{
			&mcpSDK.TextContent{Text: text},
			&mcpSDK.ImageContent{MIMEType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
			&mcpSDK.AudioContent{MIMEType: "audio/wav", Data: []byte{1, 2}},
			&mcpSDK.EmbeddedResource{Resource: &mcpSDK.ResourceContents{URI: "file:///tmp/guide.pdf", MIMEType: "application/pdf", Blob: []byte("%PDF")}},
		}}, nil
	})
	server.AddTool(&mcpSDK.Tool{
		Name:        "hidden",
		Description: "deny-list fixture",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(_ context.Context, _ *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "hidden"}}}, nil
	})
	if err := server.Run(context.Background(), &mcpSDK.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestManagerConnectsStdioAndMapsMultimodalToolResults(t *testing.T) {
	cwd := t.TempDir()
	configPath := writeStdioConfig(t, cwd, "stdio-setting")
	secondConfigPath := filepath.Join(cwd, "second-server.json")
	copyFile(t, configPath, secondConfigPath)
	manager := NewManager(currentEnvironment())
	servers := []sessionfs.MCPServerReference{
		{ID: "fixture", ConfigPath: configPath, Enabled: true, DenyTools: []string{"hidden"}},
		{ID: "other", ConfigPath: secondConfigPath, Enabled: true, DenyTools: []string{}},
	}
	defer manager.Close()

	loaded, err := manager.LoadTools(context.Background(), servers, cwd)
	if err != nil {
		t.Fatalf("LoadTools() error = %v", err)
	}
	if len(loaded) != 3 {
		t.Fatalf("loaded tool count = %d, want 3", len(loaded))
	}
	var echo, otherEcho *tools.Tool
	for index := range loaded {
		tool := &loaded[index]
		if tool.Definition.ID == "mcp:fixture:echo" {
			echo = tool
		}
		if tool.Definition.ID == "mcp:other:echo" {
			otherEcho = tool
		}
	}
	if echo == nil || otherEcho == nil {
		t.Fatalf("loaded tools do not contain both namespaced echo tools: %+v", loaded)
	}
	if echo.Definition.Source != "mcp" || echo.Definition.Server != "fixture" || !strings.HasPrefix(echo.Definition.Name, "mcp_fixture_echo_") {
		t.Fatalf("fixture definition = %+v", echo.Definition)
	}
	if !strings.Contains(echo.Definition.Description, "stdio-setting") || echo.Definition.Name == otherEcho.Definition.Name {
		t.Fatalf("fixture metadata = %+v; other name %q", echo.Definition, otherEcho.Definition.Name)
	}
	if got := manager.connections["fixture"]; got == nil {
		t.Fatal("fixture connection was not retained")
	} else {
		loadedAgain, err := manager.LoadTools(context.Background(), servers, cwd)
		if err != nil {
			t.Fatalf("LoadTools(second call) error = %v", err)
		}
		if manager.connections["fixture"] != got || len(loadedAgain) != len(loaded) {
			t.Fatal("unchanged server configuration did not reuse its connection")
		}
	}

	catalog, err := tools.NewCatalog(loaded)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	call := marshalFunctionCall(t, "call-mcp", echo.Definition.Name, `{"text":"hello from MCP","pair":["word",42]}`)
	output, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{})
	if err != nil {
		t.Fatalf("ExecuteFunctionCall() error = %v", err)
	}
	var decoded struct {
		Type   string          `json:"type"`
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("Unmarshal(function output) error = %v", err)
	}
	if decoded.Type != "function_call_output" || decoded.CallID != "call-mcp" {
		t.Fatalf("function output envelope = %+v", decoded)
	}
	var parts []map[string]any
	if err := json.Unmarshal(decoded.Output, &parts); err != nil {
		t.Fatalf("MCP output must be a JSON content array, got %s: %v", decoded.Output, err)
	}
	if len(parts) != 4 || parts[0]["type"] != "input_text" || parts[0]["text"] != "hello from MCP" || parts[1]["type"] != "input_image" || parts[2]["type"] != "input_text" || parts[3]["type"] != "input_file" {
		t.Fatalf("adapted MCP output = %#v", parts)
	}

	validationIssues, validationErr := echo.Validator.Validate([]byte(`{"text":"bad tuple","pair":["word",42,true]}`))
	if validationErr != nil || len(validationIssues) != 1 {
		t.Fatalf("Draft 2020-12 validation = (%#v, %v), want one AJV-shaped items issue", validationIssues, validationErr)
	}
	if issue := validationIssues[0]; issue.Keyword != "items" || issue.InstancePath != "/pair" || issue.SchemaPath != "#/properties/pair/items" || issue.Params["limit"] != 2 {
		t.Fatalf("Draft 2020-12 validation issue = %+v, want items error with limit 2", issue)
	}
	invalid := marshalFunctionCall(t, "call-invalid", echo.Definition.Name, `{"text":"bad tuple","pair":["word",42,true]}`)
	invalidOutput, err := catalog.ExecuteFunctionCall(context.Background(), invalid, tools.ExecutionContext{})
	if err != nil {
		t.Fatalf("ExecuteFunctionCall(invalid schema) error = %v", err)
	}
	if !strings.Contains(string(invalidOutput), "invalid_input") {
		t.Fatalf("Draft 2020-12 prefixItems did not reject extra tuple entries: %s", invalidOutput)
	}

	if manager.Close() != nil {
		t.Fatal("Manager.Close() returned an error")
	}
	if _, err := manager.LoadTools(context.Background(), servers, cwd); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("LoadTools after Close() error = %v, want closed error", err)
	}
}

func TestManagerConnectsStreamableHTTPWithHeaderFromEnvironment(t *testing.T) {
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "http-fixture", Version: "1.0.0"}, nil)
	server.AddTool(&mcpSDK.Tool{
		Name:        "status",
		Description: "authenticated HTTP fixture",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(_ context.Context, _ *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "authorized"}}}, nil
	})
	mcpHandler := mcpSDK.NewStreamableHTTPHandler(func(*http.Request) *mcpSDK.Server { return server }, nil)
	var authorizedRequests atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer fixture-token" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		authorizedRequests.Add(1)
		mcpHandler.ServeHTTP(writer, request)
	}))
	defer httpServer.Close()

	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "http-server.json")
	configBytes, err := json.Marshal(map[string]any{
		"transport": map[string]any{
			"type":         "streamable_http",
			"url":          httpServer.URL,
			"headers_from": map[string]string{"Authorization": "MCP_AUTH_TOKEN"},
		},
		"timeout_ms":       10_000,
		"max_result_bytes": 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(map[string]string{"MCP_AUTH_TOKEN": "Bearer fixture-token"})
	defer manager.Close()
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{{ID: "http-fixture", ConfigPath: configPath, Enabled: true}}, cwd)
	if err != nil {
		t.Fatalf("LoadTools(streamable HTTP) error = %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded HTTP tools = %d, want 1", len(loaded))
	}
	catalog, err := tools.NewCatalog(loaded)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	output, err := catalog.ExecuteFunctionCall(context.Background(), marshalFunctionCall(t, "http-call", loaded[0].Definition.Name, `{}`), tools.ExecutionContext{})
	if err != nil {
		t.Fatalf("ExecuteFunctionCall(streamable HTTP) error = %v", err)
	}
	var decoded struct {
		Output []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"output"`
	}
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("Unmarshal(streamable HTTP output) error = %v", err)
	}
	if len(decoded.Output) != 1 || decoded.Output[0].Type != "input_text" || decoded.Output[0].Text != "authorized" {
		t.Fatalf("streamable HTTP output = %+v", decoded.Output)
	}
	if authorizedRequests.Load() == 0 {
		t.Fatal("HTTP MCP requests did not include the configured Authorization header")
	}
}

func TestManagerDoesNotReadDisabledServerConfig(t *testing.T) {
	manager := NewManager(nil)
	defer manager.Close()
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{{ID: "disabled", ConfigPath: filepath.Join(t.TempDir(), "missing.json"), Enabled: false}}, t.TempDir())
	if err != nil {
		t.Fatalf("LoadTools(disabled) error = %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("disabled server produced tools: %+v", loaded)
	}
}

func writeStdioConfig(t *testing.T, directory, setting string) string {
	t.Helper()
	configPath := filepath.Join(directory, "server.json")
	config := map[string]any{
		"transport": map[string]any{
			"type":    "stdio",
			"command": mustExecutable(t),
			"args":    []string{"-test.run=^TestMCPFixtureProcess$"},
			"cwd":     directory,
			"env":     map[string]string{mcpFixtureEnvironment: "1", "MCP_TEST_SETTING": setting},
		},
		"timeout_ms":       10_000,
		"max_result_bytes": 4096,
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func currentEnvironment() map[string]string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = value
		}
	}
	return values
}

func copyFile(t *testing.T, source, target string) {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func marshalFunctionCall(t *testing.T, callID, name, arguments string) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Type: "function_call", CallID: callID, Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// Two profiles may pick different environments of one server (same id, two files). Each
// profile has its own Manager. A shared Manager would replace the first connection when
// the second profile loads, and the first profile's tool would then fail while it runs.
func TestManagersKeepTwoEnvironmentsOfOneServerApart(t *testing.T) {
	cwd := t.TempDir()
	headedDir, headlessDir := filepath.Join(cwd, "headed"), filepath.Join(cwd, "headless")
	for _, directory := range []string{headedDir, headlessDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	headed := []sessionfs.MCPServerReference{{ID: "betterwright", ConfigPath: writeStdioConfig(t, headedDir, "headed"), Enabled: true}}
	headless := []sessionfs.MCPServerReference{{ID: "betterwright", ConfigPath: writeStdioConfig(t, headlessDir, "headless"), Enabled: true}}

	callEcho := func(loaded []tools.Tool) string {
		t.Helper()
		var name string
		for _, tool := range loaded {
			if tool.Definition.ID == "mcp:betterwright:echo" {
				name = tool.Definition.Name
			}
		}
		if name == "" {
			t.Fatalf("no echo tool in %+v", loaded)
		}
		catalog, err := tools.NewCatalog(loaded)
		if err != nil {
			t.Fatal(err)
		}
		output, err := catalog.ExecuteFunctionCall(context.Background(), marshalFunctionCall(t, "c", name, `{"text":"hi","pair":["word",42]}`), tools.ExecutionContext{})
		if err != nil {
			t.Fatal(err)
		}
		return string(output)
	}

	coder, reviewer := NewManager(currentEnvironment()), NewManager(currentEnvironment())
	defer coder.Close()
	defer reviewer.Close()
	coderTools, err := coder.LoadTools(context.Background(), headed, cwd)
	if err != nil {
		t.Fatal(err)
	}
	reviewerTools, err := reviewer.LoadTools(context.Background(), headless, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(callEcho(coderTools), "hi") || !strings.Contains(callEcho(reviewerTools), "hi") {
		t.Fatal("both profiles must be able to call their own connection after the other loaded")
	}
	descriptions := func(loaded []tools.Tool) string {
		for _, tool := range loaded {
			if tool.Definition.ID == "mcp:betterwright:echo" {
				return tool.Definition.Description
			}
		}
		return ""
	}
	if !strings.Contains(descriptions(coderTools), "headed") || !strings.Contains(descriptions(reviewerTools), "headless") {
		t.Fatalf("environments mixed: %q / %q", descriptions(coderTools), descriptions(reviewerTools))
	}

	// What a shared Manager does: the second load replaces the first connection.
	shared := NewManager(currentEnvironment())
	defer shared.Close()
	first, err := shared.LoadTools(context.Background(), headed, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shared.LoadTools(context.Background(), headless, cwd); err != nil {
		t.Fatal(err)
	}
	if out := callEcho(first); !strings.Contains(out, `\"status\":\"error\"`) {
		t.Fatalf("expected the replaced connection to fail, got %s", out)
	}
}
