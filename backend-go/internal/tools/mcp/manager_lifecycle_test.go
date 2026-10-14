package mcp

import (
	"context"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func echoTool(t *testing.T, loaded []tools.Tool) tools.Tool {
	t.Helper()
	for _, tool := range loaded {
		if tool.Definition.ID == "mcp:fixture:echo" {
			return tool
		}
	}
	t.Fatalf("no echo tool in %+v", loaded)
	return tools.Tool{}
}

// callEchoTool returns the tool result; a call on a closed connection is not a Go error but a
// result with "status":"error", which is all the model sees.
func callEchoTool(t *testing.T, tool tools.Tool) string {
	t.Helper()
	output, err := tool.ExecuteOutput(context.Background(), map[string]any{"text": "hi", "pair": []any{"word", 42}}, tools.ExecutionContext{})
	if err != nil {
		t.Fatalf("ExecuteOutput() error = %v", err)
	}
	return string(output)
}

func wantWorks(t *testing.T, label string, tool tools.Tool) {
	t.Helper()
	if got := callEchoTool(t, tool); strings.Contains(got, `\"status\":\"error\"`) || !strings.Contains(got, "hi") {
		t.Fatalf("%s: the tool did not work: %s", label, got)
	}
}

func wantClosed(t *testing.T, label string, tool tools.Tool) {
	t.Helper()
	if got := callEchoTool(t, tool); !strings.Contains(got, `\"status\":\"error\"`) {
		t.Fatalf("%s: a tool of a closed connection returned %s, want an error result", label, got)
	}
}

// A changed server file starts a new connection and closes the old one; a tool built from the
// old connection fails with "closed" and the tool from the new one works.
func TestManagerReplacesAConnectionWhoseConfigChanged(t *testing.T) {
	cwd := t.TempDir()
	configPath := writeStdioConfig(t, cwd, "before")
	servers := []sessionfs.MCPServerReference{{ID: "fixture", ConfigPath: configPath, Enabled: true}}
	manager := NewManager(currentEnvironment())
	defer manager.Close()

	first, err := manager.LoadTools(context.Background(), servers, cwd)
	if err != nil {
		t.Fatal(err)
	}
	oldConnection := manager.connections["fixture"]
	oldTool := echoTool(t, first)
	wantWorks(t, "before the change", oldTool)

	writeStdioConfig(t, cwd, "after")
	second, err := manager.LoadTools(context.Background(), servers, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if manager.connections["fixture"] == oldConnection {
		t.Fatal("a changed config kept the old connection")
	}
	if !strings.Contains(echoTool(t, second).Definition.Description, "after") {
		t.Fatalf("the new tool does not come from the new config: %q", echoTool(t, second).Definition.Description)
	}
	wantWorks(t, "new connection", echoTool(t, second))
	wantClosed(t, "replaced connection", oldTool)
}

// Leaving a server out of the enabled list, or disabling it, closes and forgets its connection.
func TestManagerClosesAConnectionWhoseServerIsNoLongerEnabled(t *testing.T) {
	cwd := t.TempDir()
	configPath := writeStdioConfig(t, cwd, "x")
	manager := NewManager(currentEnvironment())
	defer manager.Close()

	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{{ID: "fixture", ConfigPath: configPath, Enabled: true}}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	tool := echoTool(t, loaded)
	wantWorks(t, "first load", tool)

	for name, servers := range map[string][]sessionfs.MCPServerReference{
		"disabled": {{ID: "fixture", ConfigPath: configPath, Enabled: false}},
		"absent":   nil,
	} {
		again, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{{ID: "fixture", ConfigPath: configPath, Enabled: true}}, cwd)
		if err != nil {
			t.Fatalf("%s: reconnect: %v", name, err)
		}
		tool = echoTool(t, again)
		if got, err := manager.LoadTools(context.Background(), servers, cwd); err != nil || len(got) != 0 {
			t.Fatalf("%s: LoadTools = %d tools, %v; want none", name, len(got), err)
		}
		if _, kept := manager.connections["fixture"]; kept {
			t.Fatalf("%s: the connection is still retained", name)
		}
		wantClosed(t, name, tool)
	}
}

// Close closes every connection, is safe twice, and a closed manager loads nothing.
func TestManagerCloseClosesConnectionsAndRefusesFurtherLoads(t *testing.T) {
	cwd := t.TempDir()
	servers := []sessionfs.MCPServerReference{{ID: "fixture", ConfigPath: writeStdioConfig(t, cwd, "x"), Enabled: true}}
	manager := NewManager(currentEnvironment())

	loaded, err := manager.LoadTools(context.Background(), servers, cwd)
	if err != nil {
		t.Fatal(err)
	}
	tool := echoTool(t, loaded)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	wantClosed(t, "closed manager", tool)
	if _, err := manager.LoadTools(context.Background(), servers, cwd); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("LoadTools on a closed manager returned %v", err)
	}
}
