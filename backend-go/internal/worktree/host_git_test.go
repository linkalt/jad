package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeHook installs an executable hook that leaves a marker file when it runs.
func writeHook(t *testing.T, repo, name, marker string) {
	t.Helper()
	path := filepath.Join(repo, ".git", "hooks", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// S1: a hook a sandboxed run planted in the repository's git directory never runs in the git
// commands jad runs on the host (worktree add runs post-checkout, commit runs pre-commit).
func TestHostGitIgnoresRepositoryHooks(t *testing.T) {
	repo := repository(t)
	marker := filepath.Join(t.TempDir(), "ran")
	for _, hook := range []string{"post-checkout", "pre-commit", "post-commit", "reference-transaction"} {
		writeHook(t, repo, hook, marker)
	}
	tree, err := Create(context.Background(), testOptions(), repo, "hooked", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tree.Workspace, "c.txt"), "c\n")
	if outcome, err := tree.Finish(context.Background(), "change", nil); err != nil || !outcome.Changed {
		t.Fatalf("outcome = %+v, err %v", outcome, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a repository hook ran in jad's host git")
	}
}

// commit_hooks still runs the repository's commit hooks for the commit Finish makes, as the
// setting asks, and only for it.
func TestCommitHooksRunOnlyForTheCommit(t *testing.T) {
	repo := repository(t)
	commitMarker := filepath.Join(t.TempDir(), "commit")
	checkoutMarker := filepath.Join(t.TempDir(), "checkout")
	writeHook(t, repo, "pre-commit", commitMarker)
	writeHook(t, repo, "post-checkout", checkoutMarker)
	options := testOptions()
	options.CommitHooks = true
	tree, err := Create(context.Background(), options, repo, "hooks-on", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tree.Workspace, "c.txt"), "c\n")
	if _, err := tree.Finish(context.Background(), "change", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(commitMarker); err != nil {
		t.Fatal("commit_hooks did not run the pre-commit hook")
	}
	if _, err := os.Stat(checkoutMarker); err == nil {
		t.Fatal("the post-checkout hook ran in worktree add")
	}
}

// S2: an fsmonitor command a sandboxed run set in the repository's config never runs in the
// status jad runs on the host when a run ends.
func TestHostGitIgnoresFsmonitor(t *testing.T) {
	repo := repository(t)
	tree, err := Create(context.Background(), testOptions(), repo, "monitored", "")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "monitor.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Written as a run would write it: straight into the shared config, not through jad's git.
	config, err := os.OpenFile(filepath.Join(repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.WriteString("[core]\n\tfsmonitor = " + script + "\n"); err != nil {
		t.Fatal(err)
	}
	config.Close()
	write(t, filepath.Join(tree.Workspace, "c.txt"), "c\n")
	if _, err := tree.Finish(context.Background(), "change", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the repository's fsmonitor ran in jad's host git")
	}
}

// Open finishes a worktree an earlier server left: it commits what is there, removes the
// folder and keeps the branch; a folder that is gone is ErrGone. A record of an earlier
// version names only the folder, branch and base, and still opens.
func TestOpenFinishesALeftWorktree(t *testing.T) {
	repo := repository(t)
	for _, legacy := range []bool{false, true} {
		name := "left"
		if legacy {
			name = "left-legacy"
		}
		tree, err := Create(context.Background(), testOptions(), repo, name, "")
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(tree.Workspace, "c.txt"), "c\n")
		state := tree.State()
		if legacy {
			state = State{Path: tree.Path, Branch: tree.Branch, Base: tree.Base}
		}
		opened, err := Open(context.Background(), testOptions(), state)
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := opened.Finish(context.Background(), "recovered", nil)
		if err != nil || !outcome.Changed || outcome.Kept {
			t.Fatalf("legacy %v: outcome = %+v, err %v", legacy, outcome, err)
		}
		if _, err := os.Stat(tree.Path); !os.IsNotExist(err) {
			t.Fatalf("the worktree folder is still there: %v", err)
		}
		if !BranchExists(context.Background(), testOptions(), repo, tree.Branch) {
			t.Fatalf("the branch of the recovered run %s is gone", tree.Branch)
		}
		if _, err := Open(context.Background(), testOptions(), state); !errors.Is(err, ErrGone) {
			t.Fatalf("Open of a removed folder = %v, want ErrGone", err)
		}
	}
}
