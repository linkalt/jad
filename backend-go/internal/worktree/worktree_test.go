package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testOptions() Options {
	return Options{Git: "git", BranchPrefix: "jad/", AuthorName: "jad", AuthorEmail: "jad@localhost", Timeout: 30 * time.Second}
}

// repository makes a repository with one commit holding a.txt and sub/b.txt, and no
// identity of its own (HOME points at an empty folder, so no global one either).
func repository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	write(t, filepath.Join(dir, "sub", "b.txt"), "b\n")
	gitIn(t, dir, "init", "--quiet")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--quiet", "-m", "init")
	return dir
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitIn(t *testing.T, dir string, arguments ...string) string {
	t.Helper()
	out, err := run(context.Background(), testOptions(), dir, arguments...)
	if err != nil {
		t.Fatalf("git %v: %v", arguments, err)
	}
	return out
}

func TestCreateFinishKeepsTheBranchOfAChange(t *testing.T) {
	repo := repository(t)
	head := gitIn(t, repo, "rev-parse", "HEAD")
	// An uncommitted change of the session's checkout is not in the worktree.
	write(t, filepath.Join(repo, "a.txt"), "dirty\n")
	tree, err := Create(context.Background(), testOptions(), repo, "run1", "")
	if err != nil {
		t.Fatal(err)
	}
	common, _ := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if tree.Path != filepath.Join(common, DefaultDirName, "run1") || tree.Workspace != tree.Path || tree.Branch != "jad/run1" || tree.Base != head {
		t.Fatalf("tree = %+v", tree)
	}
	if data, _ := os.ReadFile(filepath.Join(tree.Workspace, "a.txt")); string(data) != "a\n" {
		t.Fatalf("worktree a.txt = %q", data)
	}
	write(t, filepath.Join(tree.Workspace, "a.txt"), "changed\n")
	write(t, filepath.Join(tree.Workspace, "new.txt"), "new\n")
	outcome, err := tree.Finish(context.Background(), "jad: test run", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Changed || outcome.Kept || outcome.Commit == head || !strings.Contains(outcome.Stat, "2 files changed") {
		t.Fatalf("outcome = %+v", outcome)
	}
	if _, err := os.Stat(tree.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree folder still there: %v", err)
	}
	if !BranchExists(context.Background(), testOptions(), repo, "jad/run1") {
		t.Fatal("branch jad/run1 was dropped")
	}
	// The commit is signed with the configured author, because the repository has none.
	if author := gitIn(t, repo, "log", "-1", "--format=%an <%ae> %s", "jad/run1"); author != "jad <jad@localhost> jad: test run" {
		t.Fatalf("author = %q", author)
	}
	// The session's checkout keeps its own uncommitted change and its branch.
	if data, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(data) != "dirty\n" {
		t.Fatalf("session a.txt = %q", data)
	}
	if list := gitIn(t, repo, "worktree", "list"); strings.Contains(list, "run1") {
		t.Fatalf("worktree still listed: %s", list)
	}
}

func TestFinishWithoutAChangeDropsTheBranch(t *testing.T) {
	repo := repository(t)
	tree, err := Create(context.Background(), testOptions(), repo, "run2", "")
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := tree.Finish(context.Background(), "unused", nil)
	if err != nil || outcome.Changed || outcome.Stat != "" || outcome.Commit != tree.Base {
		t.Fatalf("outcome = %+v, err %v", outcome, err)
	}
	if BranchExists(context.Background(), testOptions(), repo, "jad/run2") {
		t.Fatal("branch jad/run2 is left")
	}
}

// A commit the run made itself counts as a change, and a configured identity wins over
// the options' author.
func TestFinishKeepsTheRunsOwnCommit(t *testing.T) {
	repo := repository(t)
	gitIn(t, repo, "config", "user.name", "Owner")
	gitIn(t, repo, "config", "user.email", "owner@example.com")
	tree, err := Create(context.Background(), testOptions(), repo, "run3", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tree.Workspace, "c.txt"), "c\n")
	gitIn(t, tree.Workspace, "add", "c.txt")
	gitIn(t, tree.Workspace, "commit", "--quiet", "-m", "own")
	write(t, filepath.Join(tree.Workspace, "d.txt"), "d\n")
	outcome, err := tree.Finish(context.Background(), "rest", nil)
	if err != nil || !outcome.Changed {
		t.Fatalf("outcome = %+v, err %v", outcome, err)
	}
	if log := gitIn(t, repo, "log", "--format=%an %s", tree.Base+"..jad/run3"); log != "Owner rest\nOwner own" {
		t.Fatalf("log = %q", log)
	}
}

// A session working in a subfolder works in the same subfolder of the worktree, and a
// later run can start from an earlier run's branch.
func TestCreateInASubfolderAndFromABranch(t *testing.T) {
	repo := repository(t)
	first, err := Create(context.Background(), testOptions(), filepath.Join(repo, "sub"), "run4", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Workspace != filepath.Join(first.Path, "sub") {
		t.Fatalf("workspace = %s", first.Workspace)
	}
	write(t, filepath.Join(first.Workspace, "b.txt"), "b2\n")
	if _, err := first.Finish(context.Background(), "first", nil); err != nil {
		t.Fatal(err)
	}
	second, err := Create(context.Background(), testOptions(), repo, "run5", "jad/run4")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = second.Finish(context.Background(), "second", nil) }()
	if data, _ := os.ReadFile(filepath.Join(second.Workspace, "sub", "b.txt")); string(data) != "b2\n" {
		t.Fatalf("second run sees b.txt = %q", data)
	}
	if _, err := Create(context.Background(), testOptions(), repo, "run6", "jad/missing"); err == nil || !strings.Contains(err.Error(), "does not name a commit") {
		t.Fatalf("missing revision: %v", err)
	}
}

func TestCreateRefusals(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	plain := t.TempDir()
	if _, err := Create(context.Background(), testOptions(), plain, "x", ""); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("plain folder: %v", err)
	}
	empty := t.TempDir()
	gitIn(t, empty, "init", "--quiet")
	if _, err := Create(context.Background(), testOptions(), empty, "x", ""); err == nil || !strings.Contains(err.Error(), "no commit") {
		t.Fatalf("empty repository: %v", err)
	}
	repo := repository(t)
	tree, err := Create(context.Background(), testOptions(), repo, "dup", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = tree.Finish(context.Background(), "x", nil) }()
	if _, err := Create(context.Background(), testOptions(), repo, "dup", ""); err == nil {
		t.Fatal("a second worktree of the same name was created")
	}
}

// Several runs of one repository start and finish at once without tripping on git's locks.
func TestConcurrentRuns(t *testing.T) {
	repo := repository(t)
	var wait sync.WaitGroup
	errs := make(chan error, 8)
	for index := range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			name := "par" + string(rune('a'+index))
			tree, err := Create(context.Background(), testOptions(), repo, name, "")
			if err != nil {
				errs <- err
				return
			}
			if err := os.WriteFile(filepath.Join(tree.Workspace, name+".txt"), []byte(name), 0o644); err != nil {
				errs <- err
				return
			}
			if _, err := tree.Finish(context.Background(), name, nil); err != nil {
				errs <- err
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if branches := gitIn(t, repo, "branch", "--list", "jad/*"); strings.Count(branches, "jad/par") != 8 {
		t.Fatalf("branches = %q", branches)
	}
}

// A configured folder holds the worktrees instead of the git directory.
func TestConfiguredDir(t *testing.T) {
	repo := repository(t)
	options := testOptions()
	options.Dir = filepath.Join(t.TempDir(), "trees")
	tree, err := Create(context.Background(), options, repo, "run7", "")
	if err != nil {
		t.Fatal(err)
	}
	if tree.Path != filepath.Join(options.Dir, "run7") {
		t.Fatalf("path = %s", tree.Path)
	}
	if _, err := tree.Finish(context.Background(), "x", nil); err != nil {
		t.Fatal(err)
	}
}
