package native

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// gitRepository makes a repository with one commit holding a.txt and keep/secret.txt,
// with no identity of its own, and returns its folder with symlinks resolved.
func gitRepository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"a.txt": "a\n", "keep/secret.txt": "s\n"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCommand(t, dir, "init", "--quiet")
	gitCommand(t, dir, "add", ".")
	gitCommand(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--quiet", "-m", "init")
	return dir
}

func gitCommand(t *testing.T, dir string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = dir
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, out)
	}
	return strings.TrimSpace(string(out))
}

func isolatedAgent(name string) sessionfs.NativeSubagentAgent {
	agent := writerAgent(name, "read", "exec")
	agent.Isolation = sessionfs.SubagentIsolationWorktree
	return agent
}

// worktreeChild is a child that writes files through the context the manager gave it,
// under the session's path rules moved into its workspace (workspacePolicy, as exec's cwd
// check uses them), and records that context.
type worktreeChild struct {
	executions chan tools.ExecutionContext
	policy     sessionfs.NativePathPolicy
	files      map[string]string
}

func (c *worktreeChild) write(path, text string, execution tools.ExecutionContext) string {
	target := filepath.Join(execution.WorkspaceRoot, filepath.FromSlash(path))
	for _, rule := range workspacePolicy(c.policy, execution).Paths {
		if rule.Action == "deny" && sessionfs.PathWithin(rule.Path, target) {
			return "path_denied " + path
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err.Error()
	}
	if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
		return err.Error()
	}
	return "wrote " + path
}

func (c *worktreeChild) run(_ context.Context, request SubagentRequest) (SubagentResult, error) {
	c.executions <- request.Execution
	var outputs []string
	for path, text := range c.files {
		execution := request.Execution
		execution.CallID = "w"
		outputs = append(outputs, c.write(path, text, execution))
	}
	return SubagentResult{Text: "wrote: " + strings.Join(outputs, " | ")}, nil
}

func worktreeCall(t *testing.T, catalog *tools.Catalog, root, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_w", "name": "subagent", "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: root, SessionDir: "/sess", SessionKey: testSessionKey})
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	return item.Output
}

func TestIsolatedAgentWorksInAWorktreeAndReturnsABranch(t *testing.T) {
	repo := gitRepository(t)
	// The session denies keep/; the rule follows the child into its worktree.
	policy := sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Join(repo, "keep")}}}
	child := &worktreeChild{executions: make(chan tools.ExecutionContext, 1), policy: policy, files: map[string]string{"a.txt": "changed\n", "new.txt": "new\n", "keep/secret.txt": "stolen\n"}}
	settings := writeSettings(isolatedAgent("fixer"))
	settings.StorageDir = t.TempDir()
	catalog, _ := buildSubagentCatalog(t, settings, &fakeLauncher{run: child.run}, readOnlyTool("read"), writeTool("exec"))

	out := worktreeCall(t, catalog, repo, `{"agent":"fixer","task":"fix it","block_ms":3000}`)
	execution := <-child.executions
	if execution.WorkspaceOrigin != repo || !strings.Contains(execution.WorkspaceRoot, filepath.Join(".git", "jad-worktrees", "sub_")) {
		t.Fatalf("child root %q, origin %q", execution.WorkspaceRoot, execution.WorkspaceOrigin)
	}
	for _, want := range []string{"worktree_branch: jad/sub_", "worktree_changes: 2 files changed", "git merge jad/sub_", "path_denied"} {
		if !strings.Contains(out, want) {
			t.Fatalf("result lacks %q:\n%s", want, out)
		}
	}
	// The session's files are as they were, and the worktree folder is gone.
	if data, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(data) != "a\n" {
		t.Fatalf("session a.txt = %q", data)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt reached the session: %v", err)
	}
	if _, err := os.Stat(execution.WorkspaceRoot); !os.IsNotExist(err) {
		t.Fatalf("worktree folder left behind: %v", err)
	}
	branch := strings.TrimSpace(gitCommand(t, repo, "branch", "--list", "jad/*", "--format=%(refname:short)"))
	if !strings.HasPrefix(branch, "jad/sub_") || strings.Contains(branch, "\n") {
		t.Fatalf("branches = %q", branch)
	}
	if files := gitCommand(t, repo, "diff", "--name-only", "HEAD", branch); files != "a.txt\nnew.txt" {
		t.Fatalf("branch changes = %q", files)
	}
	if secret := gitCommand(t, repo, "show", branch+":keep/secret.txt"); secret != "s" {
		t.Fatalf("the denied file changed on the branch: %q", secret)
	}
}

// An isolated agent's call is not a change of the session's files: it takes no
// checkpoint, needs no background writer place, and is handed off like a reader.
func TestIsolatedAgentIsScheduledLikeAReader(t *testing.T) {
	catalog, _ := buildSubagentCatalog(t, writeSettings(isolatedAgent("fixer"), writerAgent("plain", "read", "exec")), &fakeLauncher{}, readOnlyTool("read"), writeTool("exec"))
	for agent, want := range map[string]bool{"fixer": false, "plain": true} {
		call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c", "name": "subagent", "arguments": `{"agent":"` + agent + `","task":"x"}`})
		if got := catalog.CallMayChangeFiles(call); got != want {
			t.Errorf("%s: CallMayChangeFiles = %v, want %v", agent, got, want)
		}
	}
	list := subagentAgentList([]sessionfs.NativeSubagentAgent{isolatedAgent("fixer")}, 4096, nil)
	if !strings.Contains(list, "fixer [can change files in a git worktree of its own; its changes come back as a branch]") {
		t.Fatalf("list:\n%s", list)
	}
}

func TestIsolatedRunThatChangesNothingLeavesNoBranch(t *testing.T) {
	repo := gitRepository(t)
	child := &worktreeChild{executions: make(chan tools.ExecutionContext, 1), policy: sessionfs.NativePathPolicy{}}
	settings := writeSettings(isolatedAgent("fixer"))
	settings.StorageDir = t.TempDir()
	catalog, _ := buildSubagentCatalog(t, settings, &fakeLauncher{run: child.run}, readOnlyTool("read"), writeTool("exec"))
	out := worktreeCall(t, catalog, repo, `{"agent":"fixer","task":"look","block_ms":3000}`)
	<-child.executions
	if !strings.Contains(out, "worktree_changes: none (its worktree and branch were removed)") {
		t.Fatalf("result:\n%s", out)
	}
	if branches := gitCommand(t, repo, "branch", "--list", "jad/*"); branches != "" {
		t.Fatalf("branches = %q", branches)
	}
}

// Outside a git repository the run fails with a reason, and nothing was started.
func TestIsolatedRunOutsideARepositoryFails(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	launcher := &fakeLauncher{}
	settings := writeSettings(isolatedAgent("fixer"))
	settings.StorageDir = t.TempDir()
	catalog, _ := buildSubagentCatalog(t, settings, launcher, readOnlyTool("read"), writeTool("exec"))
	out := worktreeCall(t, catalog, t.TempDir(), `{"agent":"fixer","task":"x","block_ms":3000}`)
	if !strings.Contains(out, "subagent_failed") || !strings.Contains(out, "not inside a git repository") || len(launcher.requests) != 0 {
		t.Fatalf("result (launches %d):\n%s", len(launcher.requests), out)
	}
}

// A resume of an isolated run starts its worktree at the branch the run left.
func TestIsolatedResumeStartsFromTheRunsBranch(t *testing.T) {
	repo := gitRepository(t)
	child := &worktreeChild{executions: make(chan tools.ExecutionContext, 2), policy: sessionfs.NativePathPolicy{}, files: map[string]string{"a.txt": "first\n"}}
	var seen string
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		if data, err := os.ReadFile(filepath.Join(request.Execution.WorkspaceRoot, "a.txt")); err == nil {
			seen = string(data)
		}
		// The resume needs a transcript to copy.
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		request.OnTranscript(dir)
		return child.run(ctx, request)
	}}
	settings := writeSettings(isolatedAgent("fixer"))
	settings.StorageDir = t.TempDir()
	catalog, _ := buildSubagentCatalog(t, settings, launcher, readOnlyTool("read"), writeTool("exec"))
	first := worktreeCall(t, catalog, repo, `{"agent":"fixer","task":"one","block_ms":3000}`)
	<-child.executions
	run := runIDOf(t, first)
	child.files = map[string]string{"b.txt": "second\n"}
	second := worktreeCall(t, catalog, repo, `{"resume":"`+run+`","task":"two","block_ms":3000}`)
	<-child.executions
	if seen != "first\n" {
		t.Fatalf("the resumed run saw a.txt = %q, want the first run's change\n%s", seen, second)
	}
	if !strings.Contains(second, "worktree_changes: 1 file changed") || !strings.Contains(second, "resumed_from: "+run) {
		t.Fatalf("second result:\n%s", second)
	}
}

func TestWorkspacePolicyMovesTheOriginsRules(t *testing.T) {
	policy := sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{
		{Action: "deny", Path: "/repo/keep"},
		{Action: "allow", Path: "/repo/../elsewhere"},
		{Action: "allow", Path: "/shared"},
	}}
	moved := workspacePolicy(policy, tools.ExecutionContext{WorkspaceRoot: "/trees/sub_1", WorkspaceOrigin: "/repo"})
	want := []sessionfs.NativePathRule{{Action: "deny", Path: "/trees/sub_1/keep"}, {Action: "allow", Path: "/repo/../elsewhere"}, {Action: "allow", Path: "/shared"}}
	for index, rule := range moved.Paths {
		if rule != want[index] {
			t.Fatalf("rule %d = %+v, want %+v", index, rule, want[index])
		}
	}
	if policy.Paths[0].Path != "/repo/keep" {
		t.Fatal("the session's policy was changed in place")
	}
	if same := workspacePolicy(policy, tools.ExecutionContext{WorkspaceRoot: "/repo"}); len(same.Paths) != 3 || same.Paths[0].Path != "/repo/keep" {
		t.Fatalf("without an origin: %+v", same)
	}
	scope := movedScope(&tools.PathScope{Allow: []string{"src", "/repo/docs", "/other"}, Deny: []string{"src/gen"}}, "/repo", "/trees/sub_1")
	if strings.Join(scope.Allow, ",") != "/trees/sub_1/src,/trees/sub_1/docs,/other" || strings.Join(scope.Deny, ",") != "/trees/sub_1/src/gen" {
		t.Fatalf("scope = %+v", scope)
	}
}

// A writer in its own worktree is handed off as an async handle, and the wait for it
// names the branch.
func TestIsolatedRunIsAHandleAndItsWaitNamesTheBranch(t *testing.T) {
	repo := gitRepository(t)
	release := make(chan struct{})
	child := &worktreeChild{executions: make(chan tools.ExecutionContext, 1), policy: sessionfs.NativePathPolicy{}, files: map[string]string{"c.txt": "c\n"}}
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		<-release
		return child.run(ctx, request)
	}}
	settings := writeSettings(isolatedAgent("fixer"))
	settings.StorageDir = t.TempDir()
	catalog, _ := buildSubagentCatalog(t, settings, launcher, readOnlyTool("read"), writeTool("exec"))
	started := worktreeCall(t, catalog, repo, `{"agent":"fixer","task":"later","block_ms":0}`)
	if !strings.Contains(started, "is still running") || strings.Contains(started, "goes on after this turn ends") {
		t.Fatalf("start:\n%s", started)
	}
	close(release)
	<-child.executions
	if waited := asyncWait(t, catalog); !strings.Contains(waited, "worktree branch jad/sub_") || !strings.Contains(waited, "1 file changed") {
		t.Fatalf("wait:\n%s", waited)
	}
}
