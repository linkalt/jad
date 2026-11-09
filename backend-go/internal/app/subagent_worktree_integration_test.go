package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/remoteapi"
)

// gitIn runs git in dir with a fixed identity and fails the test on an error.
func gitIn(t *testing.T, dir string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, arguments...)...)
	command.Dir = dir
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A writer agent with isolation worktree writes through the real write tool into a
// worktree of the session's repository: the session's files stay as they were, the change
// is on the branch the parent is told of, and the run takes no checkpoint.
func TestIsolatedWriterChangesABranchNotTheSession(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"skill", "exec"}, "isolation": "worktree",
		"__exec": map[string]any{}, "__checkpoint": `{"enabled":true}`,
	})
	f.provider.childFirstCall = [2]string{"exec", execWrite("made.txt", "by the child")}
	gitIn(t, f.workspace, "init", "--quiet")
	gitIn(t, f.workspace, "add", "notes.txt")
	gitIn(t, f.workspace, "commit", "--quiet", "-m", "init")

	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "made.txt")); !os.IsNotExist(err) {
		t.Fatalf("the child's write reached the session's folder: %v", err)
	}
	branch := gitIn(t, f.workspace, "branch", "--list", "jad/*", "--format=%(refname:short)")
	if !strings.HasPrefix(branch, "jad/sub_") {
		t.Fatalf("branches = %q", branch)
	}
	if made := gitIn(t, f.workspace, "show", branch+":made.txt"); made != "by the child" {
		t.Fatalf("made.txt on %s = %q", branch, made)
	}
	parent := f.provider.forModel("parent-model")
	last := parent[len(parent)-1].Text
	if !strings.Contains(last, "Agent explore finished") || !strings.Contains(last, "worktree_branch: "+branch) || !strings.Contains(last, "git merge "+branch) {
		t.Fatalf("the parent was not told of the branch:\n%s", last)
	}
	if records := f.checkpointRecords(t); len(records) != 0 {
		t.Fatalf("an isolated writer took a checkpoint of the session's files: %v", records)
	}
	if list := gitIn(t, f.workspace, "worktree", "list"); strings.Count(list, "\n") != 0 {
		t.Fatalf("a worktree is left: %s", list)
	}
}

// An isolated agent started over the remote API works in a worktree as a local call's does, and
// the remote run's view carries its worktree lines, so the calling server can tell where the work is.
func TestRemoteIsolatedSubagentReportsItsWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"skill", "exec"}, "isolation": "worktree", "__exec": map[string]any{},
	})
	f.provider.childFirstCall = [2]string{"exec", execWrite("made.txt", "by the child")}
	gitIn(t, f.workspace, "init", "--quiet")
	gitIn(t, f.workspace, "add", "notes.txt")
	gitIn(t, f.workspace, "commit", "--quiet", "-m", "init")
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))
	status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "write it", "request_id": "req-sub-wt001"})
	if status != 202 {
		t.Fatalf("start: %d %v", status, body)
	}
	done := server.waitDone(t, body["id"].(string))
	lines, _ := done["worktree"].([]any)
	values := map[string]string{}
	for _, line := range lines {
		entry, _ := line.(map[string]any)
		key, _ := entry["key"].(string)
		values[key], _ = entry["value"].(string)
	}
	branch := values["worktree_branch"]
	if done["state"] != "done" || !strings.HasPrefix(branch, "jad/sub_") || values["worktree_conflicts"] == "" {
		t.Fatalf("outcome: %v", done)
	}
	if made := gitIn(t, f.workspace, "show", branch+":made.txt"); made != "by the child" {
		t.Fatalf("made.txt on %s = %q", branch, made)
	}
}
