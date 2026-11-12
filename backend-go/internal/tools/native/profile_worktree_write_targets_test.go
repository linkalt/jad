package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// A worktree run resolves exec's spill files against its own worktree, after the load judged
// them against the session's cwd (3b.2): a spill folder that reaches the workspace's profile-own
// through .. or through a link committed in the repository fails that call before the command
// runs or anything is pruned or written; a spill folder inside the worktree still works.
func TestWorktreeRunSpillIntoProfileOwnFailsTheCall(t *testing.T) {
	requireInterpreters(t, "bash")
	repo := gitRepository(t)
	own := filepath.Join(repo, ".jad", "profile-own")
	coder := filepath.Join(own, "dev", "coder")
	if err := os.MkdirAll(filepath.Join(coder, "documents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(coder, filepath.Join(repo, "out")); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repo, "add", "out")
	gitCommand(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--quiet", "-m", "link")
	execution := tools.ExecutionContext{WorkspaceRoot: repo}
	tree, err := OpenDelegatedWorktree(context.Background(), nil, sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationWorktree}, &execution, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tree.Close(context.Background(), execution, "m") })
	root := execution.WorkspaceRoot
	if root == repo {
		t.Fatal("the run was not moved into its worktree")
	}
	climb, err := filepath.Rel(root, filepath.Join(coder, "spill"))
	if err != nil || !strings.HasPrefix(climb, "..") {
		t.Fatalf("relative path %q: %v", climb, err)
	}
	run := func(storage string) string {
		bash := bashTestSettings(t, root)
		bash.Spill = sessionfs.NativeBashSpillSettings{Enabled: true, Keep: "always", CapBytes: 1024, StorageDir: storage}
		bash.ProfileOwnRoots = []string{own}
		settings := execTestSettings()
		settings.Child = bash
		return executeNativeExec(context.Background(), normalizeExecArguments(map[string]any{"lang": "bash", "code": "touch ran; echo out"}, settings.DefaultLang), settings, "",
			tools.ExecutionContext{CallID: "exec-call", WorkspaceRoot: root, SessionDir: t.TempDir()})
	}
	for _, storage := range []string{climb, "out/spill", filepath.Join(coder, "spill")} {
		result := run(storage)
		if !strings.Contains(result, "write_target_refused") || !strings.Contains(result, "tools.native.exec.spill.storage_dir") {
			t.Fatalf("spill %s: %s", storage, result)
		}
		if _, err := os.Lstat(filepath.Join(coder, "spill")); err == nil {
			t.Fatalf("spill %s: a folder was made in profile-own", storage)
		}
		if _, err := os.Lstat(filepath.Join(root, "ran")); err == nil {
			t.Fatalf("spill %s: the command ran", storage)
		}
	}
	result := run(".spill")
	if strings.Contains(result, "write_target_refused") || execContent(result) != "out\n" {
		t.Fatalf("a spill folder inside the worktree: %s", result)
	}
	if entries, err := os.ReadDir(filepath.Join(root, ".spill")); err != nil || len(entries) == 0 {
		t.Fatalf("no spill file in the worktree: %v %v", entries, err)
	}
}

// The function library is judged the same way: one that resolves into profile-own is refused
// with the setting named, one elsewhere is resolved as before, and without the roots (v1)
// nothing changes.
func TestFunctionsDirInProfileOwnIsRefused(t *testing.T) {
	root := t.TempDir()
	own := filepath.Join(root, ".jad", "profile-own")
	inside := filepath.Join(own, "dev", "coder")
	for _, dir := range []string{filepath.Join(inside, "fns"), filepath.Join(root, "fns")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := resolveToolSmithFunctionsDir("fns", inside, []string{own}); err == nil || !strings.Contains(err.Error(), "functions_dir") || !strings.Contains(err.Error(), "profile-own") {
		t.Fatalf("a library in profile-own: %v", err)
	}
	if dir, _, err := resolveToolSmithFunctionsDir("fns", inside, nil); err != nil || !strings.HasSuffix(dir, filepath.Join("coder", "fns")) {
		t.Fatalf("without roots: %q %v", dir, err)
	}
	if _, _, err := resolveToolSmithFunctionsDir("fns", root, []string{own}); err != nil {
		t.Fatalf("a library outside profile-own: %v", err)
	}
}
