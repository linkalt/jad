package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func overlayOptions() Options {
	options := testOptions()
	options.Materialize = MaterializeOverlay
	return options
}

// The overlay materialiser on the host side (no mount is needed for these): every run of one
// commit holds the same base checkout, gets the base's index (so git status starts clean in
// the merged view) and its own .git file in its upper layer; on the host its folder holds only
// the .git file. The base cache is refcounted under the repository's lock, also with 8 runs
// starting and ending together (run with -race).
func TestOverlayRunsShareOneBaseCheckout(t *testing.T) {
	repo := repository(t)
	head := gitIn(t, repo, "rev-parse", "HEAD")
	const runs = 8
	trees := make([]*Tree, runs)
	var wait sync.WaitGroup
	errs := make([]error, runs)
	for index := range runs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			trees[index], errs[index] = Create(context.Background(), overlayOptions(), repo, fmt.Sprintf("run-%d", index), "")
		}()
	}
	wait.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", index, err)
		}
	}
	entry := filepath.Join(trees[0].common, DefaultDirName, baseDirName, head)
	base := filepath.Join(entry, baseTreeName)
	holders, err := readBaseRefs(filepath.Join(entry, baseRefsName))
	if err != nil || len(holders) != runs {
		t.Fatalf("holders of the base = %v, %v", holders, err)
	}
	baseIndex, err := os.ReadFile(filepath.Join(entry, baseIndexName))
	if err != nil {
		t.Fatal(err)
	}
	for _, tree := range trees {
		layers, overlay := tree.Layers()
		if !overlay || !slices.Equal(layers.Lower, []string{base}) || layers.Target != tree.Path {
			t.Fatalf("layers = %+v", layers)
		}
		if entries, _ := os.ReadDir(tree.Path); len(entries) != 1 || entries[0].Name() != ".git" {
			t.Fatalf("%s holds %v on the host", tree.Path, entries)
		}
		dotGit, _ := os.ReadFile(filepath.Join(tree.Path, ".git"))
		if upper, _ := os.ReadFile(filepath.Join(layers.Upper, ".git")); !bytes.Equal(upper, dotGit) || !strings.Contains(string(dotGit), tree.Admin) {
			t.Fatalf("the upper's .git %q, the worktree's %q", upper, dotGit)
		}
		if index, _ := os.ReadFile(filepath.Join(tree.Admin, "index")); !bytes.Equal(index, baseIndex) {
			t.Fatal("the run's index is not the base's")
		}
		if info, err := os.Stat(tree.Objects); err != nil || !info.IsDir() {
			t.Fatalf("objects folder: %v", err)
		}
	}
	for _, tree := range trees {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := tree.Remove(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	// HEAD's checkout stays for the next run, held by nobody; every run's folders are gone.
	if holders, err := readBaseRefs(filepath.Join(entry, baseRefsName)); err != nil || len(holders) != 0 {
		t.Fatalf("holders after the runs = %v, %v", holders, err)
	}
	if _, err := os.Stat(filepath.Join(base, "a.txt")); err != nil {
		t.Fatalf("the base checkout of HEAD is gone: %v", err)
	}
	for _, tree := range trees {
		for _, path := range []string{tree.Path, tree.Admin, overlayFolder(tree.Path)} {
			if _, err := os.Lstat(path); err == nil {
				t.Fatalf("%s is left", path)
			}
		}
	}
	// Once HEAD moves on, the next release removes the old checkout.
	write(t, filepath.Join(repo, "a.txt"), "next\n")
	gitIn(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--quiet", "-am", "next")
	if list := gitIn(t, repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("worktrees with the base checkout of HEAD held by nobody:\n%s", list)
	}
	next, err := Create(context.Background(), overlayOptions(), repo, "next", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(entry); err == nil {
		t.Fatal("the base checkout of the old HEAD is left")
	}
	if _, err := os.Lstat(entry + baseLockSuffix); err == nil {
		t.Fatal("the lock of the old HEAD's base checkout is left")
	}
	// F7: the cache is no worktree of the repository, and the repository's status is clean.
	if list := gitIn(t, repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("worktrees:\n%s", list)
	}
	if status := gitIn(t, repo, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("status:\n%s", status)
	}
}

// An overlay run is collected only inside its mount: without a launcher Finish keeps it and
// lets its lock go, after which Open finds the same layers; while the lock is held Open refuses,
// so no second mount of the layers can start (C2).
func TestOverlayRunIsHeldUntilFinished(t *testing.T) {
	repo := repository(t)
	tree, err := Create(context.Background(), overlayOptions(), repo, "held", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), testOptions(), tree.State()); err == nil || !strings.Contains(err.Error(), "still held") {
		t.Fatalf("Open of a held run = %v", err)
	}
	outcome, err := tree.Finish(context.Background(), "m", nil)
	if err == nil || !outcome.Kept {
		t.Fatalf("Finish without the mount = %+v, %v", outcome, err)
	}
	if !tree.Pending() {
		t.Fatal("a collection that could not start is not marked pending")
	}
	if _, err := os.Stat(filepath.Join(tree.Path, ".git")); err != nil {
		t.Fatalf("the kept run lost its folder: %v", err)
	}
	reopened, err := Open(context.Background(), testOptions(), tree.State())
	if err != nil {
		t.Fatal(err)
	}
	layers, _ := tree.Layers()
	if again, overlay := reopened.Layers(); !overlay || !slices.Equal(again.Lower, layers.Lower) || again.Upper != layers.Upper {
		t.Fatalf("reopened layers %+v, want %+v", again, layers)
	}
}

// The session's folder inside the repository: a folder of the commit is made on the host too
// (under the mount), so what resolves the run's workspace there finds it; a folder the commit
// lacks starts empty in the run's upper layer.
func TestOverlayRunKeepsTheSessionsFolder(t *testing.T) {
	repo := repository(t)
	tree, err := Create(context.Background(), overlayOptions(), filepath.Join(repo, "sub"), "in-sub", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tree.Remove(context.Background()) })
	if info, err := os.Stat(tree.Workspace); err != nil || !info.IsDir() || tree.Workspace != filepath.Join(tree.Path, "sub") {
		t.Fatalf("workspace %s: %v", tree.Workspace, err)
	}
	layers, _ := tree.Layers()
	if _, err := os.Stat(filepath.Join(layers.Upper, "sub")); err == nil {
		t.Fatal("a folder of the commit was made in the upper layer")
	}
	if err := os.MkdirAll(filepath.Join(repo, "fresh"), 0o755); err != nil {
		t.Fatal(err)
	}
	fresh, err := Create(context.Background(), overlayOptions(), filepath.Join(repo, "fresh"), "in-fresh", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Remove(context.Background()) })
	freshLayers, _ := fresh.Layers()
	if info, err := os.Stat(filepath.Join(freshLayers.Upper, "fresh")); err != nil || !info.IsDir() {
		t.Fatalf("the new folder is not in the upper layer: %v", err)
	}
}

// A base checkout an interrupted start left without its holders file is made again.
func TestOverlayBaseLeftIncompleteIsMadeAgain(t *testing.T) {
	repo := repository(t)
	head := gitIn(t, repo, "rev-parse", "HEAD")
	folder := filepath.Join(repo, ".git", DefaultDirName, baseDirName)
	entry := filepath.Join(folder, head)
	// An entry without its complete marker, and a build folder, as a killed server leaves them.
	write(t, filepath.Join(entry, baseTreeName, "partial.txt"), "half\n")
	write(t, filepath.Join(folder, head+baseBuildInfix+"dead", baseTreeName, "half.txt"), "half\n")
	tree, err := Create(context.Background(), overlayOptions(), repo, "after-crash", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tree.Remove(context.Background()) })
	if _, err := os.Stat(filepath.Join(entry, baseTreeName, "partial.txt")); err == nil {
		t.Fatal("the incomplete base checkout was used")
	}
	if _, err := os.Stat(filepath.Join(entry, baseTreeName, "sub", "b.txt")); err != nil {
		t.Fatalf("the base checkout was not made again: %v", err)
	}
	if !baseComplete(entry) {
		t.Fatal("the base checkout has no complete marker")
	}
	if _, err := os.Stat(filepath.Join(folder, head+baseBuildInfix+"dead")); err == nil {
		t.Fatal("a build folder of a killed start is left")
	}
}

// hostLauncher starts git on the host, with the run's environment only where the worktree package
// adds it: enough to collect a run whose "mount" is the folder as it stands on the host.
func hostLauncher(git string, args []string, _ string) (Launch, error) {
	return Launch{File: git, Argv0: git, Args: args, Env: os.Environ()}, nil
}

// F3: a collection that fails leaves the run's layers marked collect-pending in its state file,
// kept, and refused by Remove; the run is opened again and collected by a later try, which is
// the only thing that deletes them.
func TestOverlayCollectionFailureIsPersistedAndRetried(t *testing.T) {
	repo := repository(t)
	tree, err := Create(context.Background(), overlayOptions(), repo, "pending", "")
	if err != nil {
		t.Fatal(err)
	}
	layers, _ := tree.Layers()
	stateFile := filepath.Join(filepath.Dir(layers.Upper), overlayStateName)
	if tree.Phase() != PhaseRunning {
		t.Fatalf("a new run's phase = %q", tree.Phase())
	}
	failing := func(string, []string, string) (Launch, error) { return Launch{}, errors.New("no holder to collect in") }
	outcome, err := tree.Finish(context.Background(), "m", failing)
	if err == nil || !outcome.Kept || !tree.Pending() {
		t.Fatalf("Finish with a failing launcher: %+v %v, phase %q", outcome, err, tree.Phase())
	}
	if data, _ := os.ReadFile(stateFile); !strings.Contains(string(data), `"phase":"collect-pending"`) {
		t.Fatalf("the state file does not say the collection is pending: %s", data)
	}
	if err := tree.Remove(context.Background()); err == nil || !strings.Contains(err.Error(), "collection pending") {
		t.Fatalf("Remove of a pending run: %v", err)
	}
	for _, kept := range []string{layers.Upper, layers.Work, stateFile, tree.Path, tree.Admin} {
		if _, err := os.Lstat(kept); err != nil {
			t.Fatalf("%s was deleted after a failed collection: %v", kept, err)
		}
	}
	// A later try (another server, after a restart) finds it pending and collects it.
	reopened, err := Open(context.Background(), testOptions(), tree.State())
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Pending() {
		t.Fatal("the reopened run does not know its collection is pending")
	}
	outcome, err = reopened.Finish(context.Background(), "m", hostLauncher)
	if err != nil || outcome.Kept {
		t.Fatalf("the retry: %+v %v", outcome, err)
	}
	for _, gone := range []string{layers.Upper, stateFile, tree.Path, tree.Admin} {
		if _, err := os.Lstat(gone); err == nil {
			t.Fatalf("%s is left after the collection worked", gone)
		}
	}
}

// The holder recorded for a run is kept in its state file and cleared, and a run with its
// folders gone has nothing to record.
func TestOverlayStateRecordsTheHolder(t *testing.T) {
	repo := repository(t)
	tree, err := Create(context.Background(), overlayOptions(), repo, "holder", "")
	if err != nil {
		t.Fatal(err)
	}
	if tree.Holder() != (HolderRef{}) {
		t.Fatalf("a new run has holder %+v", tree.Holder())
	}
	if err := tree.SetHolder(HolderRef{PID: 4242, Start: 99}); err != nil {
		t.Fatal(err)
	}
	layers, _ := tree.Layers()
	stateFile := filepath.Join(filepath.Dir(layers.Upper), overlayStateName)
	if data, _ := os.ReadFile(stateFile); !strings.Contains(string(data), `"pid":4242`) || !strings.Contains(string(data), `"start":99`) {
		t.Fatalf("state file: %s", data)
	}
	if err := tree.SetHolder(HolderRef{}); err != nil || tree.Holder() != (HolderRef{}) {
		t.Fatalf("clear: %v %+v", err, tree.Holder())
	}
	if data, _ := os.ReadFile(stateFile); strings.Contains(string(data), "holder") {
		t.Fatalf("state file after clearing: %s", data)
	}
	if err := tree.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tree.SetHolder(HolderRef{PID: 1, Start: 1}); err != nil {
		t.Fatalf("SetHolder of a removed run: %v", err)
	}
}
