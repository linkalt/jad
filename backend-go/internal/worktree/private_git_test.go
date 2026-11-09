package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// gitWith runs git in dir with extra variables, as a process of the run would.
func gitWith(t *testing.T, dir string, env []string, arguments ...string) string {
	t.Helper()
	out, _, err := runGit(context.Background(), testOptions(), gitCommand{dir: dir, args: arguments, env: env})
	if err != nil {
		t.Fatalf("git %v: %v", arguments, err)
	}
	return out
}

// A run's worktree is on a detached HEAD; its commits go to its private object folder, which
// the repository does not read, and only Finish puts them in the repository on a new branch.
func TestRunObjectsArePrivateUntilFinish(t *testing.T) {
	repo := repository(t)
	tree, err := Create(context.Background(), testOptions(), repo, "private", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(context.Background(), testOptions(), tree.Path, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		t.Fatal("the worktree's HEAD is a branch, want detached")
	}
	if BranchExists(context.Background(), testOptions(), repo, tree.Branch) {
		t.Fatal("the branch exists before the run finished")
	}
	common, _ := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if tree.Admin != filepath.Join(common, "worktrees", "private") || tree.Objects != filepath.Join(tree.Admin, ObjectsDirName) {
		t.Fatalf("admin %s, objects %s", tree.Admin, tree.Objects)
	}
	write(t, filepath.Join(tree.Workspace, "c.txt"), "c\n")
	env := append(tree.RunEnvironment(), "GIT_AUTHOR_NAME=r", "GIT_AUTHOR_EMAIL=r@r", "GIT_COMMITTER_NAME=r", "GIT_COMMITTER_EMAIL=r@r")
	gitWith(t, tree.Workspace, env, "add", "c.txt")
	gitWith(t, tree.Workspace, env, "commit", "--quiet", "-m", "own")
	own := gitWith(t, tree.Workspace, env, "rev-parse", "HEAD")
	if _, err := run(context.Background(), testOptions(), repo, "cat-file", "-e", own); err == nil {
		t.Fatal("the run's commit is in the repository's own objects before Finish")
	}
	if entries, _ := os.ReadDir(tree.Objects); len(entries) == 0 {
		t.Fatal("the private object folder is empty after the run's commit")
	}
	outcome, err := tree.Finish(context.Background(), "rest", nil)
	if err != nil || !outcome.Changed || outcome.Commit != own {
		t.Fatalf("outcome = %+v, err %v", outcome, err)
	}
	if got := gitIn(t, repo, "rev-parse", tree.Branch); got != own {
		t.Fatalf("branch = %s, want %s", got, own)
	}
	for _, gone := range []string{tree.Path, tree.Admin} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", gone, err)
		}
	}
}

// Collection reads the worktree through the folders jad made, not through the .git file a
// run can rewrite: a configuration planted behind a rewritten .git file never runs.
func TestFinishIgnoresARewrittenGitFile(t *testing.T) {
	repo := repository(t)
	tree, err := Create(context.Background(), testOptions(), repo, "rewritten", "")
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), "fake")
	gitIn(t, filepath.Dir(fake), "init", "--quiet", "--bare", fake)
	marker := filepath.Join(t.TempDir(), "ran")
	program := filepath.Join(t.TempDir(), "sign.sh")
	write(t, program, "#!/bin/sh\ntouch "+marker+"\nexit 1\n")
	if err := os.Chmod(program, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(fake, "config"), "[core]\n\tbare = false\n[commit]\n\tgpgSign = true\n[gpg]\n\tprogram = "+program+"\n")
	write(t, filepath.Join(tree.Path, ".git"), "gitdir: "+fake+"\n")
	write(t, filepath.Join(tree.Workspace, "c.txt"), "c\n")
	outcome, err := tree.Finish(context.Background(), "collected", nil)
	if err != nil || !outcome.Changed {
		t.Fatalf("outcome = %+v, err %v", outcome, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the configuration behind the rewritten .git file ran on the host")
	}
}

// With a launcher, the commands that read what the run left (status, add, commit, its HEAD,
// the pack of its objects) start through it; the ones that change the repository do not.
func TestFinishCollectsThroughTheLauncher(t *testing.T) {
	repo := repository(t)
	tree, err := Create(context.Background(), testOptions(), repo, "launched", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tree.Workspace, "c.txt"), "c\n")
	var mu sync.Mutex
	var launched []string
	launcher := func(git string, args []string, dir string) (Launch, error) {
		mu.Lock()
		defer mu.Unlock()
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") && !strings.Contains(arg, "=") {
				launched = append(launched, arg)
				break
			}
		}
		if dir != tree.Path {
			t.Errorf("launched in %s, want the worktree", dir)
		}
		program, err := exec.LookPath(git)
		return Launch{File: program, Argv0: git, Args: args, Env: os.Environ()}, err
	}
	outcome, err := tree.Finish(context.Background(), "collected", launcher)
	if err != nil || !outcome.Changed {
		t.Fatalf("outcome = %+v, err %v", outcome, err)
	}
	for _, want := range []string{"status", "add", "commit", "rev-parse", "pack-objects"} {
		if !slices.Contains(launched, want) {
			t.Errorf("%s did not start through the launcher: %v", want, launched)
		}
	}
	for _, host := range []string{"index-pack", "update-ref", "merge-tree"} {
		if slices.Contains(launched, host) {
			t.Errorf("%s started through the launcher", host)
		}
	}
}

// Finish checks the branch against the session's HEAD at that moment and names the paths
// that would conflict.
func TestFinishReportsConflictsWithTheSessionHead(t *testing.T) {
	repo := repository(t)
	clean, err := Create(context.Background(), testOptions(), repo, "clean", "")
	if err != nil {
		t.Fatal(err)
	}
	conflicting, err := Create(context.Background(), testOptions(), repo, "conflicting", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(clean.Workspace, "new.txt"), "new\n")
	write(t, filepath.Join(conflicting.Workspace, "a.txt"), "run\n")
	write(t, filepath.Join(repo, "a.txt"), "session\n")
	gitIn(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--quiet", "-am", "session")
	head := gitIn(t, repo, "rev-parse", "HEAD")
	outcome, err := clean.Finish(context.Background(), "clean", nil)
	if err != nil || outcome.Target != head || len(outcome.Conflicts) != 0 || outcome.CheckError != "" {
		t.Fatalf("clean outcome = %+v, err %v", outcome, err)
	}
	outcome, err = conflicting.Finish(context.Background(), "conflicting", nil)
	if err != nil || outcome.Target != head || !slices.Equal(outcome.Conflicts, []string{"a.txt"}) {
		t.Fatalf("conflicting outcome = %+v, err %v", outcome, err)
	}
}

// Base "working" starts the run from the session's uncommitted and untracked files; the
// branch holds only the run's own change, on the session's HEAD, and the session's index is
// untouched. A run that changes what the session had changed keeps its branch on the snapshot.
func TestWorkingBase(t *testing.T) {
	repo := repository(t)
	head := gitIn(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "a.txt"), "dirty\n")
	write(t, filepath.Join(repo, "untracked.txt"), "u\n")
	write(t, filepath.Join(repo, ".gitignore"), "ignored.txt\n")
	write(t, filepath.Join(repo, "ignored.txt"), "i\n")
	options := testOptions()
	options.Base = BaseWorking
	tree, err := Create(context.Background(), options, repo, "working", "")
	if err != nil {
		t.Fatal(err)
	}
	if tree.Base != head || tree.Start == head {
		t.Fatalf("base %s start %s, want a snapshot on %s", tree.Base, tree.Start, head)
	}
	for file, want := range map[string]string{"a.txt": "dirty\n", "untracked.txt": "u\n"} {
		if data, _ := os.ReadFile(filepath.Join(tree.Workspace, file)); string(data) != want {
			t.Fatalf("%s in the run = %q, want %q", file, data, want)
		}
	}
	if _, err := os.Stat(filepath.Join(tree.Workspace, "ignored.txt")); err == nil {
		t.Fatal("an ignored file is in the run")
	}
	write(t, filepath.Join(tree.Workspace, "sub", "b.txt"), "run\n")
	outcome, err := tree.Finish(context.Background(), "on working", nil)
	if err != nil || !outcome.Changed || outcome.OnWorkingState || outcome.Start != tree.Start {
		t.Fatalf("outcome = %+v, err %v", outcome, err)
	}
	if parent := gitIn(t, repo, "rev-parse", tree.Branch+"^"); parent != head {
		t.Fatalf("branch parent = %s, want HEAD %s", parent, head)
	}
	if files := gitIn(t, repo, "diff", "--name-only", head, tree.Branch); files != "sub/b.txt" {
		t.Fatalf("branch changes = %q, want only the run's", files)
	}
	if status := gitIn(t, repo, "status", "--porcelain"); !strings.Contains(status, " M a.txt") || !strings.Contains(status, "?? untracked.txt") {
		t.Fatalf("the session's state changed: %q", status)
	}

	conflict, err := Create(context.Background(), options, repo, "working-conflict", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(conflict.Workspace, "a.txt"), "run\n")
	outcome, err = conflict.Finish(context.Background(), "conflict", nil)
	if err != nil || !outcome.Changed || !outcome.OnWorkingState {
		t.Fatalf("conflict outcome = %+v, err %v", outcome, err)
	}
	if parent := gitIn(t, repo, "rev-parse", conflict.Branch+"^"); parent != conflict.Start {
		t.Fatalf("branch parent = %s, want the snapshot %s", parent, conflict.Start)
	}

	// Without a change in the session, the run starts from HEAD itself.
	gitIn(t, repo, "add", "--all")
	gitIn(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--quiet", "-m", "all")
	same, err := Create(context.Background(), options, repo, "working-clean", "")
	if err != nil {
		t.Fatal(err)
	}
	if same.Start != same.Base {
		t.Fatalf("a clean session gave a snapshot %s", same.Start)
	}
	if _, err := same.Finish(context.Background(), "x", nil); err != nil {
		t.Fatal(err)
	}
}

// A worktree an earlier version made has its branch checked out; Open finishes it on that
// branch, and drops the branch when it holds no change.
func TestOpenFinishesAnAttachedWorktree(t *testing.T) {
	repo := repository(t)
	head := gitIn(t, repo, "rev-parse", "HEAD")
	for _, changed := range []bool{true, false} {
		name := "jad/attached-unchanged"
		if changed {
			name = "jad/attached"
		}
		path := filepath.Join(t.TempDir(), "tree")
		gitIn(t, repo, "worktree", "add", "--quiet", "-b", name, path, head)
		if changed {
			write(t, filepath.Join(path, "c.txt"), "c\n")
		}
		tree, err := Open(context.Background(), testOptions(), State{Path: path, Branch: name, Base: head})
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := tree.Finish(context.Background(), "recovered", nil)
		if err != nil || outcome.Changed != changed {
			t.Fatalf("changed %v: outcome = %+v, err %v", changed, outcome, err)
		}
		if BranchExists(context.Background(), testOptions(), repo, name) != changed {
			t.Fatalf("changed %v: branch left = %v", changed, !changed)
		}
		if changed && gitIn(t, repo, "rev-parse", name) != outcome.Commit {
			t.Fatal("the branch is not the recovered commit")
		}
	}
}

// The commit Finish makes through a launcher (in the run's sandbox) is not signed: a signing
// program there has neither the person's agent nor the network, and a failed signature would
// leave the run's work behind. On the host the configured signing still applies.
func TestFinishThroughALauncherDoesNotSign(t *testing.T) {
	repo := repository(t)
	for _, setting := range [][]string{{"commit.gpgsign", "true"}, {"gpg.program", "false"}} {
		gitIn(t, repo, "config", setting[0], setting[1])
	}
	tree, err := Create(context.Background(), testOptions(), repo, "unsigned", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tree.Workspace, "s.txt"), "s\n")
	launcher := func(git string, args []string, dir string) (Launch, error) {
		program, err := exec.LookPath(git)
		return Launch{File: program, Argv0: git, Args: args, Env: os.Environ()}, err
	}
	outcome, err := tree.Finish(context.Background(), "collected", launcher)
	if err != nil || !outcome.Changed {
		t.Fatalf("outcome = %+v, err %v", outcome, err)
	}
	host, err := Create(context.Background(), testOptions(), repo, "signed", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(host.Workspace, "h.txt"), "h\n")
	if _, err := host.Finish(context.Background(), "collected", nil); err == nil {
		t.Fatal("the host commit ignored commit.gpgsign")
	}
}

// With uncommitted changes in the session, Finish also checks the branch against them (W′):
// the paths both change, and those of them a merge with the working state conflicts on. A
// clean session gets no such check.
func TestFinishChecksTheSessionsUncommittedFiles(t *testing.T) {
	repo := repository(t)
	head := gitIn(t, repo, "rev-parse", "HEAD")
	clean, err := Create(context.Background(), testOptions(), repo, "clean", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(clean.Workspace, "a.txt"), "run\n")
	outcome, err := clean.Finish(context.Background(), "clean", nil)
	if err != nil || outcome.Uncommitted != nil {
		t.Fatalf("clean session: outcome = %+v, err %v", outcome, err)
	}
	gitIn(t, repo, "branch", "-D", clean.Branch)

	tree, err := Create(context.Background(), testOptions(), repo, "dirty", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tree.Workspace, "a.txt"), "run\n")
	write(t, filepath.Join(tree.Workspace, "b.txt"), "run b\n")
	write(t, filepath.Join(tree.Workspace, "only-run.txt"), "r\n")
	// The session changes a.txt the other way (a conflict), adds b.txt with the same text
	// (overlap, no conflict) and has an untracked file the run does not touch.
	write(t, filepath.Join(repo, "a.txt"), "session\n")
	write(t, filepath.Join(repo, "b.txt"), "run b\n")
	write(t, filepath.Join(repo, "untracked.txt"), "u\n")
	status := gitIn(t, repo, "status", "--porcelain")
	outcome, err = tree.Finish(context.Background(), "dirty", nil)
	if err != nil || outcome.Target != head || len(outcome.Conflicts) != 0 {
		t.Fatalf("outcome = %+v, err %v", outcome, err)
	}
	check := outcome.Uncommitted
	if check == nil || check.Error != "" || check.Working == "" || check.Working == head {
		t.Fatalf("uncommitted = %+v", check)
	}
	if !slices.Equal(check.Overlap, []string{"a.txt", "b.txt"}) || !slices.Equal(check.Conflicts, []string{"a.txt"}) {
		t.Fatalf("overlap %v, conflicts %v", check.Overlap, check.Conflicts)
	}
	// The check leaves the session's files, index and refs alone.
	if got := gitIn(t, repo, "status", "--porcelain"); got != status {
		t.Fatalf("status after = %q, before %q", got, status)
	}
	if got := gitIn(t, repo, "rev-parse", "HEAD"); got != head {
		t.Fatal("HEAD moved")
	}
}
