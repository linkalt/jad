package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A writer child's exec starts in the session's cwd, not in the folder the server runs from:
// pwd names the workspace, a relative read finds the workspace's file, and a relative write
// lands in the workspace.
func TestSubagentExecRunsInTheSessionCwdNotTheServers(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"exec"},
		"__exec": map[string]any{},
	})
	f.provider.childFirstCall = [2]string{"exec", `{"code":"pwd; cat notes.txt; echo from-child > made-by-child.txt"}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	child := f.provider.forModel("child-model")
	if len(child) < 2 {
		t.Fatalf("child requests = %d, want at least 2", len(child))
	}
	workspace, err := filepath.EvalSymlinks(f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(child[1].Text, workspace) {
		t.Errorf("the child's pwd is not the session cwd %q:\n%s", workspace, child[1].Text)
	}
	if !strings.Contains(child[1].Text, "the secret is 42") {
		t.Errorf("the child's relative read did not find the workspace file:\n%s", child[1].Text)
	}
	data, err := os.ReadFile(filepath.Join(f.workspace, "made-by-child.txt"))
	if err != nil {
		t.Fatalf("the child's relative write is not in the workspace: %v", err)
	}
	if strings.TrimSpace(string(data)) != "from-child" {
		t.Errorf("made-by-child.txt = %q", data)
	}
	if _, err := os.Stat("made-by-child.txt"); err == nil {
		os.Remove("made-by-child.txt")
		t.Errorf("the child wrote into the server's working directory")
	}
}
