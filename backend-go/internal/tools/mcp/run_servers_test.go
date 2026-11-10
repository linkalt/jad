package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const runFixtureEnvironment = "JAD_MCP_RUN_FIXTURE"

// TestMCPRunFixtureProcess is the stdio server of the run tests. Its where tool answers with the
// server's label (from its environment) and the folder it works in, and appends one line to the
// file named by JAD_MCP_RUN_CALLS, so a test sees which server took a call. It does nothing unless
// started as the fixture.
func TestMCPRunFixtureProcess(t *testing.T) {
	if os.Getenv(runFixtureEnvironment) != "1" {
		return
	}
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "run-fixture", Version: "1.0.0"}, nil)
	server.AddTool(&mcpSDK.Tool{
		Name:        "where",
		Description: "run fixture",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(_ context.Context, _ *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		cwd, _ := os.Getwd()
		label := os.Getenv("JAD_MCP_RUN_LABEL")
		if calls := os.Getenv("JAD_MCP_RUN_CALLS"); calls != "" {
			file, err := os.OpenFile(calls, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				fmt.Fprintln(file, label)
				_ = file.Close()
			}
		}
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "label=" + label + " cwd=" + cwd}}}, nil
	})
	if err := server.Run(context.Background(), &mcpSDK.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

// writeRunServer writes the config of a run fixture server that starts in cwd and returns its path.
func writeRunServer(t *testing.T, directory, name, cwd, label, calls string) string {
	t.Helper()
	path := filepath.Join(directory, name+".json")
	config, _ := json.Marshal(map[string]any{
		"transport": map[string]any{
			"type": "stdio", "command": mustExecutable(t), "args": []string{"-test.run=^TestMCPRunFixtureProcess$"}, "cwd": cwd,
			"env": map[string]string{runFixtureEnvironment: "1", "JAD_MCP_RUN_LABEL": label, "JAD_MCP_RUN_CALLS": calls},
		},
		"timeout_ms": 10_000,
	})
	if err := os.WriteFile(path, config, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func whereTool(t *testing.T, loaded []tools.Tool) tools.Tool {
	t.Helper()
	for _, tool := range loaded {
		if tool.Definition.ID == "mcp:fix:where" {
			return tool
		}
	}
	t.Fatalf("no where tool in %d tools", len(loaded))
	return tools.Tool{}
}

func callWhere(t *testing.T, tool tools.Tool, execution tools.ExecutionContext) string {
	t.Helper()
	output, err := tool.ExecuteOutput(context.Background(), map[string]any{}, execution)
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestRunPlacementMovesOnlyAFolderInsideTheOrigin(t *testing.T) {
	// A worktree in a folder below the origin: a repository's .git folder.
	place := &RunPlacement{Origin: "/work/app", Workspace: "/work/app/.git/jad-worktrees/run"}
	for from, want := range map[string]string{
		"/work/app":                              "/work/app/.git/jad-worktrees/run",
		"/work/app/tools/x":                      "/work/app/.git/jad-worktrees/run/tools/x",
		"/work/app/.git/jad-worktrees/run":       "/work/app/.git/jad-worktrees/run",
		"/work/app/.git/jad-worktrees/run/tools": "/work/app/.git/jad-worktrees/run/tools",
		"/work/application":                      "/work/application",
		"/work":                                  "/work",
		"/elsewhere":                             "/elsewhere",
	} {
		if got := place.move(from); got != want {
			t.Errorf("move(%s) = %s, want %s", from, got, want)
		}
	}
	if got := (*RunPlacement)(nil).move("/work/app/x"); got != "/work/app/x" {
		t.Errorf("a nil placement moved the folder to %s", got)
	}
}

// A run's manager starts a server in the run's workspace: a relative folder is taken from it, an
// absolute one inside the session's folder is moved into it, and the workspace is not checked on
// the host (an overlay run's files exist only inside its mount), where the wrapper says to start.
func TestRunManagerStartsServersInTheRunsWorkspace(t *testing.T) {
	root := t.TempDir()
	origin, workspace := filepath.Join(root, "session"), filepath.Join(root, "tree")
	for _, dir := range []string{filepath.Join(origin, "sub"), filepath.Join(workspace, "sub"), filepath.Join(workspace, "abs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(origin, "abs"), 0o755); err != nil {
		t.Fatal(err)
	}
	relative := writeRunServer(t, root, "relative", "sub", "run", "")
	absolute := writeRunServer(t, root, "absolute", filepath.Join(origin, "abs"), "run", "")
	manager := NewManager(currentEnvironment())
	t.Cleanup(func() { _ = manager.Close() })
	manager.SetRunPlacement(RunPlacement{Origin: origin, Workspace: workspace})
	for config, want := range map[string]string{relative: filepath.Join(workspace, "sub"), absolute: filepath.Join(workspace, "abs")} {
		loaded, ids, err := manager.LoadStdioTools(context.Background(), []sessionfs.MCPServerReference{{ID: "fix", ConfigPath: config, Enabled: true}}, workspace)
		if err != nil || len(ids) != 1 {
			t.Fatalf("LoadStdioTools(%s) = %v, %v", config, ids, err)
		}
		if out := callWhere(t, whereTool(t, loaded), tools.ExecutionContext{}); !strings.Contains(out, "cwd="+want) {
			t.Errorf("%s: server answered %s, want it in %s", filepath.Base(config), out, want)
		}
	}

	// A folder the host does not have: the wrapper starts the server elsewhere and says where.
	missing := writeRunServer(t, root, "missing", "not-on-the-host", "run", "")
	envProgram, err := exec.LookPath("env")
	if err != nil {
		t.Skip("no env program")
	}
	manager.SetCommandWrapper("inside", func(program string, args []string, cwd string) (CommandLaunch, error) {
		return CommandLaunch{File: envProgram, Argv0: "inside", Args: append([]string{program}, args...), Dir: filepath.Join(workspace, "sub")}, nil
	})
	loaded, _, err := manager.LoadStdioTools(context.Background(), []sessionfs.MCPServerReference{{ID: "fix", ConfigPath: missing, Enabled: true}}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if out := callWhere(t, whereTool(t, loaded), tools.ExecutionContext{}); !strings.Contains(out, "cwd="+filepath.Join(workspace, "sub")) {
		t.Errorf("the wrapper's folder was not used: %s", out)
	}
}

// Without a placement a stdio folder that does not exist is still refused, as before.
func TestSessionManagerStillRefusesAMissingStdioFolder(t *testing.T) {
	root := t.TempDir()
	config := writeRunServer(t, root, "missing", "not-on-the-host", "session", "")
	manager := NewManager(currentEnvironment())
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{{ID: "fix", ConfigPath: config, Enabled: true}}, root); err == nil || !strings.Contains(err.Error(), "stdio cwd does not exist") {
		t.Fatalf("LoadTools error = %v", err)
	}
}

// A run's manager starts the stdio servers only: a streamable HTTP server is a remote endpoint,
// so the run keeps the connection the session has to it, and a disabled server stays off.
func TestLoadStdioToolsLeavesOutHTTPAndDisabledServers(t *testing.T) {
	root := t.TempDir()
	stdio := writeRunServer(t, root, "stdio", root, "run", "")
	httpPath := filepath.Join(root, "remote.json")
	if err := os.WriteFile(httpPath, []byte(`{"transport":{"type":"streamable_http","url":"https://mcp.invalid/mcp"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(currentEnvironment())
	t.Cleanup(func() { _ = manager.Close() })
	loaded, ids, err := manager.LoadStdioTools(context.Background(), []sessionfs.MCPServerReference{
		{ID: "remote", ConfigPath: httpPath, Enabled: true},
		{ID: "fix", ConfigPath: stdio, Enabled: true},
		{ID: "off", ConfigPath: stdio, Enabled: false},
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "fix" {
		t.Fatalf("stdio servers = %v, want [fix]", ids)
	}
	for _, tool := range loaded {
		if tool.Definition.Server != "fix" {
			t.Errorf("a tool of %s was loaded", tool.Definition.Server)
		}
	}
	// A server that does not start fails the load: the owner ends the run, there is no fallback.
	broken := filepath.Join(root, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"transport":{"type":"stdio","command":"jad-no-such-mcp-server"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.LoadStdioTools(context.Background(), []sessionfs.MCPServerReference{{ID: "broken", ConfigPath: broken, Enabled: true}}, root); err == nil {
		t.Fatal("a server that cannot start loaded")
	}
}

// A session manager's tool hands a run's call to the run's own tool when the route covers it, fails
// the call when the route covers it with an error (never calling the session's server), and calls
// its own connection otherwise. A run manager's tools never route.
func TestSessionToolsHandARunsCallsToTheRunsServers(t *testing.T) {
	root := t.TempDir()
	sessionCalls, runCalls := filepath.Join(root, "session.calls"), filepath.Join(root, "run.calls")
	sessionServer := writeRunServer(t, root, "session", root, "session", sessionCalls)
	runServer := writeRunServer(t, root, "run", root, "run", runCalls)
	servers := func(path string) []sessionfs.MCPServerReference {
		return []sessionfs.MCPServerReference{{ID: "fix", ConfigPath: path, Enabled: true}}
	}
	session := NewManager(currentEnvironment())
	session.SetRouteKey("agent-set")
	t.Cleanup(func() { _ = session.Close() })
	sessionTools, err := session.LoadTools(context.Background(), servers(sessionServer), root)
	if err != nil {
		t.Fatal(err)
	}
	run := NewManager(currentEnvironment())
	t.Cleanup(func() { _ = run.Close() })
	runTools, _, err := run.LoadStdioTools(context.Background(), servers(runServer), root)
	if err != nil {
		t.Fatal(err)
	}
	sessionTool, runTool := whereTool(t, sessionTools), whereTool(t, runTools)

	var asked []string
	route := func(target func() (tools.Tool, bool, error)) tools.MCPRouter {
		return func(key string, definition tools.Definition, _ tools.ExecutionContext) (tools.Tool, bool, error) {
			asked = append(asked, key+"|"+definition.ID+"|"+definition.Server)
			return target()
		}
	}
	// No route: the session's own server, as before.
	if out := callWhere(t, sessionTool, tools.ExecutionContext{}); !strings.Contains(out, "label=session") {
		t.Fatalf("no route: %s", out)
	}
	// Not covered (an HTTP server, or a set the run did not start): the session's own server.
	uncovered := tools.ExecutionContext{MCPRoute: route(func() (tools.Tool, bool, error) { return tools.Tool{}, false, nil })}
	if out := callWhere(t, sessionTool, uncovered); !strings.Contains(out, "label=session") {
		t.Fatalf("not covered: %s", out)
	}
	// Covered: the run's tool answers, and the session's server never sees the call.
	covered := tools.ExecutionContext{MCPRoute: route(func() (tools.Tool, bool, error) { return runTool, true, nil })}
	if out := callWhere(t, sessionTool, covered); !strings.Contains(out, "label=run") {
		t.Fatalf("covered: %s", out)
	}
	if want := "agent-set|mcp:fix:where|fix"; asked[len(asked)-1] != want {
		t.Fatalf("the route was asked %q, want %q", asked[len(asked)-1], want)
	}
	// Covered with an error: the call fails, and the session's server is not called.
	failed := tools.ExecutionContext{MCPRoute: route(func() (tools.Tool, bool, error) { return tools.Tool{}, true, fmt.Errorf("the run has ended") })}
	if out := callWhere(t, sessionTool, failed); !strings.Contains(out, "the run has ended") || strings.Contains(out, "label=") {
		t.Fatalf("failed route: %s", out)
	}
	calls, _ := os.ReadFile(sessionCalls)
	if strings.Count(string(calls), "session") != 2 {
		t.Fatalf("the session's server took calls %q, want only the two it was meant to take", calls)
	}
	calls, _ = os.ReadFile(runCalls)
	if strings.Count(string(calls), "run") != 1 {
		t.Fatalf("the run's server took calls %q, want one", calls)
	}
	// The run's own tool never asks the route, so it cannot route to itself.
	before := len(asked)
	if out := callWhere(t, runTool, covered); !strings.Contains(out, "label=run") || len(asked) != before {
		t.Fatalf("the run manager's tool routed: %s (route asked %d times)", out, len(asked)-before)
	}
}
