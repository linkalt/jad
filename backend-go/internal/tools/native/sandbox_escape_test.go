package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/worktree"
)

// Regression tests for the escapes of docs/isolation-sandbox-design.md §7.2 (S1–S5) and U1,
// each against a real bwrap sandbox with the default writable paths.

// sandboxedBash runs command in a bwrap sandbox with the default writable paths, as a call of
// session in workspace root.
func sandboxedBash(t *testing.T, root, session, command string, writable ...string) string {
	t.Helper()
	settings := bashTestSettings(t, root)
	settings.Sandbox = bwrapSandbox()
	if len(writable) > 0 {
		settings.Sandbox.WritablePaths = writable
	}
	return executeNativeBash(context.Background(), map[string]any{"command": command}, settings, "", tools.ExecutionContext{
		CallID: "escape-call", WorkspaceRoot: root, SessionDir: t.TempDir(), SessionKey: session,
	})
}

func requireOutput(t *testing.T, output string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in:\n%s", want, output)
		}
	}
}

// S1: a sandboxed call cannot plant a hook in the repository's git directory, neither in the
// hooks folder nor by moving the git directory aside and putting a new one in its place;
// git itself still works.
func TestSandboxKeepsTheRepositoryHooksReadOnly(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	root := gitRepository(t)
	output := sandboxedBash(t, root, "s1", `printf '#!/bin/sh\ntouch /tmp/hooked\n' > .git/hooks/post-checkout 2>&1 || echo hook-refused
mv .git .git-real 2>&1 || echo move-refused
mkdir -p .git-new && mv .git-new .git 2>&1
echo b > b.txt && git add b.txt && git -c user.name=t -c user.email=t@t commit -q -m b && echo committed`)
	requireOutput(t, output, "hook-refused", "move-refused", "committed")
	if _, err := os.Stat(filepath.Join(root, ".git", "hooks", "post-checkout")); err == nil {
		t.Fatal("the sandbox planted a hook")
	}
	if _, err := os.Stat(filepath.Join(root, ".git-real")); err == nil {
		t.Fatal("the sandbox moved the git directory")
	}
}

// S2: a sandboxed call cannot set an fsmonitor (or anything else) in the repository's config.
func TestSandboxKeepsTheRepositoryConfigReadOnly(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	root := gitRepository(t)
	before, err := os.ReadFile(filepath.Join(root, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	output := sandboxedBash(t, root, "s2", `git config core.fsmonitor 'touch /tmp/monitored' 2>&1 || echo config-refused
echo '[core] fsmonitor = x' >> .git/config 2>&1 || echo append-refused`)
	requireOutput(t, output, "config-refused", "append-refused")
	if after, _ := os.ReadFile(filepath.Join(root, ".git", "config")); string(after) != string(before) {
		t.Fatalf("the sandbox changed the config:\n%s", after)
	}
}

// S3: each session's calls get a temp folder of their own; another session's temp folder,
// with its code, result files and code pipes, is hidden.
func TestSandboxTempFolderIsTheSessionsOwn(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	root := t.TempDir()
	first := sandboxedBash(t, root, "session-a", `echo secret > "$TMPDIR/a.txt" && echo "tmp=$TMPDIR"`)
	requireOutput(t, first, "tmp=")
	firstTmp := strings.TrimSpace(strings.SplitN(strings.SplitN(first, "tmp=", 2)[1], "\n", 2)[0])
	if firstTmp == "" || filepath.Dir(firstTmp) != sandboxTempRoot() {
		t.Fatalf("TMPDIR %q is not a folder of %s", firstTmp, sandboxTempRoot())
	}
	second := sandboxedBash(t, root, "session-b", `cat '`+firstTmp+`/a.txt' 2>&1 || echo other-hidden; ls "$(dirname "$TMPDIR")"; echo "tmp=$TMPDIR"; touch "$TMPDIR/b.txt" && echo own-writable`)
	requireOutput(t, second, "other-hidden", "own-writable")
	if strings.Contains(second, "secret") || strings.Contains(second, filepath.Base(firstTmp)+"\n") {
		t.Fatalf("another session's temp folder is visible:\n%s", second)
	}
	again := sandboxedBash(t, root, "session-a", `cat "$TMPDIR/a.txt"`)
	requireOutput(t, again, "secret")
}

// S4: a sandboxed call cannot see or change other runs' worktrees; a run in a worktree of its
// own keeps it, writable.
func TestSandboxHidesOtherRunsWorktrees(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	root := gitRepository(t)
	options := worktree.Options{Git: "git", BranchPrefix: "jad/", AuthorName: "t", AuthorEmail: "t@t", Timeout: 30_000_000_000}
	mine, err := worktree.Create(context.Background(), options, root, "mine", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := worktree.Create(context.Background(), options, root, "other", "")
	if err != nil {
		t.Fatal(err)
	}
	session := sandboxedBash(t, root, "s4", `ls .git/jad-worktrees; ls .git/worktrees; echo x > '`+other.Workspace+`/a.txt' 2>&1 || echo other-refused`)
	requireOutput(t, session, "other-refused")
	if strings.Contains(session, "mine") || strings.Contains(session, "other\n") {
		t.Fatalf("the session sees the runs' worktrees:\n%s", session)
	}
	run := sandboxedBash(t, mine.Workspace, "s4-run", `ls '`+filepath.Dir(mine.Path)+`'; cat '`+other.Workspace+`/a.txt' 2>&1 || echo other-hidden
echo mine > a.txt && git add a.txt && git -c user.name=t -c user.email=t@t commit -q -m mine && echo committed`)
	requireOutput(t, run, "other-hidden", "committed")
	if strings.Contains(run, "other\n") {
		t.Fatalf("the run sees another run's worktree:\n%s", run)
	}
	if data, _ := os.ReadFile(filepath.Join(other.Workspace, "a.txt")); string(data) != "a\n" {
		t.Fatalf("the other run's file changed: %q", data)
	}
}

// S5: a writable path inside another writable path is not bound on its own, so a call that
// replaced it with a symlink cannot make the symlink's target writable for the next call.
func TestSandboxDoesNotBindASymlinkedWritablePath(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	writable := []string{"{workspace}", "{workspace}/build", "{tmp}"}
	swap := sandboxedBash(t, root, "s5", `rmdir build && ln -s '`+outside+`' build && echo swapped`, writable...)
	requireOutput(t, swap, "swapped")
	next := sandboxedBash(t, root, "s5", `touch build/escaped 2>&1 || echo refused; touch inside.txt && echo workspace-writable`, writable...)
	requireOutput(t, next, "refused", "workspace-writable")
	if _, err := os.Stat(filepath.Join(outside, "escaped")); err == nil {
		t.Fatal("the symlink's target became writable")
	}
}

// U1: a hook fired for a call of a plan-mode run starts in the sandbox's plan-mode form.
func TestSandboxedHookOfAPlanModeCallCannotWriteTheProject(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	root := t.TempDir()
	hooks := hookTestSettings()
	hooks.Sandbox = bwrapSandbox()
	hooks.Sandbox.PlanWritablePaths = []string{"{tmp}"}
	script := `touch in.txt 2>&1 || echo refused; touch "$TMPDIR/t" && echo tmp-writable`
	plan := &tools.ExecutionContext{WorkspaceRoot: root, PlanMode: true, SessionKey: "u1"}
	section := runHookChild(context.Background(), hooks, "", root, hookChild{name: "probe", setting: "hooks.pre_tool", argv: []string{"sh", "-c", script}, cwd: root, call: plan})
	requireOutput(t, section, "refused", "tmp-writable")
	if _, err := os.Stat(filepath.Join(root, "in.txt")); err == nil {
		t.Fatal("a plan-mode hook wrote the project")
	}
	writer := &tools.ExecutionContext{WorkspaceRoot: root, SessionKey: "u1"}
	section = runHookChild(context.Background(), hooks, "", root, hookChild{name: "probe", setting: "hooks.pre_tool", argv: []string{"sh", "-c", script}, cwd: root, call: writer})
	if strings.Contains(section, "refused") {
		t.Fatalf("a hook of a writer call was limited:\n%s", section)
	}
}
