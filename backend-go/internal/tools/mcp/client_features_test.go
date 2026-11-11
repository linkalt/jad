package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const clientFeaturesFixtureEnvironment = "JAD_MCP_CLIENT_FEATURES_FIXTURE"

// TestMCPClientFeaturesFixtureProcess is the stdio server of the client feature tests. Its
// probe tool lists the client's roots, asks it to sample and to elicit, logs a message and
// adds a tool (a tool list change), and answers with what it got; reading doc://elicit asks a
// question during the read. It does nothing unless
// started as the fixture.
func TestMCPClientFeaturesFixtureProcess(t *testing.T) {
	if os.Getenv(clientFeaturesFixtureEnvironment) != "1" {
		return
	}
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "probe", Version: "1.0.0"}, nil)
	server.AddTool(&mcpSDK.Tool{Name: "probe", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, request *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		responses := request.Params.InputResponses
		if len(responses) == 0 {
			// The first call asks for input (multi round-trip requests); the client answers
			// and calls again with the responses.
			all := mcpSDK.InputRequestMap{
				"roots": &mcpSDK.ListRootsParams{},
				"sample": &mcpSDK.CreateMessageParams{
					SystemPrompt: "be brief", MaxTokens: 50,
					Messages: []*mcpSDK.SamplingMessage{{Role: "user", Content: &mcpSDK.TextContent{Text: "say hi"}}},
				},
				"elicit": &mcpSDK.ElicitParams{
					Message:         "Which color?",
					RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{"color": map[string]any{"type": "string", "default": "red"}}},
				},
			}
			var arguments struct {
				Ask []string `json:"ask"`
			}
			_ = json.Unmarshal(request.Params.Arguments, &arguments)
			if len(arguments.Ask) == 0 {
				return &mcpSDK.CallToolResult{InputRequests: all}, nil
			}
			asked := mcpSDK.InputRequestMap{}
			for _, key := range arguments.Ask {
				asked[key] = all[key]
			}
			return &mcpSDK.CallToolResult{InputRequests: asked}, nil
		}
		var report []string
		for _, key := range []string{"roots", "sample", "elicit"} {
			if _, given := responses[key]; !given {
				continue
			}
			switch response := responses[key].(type) {
			case *mcpSDK.ListRootsResult:
				for _, root := range response.Roots {
					report = append(report, "root "+root.URI)
				}
				report = append(report, fmt.Sprintf("roots %d", len(response.Roots)))
			case *mcpSDK.CreateMessageResult:
				if text, ok := response.Content.(*mcpSDK.TextContent); ok {
					report = append(report, "sampled "+text.Text+" by "+response.Model)
				}
			case *mcpSDK.CreateMessageWithToolsResult:
				// The SDK decodes a sampling response in this shape on the newest protocol.
				for _, content := range response.Content {
					if text, ok := content.(*mcpSDK.TextContent); ok {
						report = append(report, "sampled "+text.Text+" by "+response.Model)
					}
				}
			case *mcpSDK.ElicitResult:
				content, _ := json.Marshal(response.Content)
				report = append(report, "elicited "+response.Action+" "+string(content))
			default:
				report = append(report, fmt.Sprintf("%s missing %T", key, response))
			}
		}
		_ = request.Session.Log(ctx, &mcpSDK.LoggingMessageParams{Level: "warning", Logger: "probe", Data: "disk almost full"})
		server.AddTool(&mcpSDK.Tool{Name: "later", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
			return &mcpSDK.CallToolResult{}, nil
		})
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: strings.Join(report, "\n")}}}, nil
	})
	// Reading doc://elicit asks the client a question in the read's result and answers with
	// what it got.
	server.AddResource(&mcpSDK.Resource{URI: "doc://elicit", Name: "elicit"}, func(_ context.Context, request *mcpSDK.ReadResourceRequest) (*mcpSDK.ReadResourceResult, error) {
		response, given := request.Params.InputResponses["elicit"]
		if !given {
			return &mcpSDK.ReadResourceResult{InputRequests: mcpSDK.InputRequestMap{"elicit": &mcpSDK.ElicitParams{
				Message:         "Which color?",
				RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{"color": map[string]any{"type": "string", "default": "red"}}},
			}}}, nil
		}
		text := fmt.Sprintf("%T", response)
		if result, ok := response.(*mcpSDK.ElicitResult); ok {
			content, _ := json.Marshal(result.Content)
			text = "elicited " + result.Action + " " + string(content)
		}
		return &mcpSDK.ReadResourceResult{Contents: []*mcpSDK.ResourceContents{{URI: "doc://elicit", MIMEType: "text/plain", Text: text}}}, nil
	})
	_ = server.Run(context.Background(), &mcpSDK.StdioTransport{})
	os.Exit(0)
}

type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func probeServer(t *testing.T, extra map[string]any) (*Manager, tools.Tool, string) {
	t.Helper()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "probe.json")
	fields := map[string]any{
		"transport":  map[string]any{"type": "stdio", "command": mustExecutable(t), "args": []string{"-test.run=^TestMCPClientFeaturesFixtureProcess$"}, "cwd": directory, "env": map[string]string{clientFeaturesFixtureEnvironment: "1"}},
		"timeout_ms": 10_000,
	}
	for key, value := range extra {
		fields[key] = value
	}
	config, _ := json.Marshal(fields)
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(currentEnvironment())
	t.Cleanup(func() { _ = manager.Close() })
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{{ID: "probe", ConfigPath: configPath, Enabled: true}}, directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range loaded {
		if tool.Definition.ID == "mcp:probe:probe" {
			return manager, tool, directory
		}
	}
	t.Fatal("no probe tool")
	return nil, tools.Tool{}, ""
}

func runProbe(t *testing.T, tool tools.Tool, execution tools.ExecutionContext, ask ...any) string {
	t.Helper()
	arguments := map[string]any{}
	if len(ask) > 0 {
		arguments["ask"] = ask
	}
	output, err := tool.ExecuteOutput(context.Background(), arguments, execution)
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestMCPClientOffersRootsSamplingElicitationAndLogs(t *testing.T) {
	manager, probe, directory := probeServer(t, map[string]any{
		"roots":        "workspace",
		"changes_note": true,
		"server_log":   "info",
		"sampling":     map[string]any{"enabled": true, "max_output_tokens": 30, "max_requests": 1},
		"elicitation":  map[string]any{"enabled": true, "timeout_ms": 5000},
	})
	logs := &syncBuffer{}
	manager.SetLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	var sampled SamplingRequest
	manager.SetSampler(func(_ context.Context, request SamplingRequest) (string, string, error) {
		sampled = request
		return "hi there", "gpt-test", nil
	})
	var question tools.InteractionQuestion
	var asked tools.InteractionIdentity
	execution := tools.ExecutionContext{CallID: "call-1", ToolName: "mcp__probe__probe", RequestInteraction: func(_ context.Context, identity tools.InteractionIdentity, q tools.InteractionQuestion) (tools.InteractionAnswer, error) {
		asked, question = identity, q
		return tools.InteractionAnswer{Decision: tools.InteractionUpdate, Outcome: tools.InteractionAnswered, Draft: json.RawMessage(`{"server":"probe","message":"Which color?","content":{"color":"blue"}}`)}, nil
	}}

	output := runProbe(t, probe, execution)
	root := workspaceRoot(directory).URI
	for _, want := range []string{"root " + root, "roots 1", "sampled hi there by gpt-test", `elicited accept {\"color\":\"blue\"}`} {
		if !strings.Contains(output, want) {
			t.Errorf("probe output misses %q:\n%s", want, output)
		}
	}
	if sampled.Server != "probe" || sampled.SystemPrompt != "be brief" || sampled.MaxOutputTokens != 30 || len(sampled.Messages) != 1 || sampled.Messages[0].Text != "say hi" {
		t.Fatalf("sampling request = %+v", sampled)
	}
	if asked.ToolCallID != "call-1" || !strings.Contains(string(question.Draft), `"color":"red"`) || question.TimeoutMS != 5000 || !strings.Contains(question.Title, "Which color?") {
		t.Fatalf("elicitation question = %+v %s", asked, question.Draft)
	}

	// The tool list change is recorded for the next run, and the log reached the server log.
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, _, _ := manager.ResourceEvents(0)
		found := false
		for _, event := range events {
			found = found || event.Kind == ResourceEventToolsChanged
		}
		if found && strings.Contains(logs.String(), "disk almost full") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("events = %+v, logs = %s", events, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if note := ResourceChangesNote(manager.TakeResourceChanges()); !strings.Contains(note, "server probe: its tool list changed") {
		t.Fatalf("note = %q", note)
	}

	// A declined question declines; a sampling request over the budget fails the call
	// (the SDK fails a call whose input a client handler refused).
	execution.RequestInteraction = func(context.Context, tools.InteractionIdentity, tools.InteractionQuestion) (tools.InteractionAnswer, error) {
		return tools.InteractionAnswer{Decision: tools.InteractionDecline, Outcome: tools.InteractionAnswered}, nil
	}
	if output := runProbe(t, probe, execution, "elicit"); !strings.Contains(output, "elicited decline") {
		t.Fatalf("declined probe output:\n%s", output)
	}
	if output := runProbe(t, probe, execution, "sample"); !strings.Contains(output, `\"status\":\"error\"`) || strings.Contains(output, "sampled") {
		t.Fatalf("over-budget probe output:\n%s", output)
	}
}

func TestMCPClientFeaturesDefaults(t *testing.T) {
	_, probe, _ := probeServer(t, nil)
	// Roots are off by default, as are sampling and elicitation: a server that asks for
	// them fails its call or gets no roots.
	if output := runProbe(t, probe, tools.ExecutionContext{CallID: "c"}, "roots"); !strings.Contains(output, "roots 0") {
		t.Fatalf("roots output:\n%s", output)
	}
	for _, ask := range []string{"sample", "elicit"} {
		if output := runProbe(t, probe, tools.ExecutionContext{CallID: "c"}, ask); !strings.Contains(output, `\"status\":\"error\"`) {
			t.Fatalf("%s output:\n%s", ask, output)
		}
	}
	_, probe, directory := probeServer(t, map[string]any{"roots": "workspace"})
	output := runProbe(t, probe, tools.ExecutionContext{CallID: "c"}, "roots")
	if !strings.Contains(output, "roots 1") || !strings.Contains(output, "root "+workspaceRoot(directory).URI) {
		t.Fatalf("roots workspace output:\n%s", output)
	}
}

func probeState(t *testing.T, manager *Manager) *clientState {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	current := manager.connections["probe"]
	if current == nil {
		t.Fatal("no probe connection")
	}
	return current.state
}

func TestMCPClientFeaturesConfigRefusals(t *testing.T) {
	for name, raw := range map[string]string{
		"roots":           `{"roots":"all"}`,
		"changes_note":    `{"changes_note":"yes"}`,
		"server_log":      `{"server_log":"loud"}`,
		"log bytes":       `{"server_log_max_bytes":1}`,
		"sampling shape":  `{"sampling":true}`,
		"sampling key":    `{"sampling":{"model":"x"}}`,
		"sampling tokens": `{"sampling":{"max_output_tokens":0}}`,
		"elicit shape":    `{"elicitation":true}`,
		"elicit timeout":  `{"elicitation":{"timeout_ms":10}}`,
		"elicit key":      `{"elicitation":{"url":true}}`,
	} {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &fields)
		if _, err := normalizeClientFeatures(fields, "server.json"); err == nil {
			t.Errorf("%s: %s accepted", name, raw)
		}
	}
	features, err := normalizeClientFeatures(map[string]json.RawMessage{}, "server.json")
	if err != nil || features.Roots != RootsNone || features.ChangesNote || features.ServerLog != ServerLogOff || features.Sampling || features.Elicitation {
		t.Fatalf("defaults = %+v, %v", features, err)
	}
}

func TestMCPStdioServersStartThroughTheCommandWrapper(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "probe.json")
	config, _ := json.Marshal(map[string]any{
		"transport":  map[string]any{"type": "stdio", "command": mustExecutable(t), "args": []string{"-test.run=^TestMCPClientFeaturesFixtureProcess$"}, "cwd": directory, "env": map[string]string{clientFeaturesFixtureEnvironment: "1"}},
		"timeout_ms": 10_000,
	})
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	envProgram, err := exec.LookPath("env")
	if err != nil {
		t.Skip("no env program")
	}
	manager := NewManager(currentEnvironment())
	defer manager.Close()
	var wrapped []string
	wrap := func(program string, args []string, cwd string) (CommandLaunch, error) {
		wrapped = append(wrapped, program+" in "+cwd)
		// env stands in for a sandbox program that runs the server after it.
		return CommandLaunch{File: envProgram, Argv0: "sandbox", Args: append([]string{program}, args...)}, nil
	}
	servers := []sessionfs.MCPServerReference{{ID: "probe", ConfigPath: configPath, Enabled: true}}
	manager.SetCommandWrapper("sandbox-a", wrap)
	if _, err := manager.LoadTools(context.Background(), servers, directory); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.LoadTools(context.Background(), servers, directory); err != nil {
		t.Fatal(err)
	}
	if len(wrapped) != 1 || !strings.HasSuffix(wrapped[0], " in "+directory) {
		t.Fatalf("wrapped = %q, want one start", wrapped)
	}
	// Another wrapper connects the server again through it.
	manager.SetCommandWrapper("sandbox-b", wrap)
	if _, err := manager.LoadTools(context.Background(), servers, directory); err != nil {
		t.Fatal(err)
	}
	if len(wrapped) != 2 {
		t.Fatalf("wrapped = %q, want a second start for the new wrapper", wrapped)
	}
}
