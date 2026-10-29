//go:build !windows

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
)

const (
	mcpGrandchildEnvironment    = "JAD_MCP_GRANDCHILD_FIXTURE"
	mcpGrandchildPIDEnvironment = "JAD_MCP_GRANDCHILD_PID_FILE"
	// mcpGrandchildMarker is in the grandchild's argv so a stray process can be found
	// with ps after the run.
	mcpGrandchildMarker = "jad-mcp-grandchild-fixture"
)

// TestMCPGrandchildFixtureProcess is a stdio MCP server that first starts a grandchild
// of its own, the way a wrapper such as npx or sh -c does. The grandchild's pid goes to
// the file named by JAD_MCP_GRANDCHILD_PID_FILE so the test can watch it.
func TestMCPGrandchildFixtureProcess(t *testing.T) {
	if os.Getenv(mcpGrandchildEnvironment) != "1" {
		return
	}
	grandchild := exec.Command("/bin/sh", "-c", "exec sleep 600000 # "+mcpGrandchildMarker)
	// The pipes of the fixture belong to the MCP transport; a grandchild must not hold
	// them open.
	grandchild.Stdout, grandchild.Stderr = nil, nil
	if err := grandchild.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if path := os.Getenv(mcpGrandchildPIDEnvironment); path != "" {
		if err := os.WriteFile(path, []byte(strconv.Itoa(grandchild.Process.Pid)), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "grandchild-fixture", Version: "1.0.0"}, nil)
	server.AddTool(&mcpSDK.Tool{
		Name:        "echo",
		Description: "grandchild fixture",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(_ context.Context, _ *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "echo"}}}, nil
	})
	if err := server.Run(context.Background(), &mcpSDK.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

// A stdio server is started in its own process group, so the wrapper's child dies with
// the manager instead of surviving as an orphan (P6AM-19).
func TestManagerStopsStdioGrandchildOnClose(t *testing.T) {
	cwd := t.TempDir()
	pidPath := filepath.Join(cwd, "grandchild.pid")
	configPath := filepath.Join(cwd, "server.json")
	config := map[string]any{
		"transport": map[string]any{
			"type":    "stdio",
			"command": mustExecutable(t),
			"args":    []string{"-test.run=^TestMCPGrandchildFixtureProcess$"},
			"cwd":     cwd,
			"env": map[string]string{
				mcpGrandchildEnvironment:    "1",
				mcpGrandchildPIDEnvironment: pidPath,
			},
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

	manager := NewManager(currentEnvironment())
	servers := []sessionfs.MCPServerReference{{ID: "grandchild", ConfigPath: configPath, Enabled: true}}
	loaded, err := manager.LoadTools(context.Background(), servers, cwd)
	if err != nil {
		t.Fatalf("LoadTools() error = %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded tool count = %d, want 1", len(loaded))
	}
	pid := waitForGrandchildPID(t, pidPath)
	t.Cleanup(func() {
		if processAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if !processAlive(pid) {
		t.Fatalf("grandchild %d did not start", pid)
	}

	if err := manager.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("stdio grandchild %d is still alive after Close()", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForGrandchildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		contents, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
			if err != nil || pid <= 1 {
				t.Fatalf("grandchild pid file %q = %q", path, contents)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild pid file %q was not written: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processAlive reports whether pid is a live process. A zombie still answers signal 0,
// so a reaped exit is told apart by its state where the system exposes it.
func processAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	state, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	fields := strings.Fields(string(state))
	return len(fields) < 3 || fields[2] != "Z"
}
