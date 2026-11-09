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

func isolatedTestOptions() worktree.Options {
	return worktree.Options{Git: "git", BranchPrefix: "jad/", AuthorName: "t", AuthorEmail: "t@t", Timeout: 30_000_000_000}
}

// isolatedTree is a registered run's worktree of a new repository, forgotten and removed at
// the end of the test.
func isolatedTree(t *testing.T, name string) (string, *worktree.Tree) {
	t.Helper()
	root := gitRepository(t)
	tree, err := worktree.Create(context.Background(), isolatedTestOptions(), root, name, "")
	if err != nil {
		t.Fatal(err)
	}
	registerIsolatedRun(tree)
	t.Cleanup(func() { forgetIsolatedRun(tree) })
	return root, tree
}

// D13 and §9.3 step 6: in an isolated run's sandbox {git_common_dir} is the run's own admin
// folder. Its git commits on the detached HEAD into its private objects; branches, tags and
// the shared objects and refs are read-only, so the run changes nothing another run or the
// session sees.
func TestIsolatedRunWritesOnlyItsOwnGitState(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	root, tree := isolatedTree(t, "own-git")
	common := tree.Common()
	output := sandboxedBash(t, tree.Workspace, "isolated", `echo run > a.txt && git add a.txt && git -c user.name=t -c user.email=t@t commit -q -m run && echo committed
git branch side 2>&1 || echo branch-refused
git tag v1 2>&1 || echo tag-refused
touch '`+filepath.Join(common, "objects", "planted")+`' 2>&1 || echo objects-refused
echo "objects=$GIT_OBJECT_DIRECTORY"`)
	requireOutput(t, output, "committed", "branch-refused", "tag-refused", "objects-refused", "objects="+tree.Objects)
	if entries, _ := os.ReadDir(tree.Objects); len(entries) == 0 {
		t.Fatal("the run's commit is not in its private objects")
	}
	if branches := gitCommand(t, root, "branch", "--list"); strings.Contains(branches, "side") {
		t.Fatalf("the run made a branch: %s", branches)
	}
	outcome, err := tree.Finish(context.Background(), "x", nil)
	if err != nil || !outcome.Changed {
		t.Fatalf("outcome = %+v, err %v", outcome, err)
	}
}

// A run that rewrites its worktree's .git file cannot move {git_common_dir} for its next
// call: jad takes the run's git folders from its own record.
func TestIsolatedRunCannotRedirectItsGitDirectory(t *testing.T) {
	_, tree := isolatedTree(t, "redirect")
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "commondir"), []byte(elsewhere+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree.Path, ".git"), []byte("gitdir: "+elsewhere+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	place := sandboxPlace{workspace: tree.Workspace, cwd: tree.Workspace}
	if got := sandboxExpander(place)("{git_common_dir}"); got != tree.Common() {
		t.Fatalf("{git_common_dir} = %s, want %s", got, tree.Common())
	}
	if got := sandboxWritePaths([]string{"{workspace}", "{git_common_dir}", "{git_common_dir}/refs", "{tmp}"}, place); strings.Join(got, ",") != "{workspace},"+tree.Admin+",{tmp}" {
		t.Fatalf("write paths = %v", got)
	}
	if got := workspaceGitDir(tree.Workspace, tree.Common()); got != tree.Admin {
		t.Fatalf("own git dir = %s, want %s", got, tree.Admin)
	}
}

// A session in a linked worktree cannot rewrite the pointers jad reads its git directory
// through: the .git file and the admin folder's commondir and gitdir stay read-only.
func TestSandboxKeepsALinkedWorktreesPointersReadOnly(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	root := gitRepository(t)
	linked := filepath.Join(t.TempDir(), "linked")
	gitCommand(t, root, "worktree", "add", "--quiet", "--detach", linked)
	admin := strings.TrimSpace(gitCommand(t, linked, "rev-parse", "--absolute-git-dir"))
	output := sandboxedBash(t, linked, "linked", `echo 'gitdir: /' > .git 2>&1 || echo dotgit-refused
echo / > '`+admin+`/commondir' 2>&1 || echo commondir-refused
echo x > a.txt && echo workspace-writable`)
	requireOutput(t, output, "dotgit-refused", "commondir-refused", "workspace-writable")
	if data, _ := os.ReadFile(filepath.Join(linked, ".git")); !strings.Contains(string(data), admin) {
		t.Fatalf(".git file changed: %q", data)
	}
}

// The commands that collect an isolated run's work run in the run's own sandbox, and work
// even when that sandbox writes only scratch: the run's admin folder is added for them.
func TestCollectRunsInTheRunsSandbox(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	root, tree := isolatedTree(t, "collect")
	if err := os.WriteFile(filepath.Join(tree.Workspace, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sandbox := bwrapSandbox()
	sandbox.WritablePaths, sandbox.PlanWritablePaths = []string{"{tmp}"}, []string{"{tmp}"}
	execution := tools.ExecutionContext{WorkspaceRoot: tree.Workspace, SessionKey: "collect", SessionDir: t.TempDir(), Environment: &tools.Environment{Sandbox: sandbox}}
	launcher := collectLauncher(execution, tree)
	if launcher == nil {
		t.Fatal("no launcher for a sandboxed run")
	}
	launch, err := launcher("git", []string{"status"}, tree.Path)
	if err != nil || filepath.Base(launch.File) != "bwrap" {
		t.Fatalf("launch = %+v, err %v", launch, err)
	}
	outcome, err := tree.Finish(context.Background(), "collected", launcher)
	if err != nil || !outcome.Changed {
		t.Fatalf("outcome = %+v, err %v", outcome, err)
	}
	if files := gitCommand(t, root, "show", "--name-only", "--format=", tree.Branch); !strings.Contains(files, "new.txt") {
		t.Fatalf("branch files = %q", files)
	}
	if collectLauncher(tools.ExecutionContext{WorkspaceRoot: tree.Workspace}, tree) != nil {
		t.Fatal("a launcher for a run without a sandbox")
	}
}
