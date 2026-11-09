package native

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/worktree"
)

// failableGit is a git that fails every command while the returned flag file exists, so a test
// can make the collection of a run fail and then let it succeed.
func failableGit(t *testing.T) (git string, flag string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	flag = filepath.Join(dir, "fail")
	git = filepath.Join(dir, "git")
	script := "#!/bin/sh\nif [ -e '" + flag + "' ]; then echo 'git is set to fail' >&2; exit 1; fi\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(git, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return git, flag
}

func setFailing(t *testing.T, flag string, failing bool) {
	t.Helper()
	if failing {
		if err := os.WriteFile(flag, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	_ = os.Remove(flag)
}

// writeFinishedWorktreeRun stores the manifest of a run that ended while its worktree's
// collection failed: the folders are kept and the record says how often it failed.
func writeFinishedWorktreeRun(t *testing.T, root, id string, record subagentWorktree) {
	t.Helper()
	writeLostWorktreeRun(t, root, id, record)
	path := filepath.Join(root, subagentManifestDir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"state":"running"`, `"state":"failed"`, 1))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// F3, sub-agent runs: a run that ended with its worktree kept (the collection failed) is tried
// again at each server start, up to collect_attempts times, with the failures counted in its
// manifest; the folder and its work stay while it fails and when the tries are used up, and
// the run is not dropped by keep_finished meanwhile.
func TestFailedCollectionOfASubagentRunIsRetriedAtStartAndKeptWhenItKeepsFailing(t *testing.T) {
	repo := gitRepository(t)
	git, flag := failableGit(t)
	settings := sessionfs.NativeSubagentWorktreeSettings{Git: git, CollectAttempts: 3}
	tree, err := worktree.Create(context.Background(), worktreeOptions(settings), repo, "sub_0000a001", "")
	if err != nil {
		t.Fatal(err)
	}
	left := filepath.Join(tree.Workspace, "left.txt")
	if err := os.WriteFile(left, []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := tree.State()
	root := t.TempDir()
	writeFinishedWorktreeRun(t, root, "sub_0000a001", subagentWorktree{Branch: tree.Branch, Base: tree.Base, Path: tree.Path, Workspace: state.Workspace, Admin: state.Admin, Common: state.Common, Top: state.Top, Error: "git commit: failed", Attempts: 1})
	// An ordinary ended run of the same owner, which keep_finished 0 drops.
	writeFinishedWorktreeRun(t, root, "sub_0000a009", subagentWorktree{Branch: "jad/sub_0000a009", Base: tree.Base, Commit: tree.Base})
	start := func() *subagentRun {
		manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 0, Worktree: settings})
		t.Cleanup(func() { _ = manager.Close() })
		return manager.byID("sub_0000a001")
	}
	// Start 1: it fails again: attempt 2, everything kept.
	setFailing(t, flag, true)
	run := start()
	if run == nil {
		t.Fatal("a run with a kept worktree was dropped by keep_finished 0")
	}
	if _, err := os.Stat(filepath.Join(root, subagentManifestDir, "sub_0000a009.json")); !os.IsNotExist(err) {
		t.Fatalf("keep_finished 0 did not drop the ordinary ended run: %v", err)
	}
	if record := run.view().worktree; record == nil || record.Attempts != 2 || record.Path != tree.Path || record.Error == "" {
		t.Fatalf("after a failed retry: %+v", record)
	}
	if saved := manifestOf(t, root, "sub_0000a001"); saved.Worktree == nil || saved.Worktree.Attempts != 2 {
		t.Fatalf("the manifest does not record the failed retry: %+v", saved.Worktree)
	}
	// Start 2: the third and last attempt fails too.
	if record := start().view().worktree; record == nil || record.Attempts != 3 || record.Path != tree.Path {
		t.Fatalf("after the last attempt: %+v", record)
	}
	// Start 3, with git working again: the tries are used up, so nothing is tried and nothing is
	// deleted: the folder, its uncommitted file, no branch.
	setFailing(t, flag, false)
	record := start().view().worktree
	if record == nil || record.Attempts != 3 || record.Path != tree.Path || record.Commit != "" {
		t.Fatalf("after the tries are used up: %+v", record)
	}
	if data, err := os.ReadFile(left); err != nil || string(data) != "work\n" {
		t.Fatalf("the run's work was touched: %q %v", data, err)
	}
	if worktree.BranchExists(context.Background(), worktreeOptions(settings), repo, tree.Branch) {
		t.Fatal("a branch was made after the tries were used up")
	}
	// More tries allowed (the setting raised): the next start collects it.
	settings.CollectAttempts = 4
	record = func() *subagentWorktree {
		// Collected, the run is an ended one like any other again: kept here by keep_finished.
		manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10, Worktree: settings})
		t.Cleanup(func() { _ = manager.Close() })
		return manager.byID("sub_0000a001").view().worktree
	}()
	if record == nil || record.Path != "" || !record.Changed || record.Commit == "" || record.Error != "" {
		t.Fatalf("after the retry that works: %+v", record)
	}
	if files := gitCommand(t, repo, "show", "--name-only", "--format=", tree.Branch); !strings.Contains(files, "left.txt") {
		t.Fatalf("the branch does not hold the run's file: %q", files)
	}
}

// F3: a failed collection is also tried again before the next isolated run starts (the manager's
// own runs), each run at most once at a time.
func TestFailedCollectionIsRetriedBeforeTheNextIsolatedRun(t *testing.T) {
	repo := gitRepository(t)
	git, flag := failableGit(t)
	settings := sessionfs.NativeSubagentWorktreeSettings{Git: git, CollectAttempts: 3}
	options := worktreeOptions(settings)
	tree, err := worktree.Create(context.Background(), options, repo, "sub_0000a002", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree.Workspace, "later.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := tree.State()
	root := t.TempDir()
	writeFinishedWorktreeRun(t, root, "sub_0000a002", subagentWorktree{Branch: tree.Branch, Base: tree.Base, Path: tree.Path, Workspace: state.Workspace, Admin: state.Admin, Common: state.Common, Top: state.Top, Error: "git status: failed", Attempts: 1})
	setFailing(t, flag, true)
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10, Worktree: settings})
	t.Cleanup(func() { _ = manager.Close() })
	run := manager.byID("sub_0000a002")
	if record := run.view().worktree; record == nil || record.Attempts != 2 {
		t.Fatalf("start: %+v", record)
	}
	// A retry in flight is not started a second time.
	run.mu.Lock()
	run.retrying = true
	run.mu.Unlock()
	setFailing(t, flag, false)
	manager.retryPendingWorktrees(context.Background(), options)
	if record := run.view().worktree; record.Path == "" {
		t.Fatal("a run being retried was retried again")
	}
	run.mu.Lock()
	run.retrying = false
	run.mu.Unlock()
	manager.retryPendingWorktrees(context.Background(), options)
	record := run.view().worktree
	if record.Path != "" || !record.Changed || record.Error != "" {
		t.Fatalf("after the next run's retry: %+v", record)
	}
	if files := gitCommand(t, repo, "show", "--name-only", "--format=", tree.Branch); !strings.Contains(files, "later.txt") {
		t.Fatalf("the branch does not hold the run's file: %q", files)
	}
	if run.retrying {
		t.Fatal("the retry was left marked as going")
	}
}

// openFailingDelegatedRun opens a delegated worktree with git that can be made to fail, leaves a
// file in it and closes it with git failing, so that its collection fails.
func openFailingDelegatedRun(t *testing.T, repo, stateDir string, settings *sessionfs.NativeSubagentSettings, flag, materialize string) (*DelegatedWorktree, DelegatedWorktreeResult) {
	t.Helper()
	execution := tools.ExecutionContext{WorkspaceRoot: repo}
	tree, err := OpenDelegatedWorktree(context.Background(), settings, sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationWorktree, Materialize: materialize}, &execution, stateDir, "jad profile run x\n\nwrite it")
	if err != nil {
		t.Fatal(err)
	}
	if materialize == sessionfs.SandboxIsolationMaterializeOverlay {
		holderBash(t, tree.tree, nil, false, "echo work > left.txt")
	} else if err := os.WriteFile(filepath.Join(tree.tree.Workspace, "left.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setFailing(t, flag, true)
	result := tree.Close(context.Background(), execution, "close")
	setFailing(t, flag, false)
	return tree, result
}

func delegatedSettings(git string, attempts int) *sessionfs.NativeSubagentSettings {
	return &sessionfs.NativeSubagentSettings{Worktree: sessionfs.NativeSubagentWorktreeSettings{Git: git, CollectAttempts: attempts}}
}

// F3, delegated runs: a run whose collection failed keeps its record and its folder; a later
// start or run tries again, counts the failures, and after collect_attempts keeps the folder and
// the record for manual recovery without deleting anything. A record another process works on is
// left alone.
func TestFailedCollectionOfADelegatedRunKeepsItsRecordAndIsRetried(t *testing.T) {
	repo := gitRepository(t)
	git, flag := failableGit(t)
	stateDir := t.TempDir()
	settings := delegatedSettings(git, 3)
	tree, result := openFailingDelegatedRun(t, repo, stateDir, settings, flag, "")
	if !strings.Contains(result.Text, tree.tree.Path) {
		t.Fatalf("the caller is not told where the kept folder is: %q", result.Text)
	}
	record, err := readDelegatedWorktreeRecord(tree.record)
	if err != nil || !record.Pending || record.Attempts != 1 || record.Error == "" {
		t.Fatalf("record after the failed close: %+v, %v", record, err)
	}
	if _, err := os.Stat(tree.tree.Path); err != nil {
		t.Fatalf("the folder of a failed collection is gone: %v", err)
	}
	// Another process works on the record: skipped, and not counted.
	held, err := lockRecord(tree.record + ".lock")
	if err != nil || held == nil {
		t.Fatalf("lock: %v", err)
	}
	if got := RecoverDelegatedWorktrees(context.Background(), stateDir, settings, nil); len(got) != 0 {
		t.Fatalf("a record another process holds was tried: %+v", got)
	}
	_ = held.Close()
	// A start with git failing: attempt 2, record and folder kept.
	setFailing(t, flag, true)
	got := RecoverDelegatedWorktrees(context.Background(), stateDir, settings, nil)
	setFailing(t, flag, false)
	if len(got) != 1 || got[0].Path != tree.tree.Path || got[0].Error == "" {
		t.Fatalf("recovery with git failing = %+v", got)
	}
	if record, _ := readDelegatedWorktreeRecord(tree.record); !record.Pending || record.Attempts != 2 {
		t.Fatalf("record after attempt 2: %+v", record)
	}
	// The last attempt (3) fails too; afterwards nothing is tried, though git works.
	setFailing(t, flag, true)
	RecoverDelegatedWorktrees(context.Background(), stateDir, settings, nil)
	setFailing(t, flag, false)
	if record, _ := readDelegatedWorktreeRecord(tree.record); record.Attempts != 3 {
		t.Fatalf("record after attempt 3: %+v", record)
	}
	if got := RecoverDelegatedWorktrees(context.Background(), stateDir, settings, nil); len(got) != 0 {
		t.Fatalf("a record whose tries are used up was tried: %+v", got)
	}
	execution := tools.ExecutionContext{WorkspaceRoot: repo}
	next, err := OpenDelegatedWorktree(context.Background(), settings, sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationWorktree}, &execution, stateDir, "m")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close(context.Background(), execution, "done")
	if data, err := os.ReadFile(filepath.Join(tree.tree.Workspace, "left.txt")); err != nil || string(data) != "work\n" {
		t.Fatalf("the kept run's work was touched: %q %v", data, err)
	}
	if _, err := os.Stat(tree.record); err != nil {
		t.Fatalf("the record of a run kept for manual recovery was deleted: %v", err)
	}
	if worktree.BranchExists(context.Background(), worktreeOptions(settings.Worktree), repo, tree.tree.Branch) {
		t.Fatal("a branch was made after the tries were used up")
	}
}

// F3: the next delegated run, with the tries not used up, collects what the failed one left.
func TestNextDelegatedRunCollectsAFailedOne(t *testing.T) {
	repo := gitRepository(t)
	git, flag := failableGit(t)
	stateDir := t.TempDir()
	settings := delegatedSettings(git, 3)
	failed, _ := openFailingDelegatedRun(t, repo, stateDir, settings, flag, "")
	execution := tools.ExecutionContext{WorkspaceRoot: repo}
	next, err := OpenDelegatedWorktree(context.Background(), settings, sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationWorktree}, &execution, stateDir, "m")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close(context.Background(), execution, "done")
	if text := gitCommand(t, repo, "show", failed.tree.Branch+":left.txt"); text != "work" {
		t.Fatalf("left.txt on the failed run's branch = %q", text)
	}
	for _, gone := range []string{failed.tree.Path, failed.record, failed.record + ".lock"} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", gone, err)
		}
	}
}

// F3 with an overlay run: the failed collection leaves the layers marked collect-pending in the
// state file, Remove refuses to delete them, and the next start collects the run in a holder of
// its own.
func TestFailedCollectionOfAnOverlayRunIsPersistedAndRetried(t *testing.T) {
	useOwnTempDir(t)
	requireOverlay(t)
	repo := gitRepository(t)
	git, flag := failableGit(t)
	stateDir := t.TempDir()
	settings := delegatedSettings(git, 3)
	tree, _ := openFailingDelegatedRun(t, repo, stateDir, settings, flag, sessionfs.SandboxIsolationMaterializeOverlay)
	state := tree.tree.State()
	layers, overlay := tree.tree.Layers()
	if !overlay {
		t.Fatal("not an overlay run")
	}
	if !tree.tree.Pending() || tree.tree.Phase() != worktree.PhaseCollectPending {
		t.Fatalf("phase = %q after a failed collection", tree.tree.Phase())
	}
	// The state file on disk says so, and the layers, with the run's work, are there.
	stateFile := filepath.Join(filepath.Dir(layers.Upper), "jad-run.json")
	if data, err := os.ReadFile(stateFile); err != nil || !strings.Contains(string(data), `"phase":"collect-pending"`) {
		t.Fatalf("state file %s: %q %v", stateFile, data, err)
	}
	if data, err := os.ReadFile(filepath.Join(layers.Upper, "left.txt")); err != nil || string(data) != "work\n" {
		t.Fatalf("the run's work is not in the upper layer: %q %v", data, err)
	}
	if err := tree.tree.Remove(context.Background()); err == nil || !strings.Contains(err.Error(), "collection pending") {
		t.Fatalf("Remove of a pending overlay run: %v", err)
	}
	if _, err := os.Stat(layers.Upper); err != nil {
		t.Fatalf("Remove deleted the layers: %v", err)
	}
	reopened, err := worktree.Open(context.Background(), worktreeOptions(settings.Worktree), state)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Pending() {
		t.Fatal("a reopened run does not know its collection is pending")
	}
	// Finish without a launcher keeps the run and lets the lock go (an overlay run is collected
	// only inside its mount); the layers stay, still pending.
	if outcome, err := reopened.Finish(context.Background(), "x", nil); err == nil || !outcome.Kept {
		t.Fatalf("Finish without a launcher: %+v %v", outcome, err)
	}
	got := RecoverDelegatedWorktrees(context.Background(), stateDir, settings, nil)
	if len(got) != 1 || got[0].Error != "" || !got[0].Changed {
		t.Fatalf("recovery = %+v", got)
	}
	if text := gitCommand(t, repo, "show", tree.tree.Branch+":left.txt"); text != "work" {
		t.Fatalf("left.txt on the branch = %q", text)
	}
	for _, gone := range []string{tree.tree.Path, filepath.Dir(layers.Upper), tree.record} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", gone, err)
		}
	}
}
