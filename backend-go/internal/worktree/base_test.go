package worktree

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// commitsOf adds commits to repo, each setting a.txt to "v<number>", and returns the ids of
// the commits that exist then, oldest first (the repository's own first commit has "a").
func commitsOf(t *testing.T, repo string, count int) []string {
	t.Helper()
	ids := []string{gitIn(t, repo, "rev-parse", "HEAD")}
	for index := 1; index <= count; index++ {
		write(t, filepath.Join(repo, "a.txt"), fmt.Sprintf("v%d\n", index))
		gitIn(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--quiet", "-am", fmt.Sprintf("v%d", index))
		ids = append(ids, gitIn(t, repo, "rev-parse", "HEAD"))
	}
	return ids
}

func commonOf(t *testing.T, repo string) string {
	t.Helper()
	common, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	return common
}

// F7: the base cache is no worktree of the repository (git worktree list and git status do not
// show it), is in the git directory even when the runs' folder is elsewhere, and is built
// without git registering anything.
func TestBaseCacheIsNotAWorktreeAndStaysInTheGitDirectory(t *testing.T) {
	repo := repository(t)
	options := overlayOptions()
	options.Dir = filepath.Join(t.TempDir(), "runs")
	tree, err := Create(context.Background(), options, repo, "outside", "")
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(commonOf(t, repo), DefaultDirName, baseDirName)
	head := gitIn(t, repo, "rev-parse", "HEAD")
	if layers, _ := tree.Layers(); layers.Lower[0] != filepath.Join(folder, head, baseTreeName) {
		t.Fatalf("lower = %v, want the entry in %s", layers.Lower, folder)
	}
	if _, err := os.Stat(filepath.Join(options.Dir, baseDirName)); err == nil {
		t.Fatal("a base cache was made in the runs' folder")
	}
	list := gitIn(t, repo, "worktree", "list", "--porcelain")
	if strings.Count(list, "worktree ") != 2 || strings.Contains(list, baseDirName) {
		t.Fatalf("worktrees:\n%s", list)
	}
	if status := gitIn(t, repo, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("status:\n%s", status)
	}
	// A run's merged view is the commit: the cache's files are the commit's, with the index.
	if data, err := os.ReadFile(filepath.Join(folder, head, baseTreeName, "sub", "b.txt")); err != nil || string(data) != "b\n" {
		t.Fatalf("the base checkout's sub/b.txt = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(folder, head, baseTreeName, ".git")); err == nil {
		t.Fatal("the base checkout has a .git entry, so git would take it for a repository")
	}
	if err := tree.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Garbage collection: once HEAD moves on, the old entry goes with the next run's end.
	commitsOf(t, repo, 1)
	next, err := Create(context.Background(), options, repo, "next", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(folder, head)); err == nil {
		t.Fatal("the base checkout of the old HEAD was not collected")
	}
}

// F2: an entry without its complete marker, a build folder and a half-removed entry are
// leftovers: a reader never uses them and garbage collection removes them.
func TestBaseCacheLeftoversAreIgnoredAndCollected(t *testing.T) {
	repo := repository(t)
	ids := commitsOf(t, repo, 2)
	folder := filepath.Join(commonOf(t, repo), DefaultDirName, baseDirName)
	old := ids[1]
	write(t, filepath.Join(folder, old, baseTreeName, "partial.txt"), "half\n")
	write(t, filepath.Join(folder, old+baseBuildInfix+"1", baseTreeName, "a.txt"), "building\n")
	write(t, filepath.Join(folder, old+baseGCInfix+"1", baseTreeName, "a.txt"), "removing\n")
	write(t, filepath.Join(folder, ids[0]+baseLockSuffix), "")
	// Not a cache name: left alone.
	write(t, filepath.Join(folder, "notes.txt"), "mine\n")
	if baseComplete(filepath.Join(folder, old)) {
		t.Fatal("an entry without the marker counts as complete")
	}
	tree := &Tree{common: commonOf(t, repo), options: testOptions()}
	tree.sweepBases(context.Background())
	for _, gone := range []string{old, old + baseBuildInfix + "1", old + baseGCInfix + "1", ids[0] + baseLockSuffix} {
		if _, err := os.Lstat(filepath.Join(folder, gone)); err == nil {
			t.Fatalf("%s was not collected", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(folder, "notes.txt")); err != nil {
		t.Fatal("garbage collection removed a file that is not the cache's")
	}
	// A run of the commit whose entry was a leftover gets a complete one.
	run, err := Create(context.Background(), overlayOptions(), repo, "after", old)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Remove(context.Background()) })
	if data, err := os.ReadFile(filepath.Join(run.overlay.base, "a.txt")); err != nil || string(data) != "v1\n" {
		t.Fatalf("a.txt of the base = %q, %v", data, err)
	}
}

// The base of an unheld, non-HEAD commit is removed with its lock; one a run holds is not, even
// from another process (the overlay folder of the run is the proof it goes), and a holder whose
// folder is gone is dropped.
func TestBaseCacheKeepsWhatARunHoldsAndDropsWhatNobodyDoes(t *testing.T) {
	repo := repository(t)
	ids := commitsOf(t, repo, 2)
	folder := filepath.Join(commonOf(t, repo), DefaultDirName, baseDirName)
	held, err := Create(context.Background(), overlayOptions(), repo, "held", ids[1])
	if err != nil {
		t.Fatal(err)
	}
	other, err := Create(context.Background(), overlayOptions(), repo, "other", ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(folder, ids[0])); err == nil {
		t.Fatal("an unheld entry was kept")
	}
	if _, err := os.Lstat(filepath.Join(folder, ids[0]+baseLockSuffix)); err == nil {
		t.Fatal("the lock of a removed entry was kept")
	}
	if !baseComplete(filepath.Join(folder, ids[1])) {
		t.Fatal("the entry a run holds was removed")
	}
	// The run is lost without a trace (its folders removed by hand): the next collection
	// drops the hold and then the entry.
	if err := removeAllWritable(held.overlay.dir); err != nil {
		t.Fatal(err)
	}
	held.unlock()
	third, err := Create(context.Background(), overlayOptions(), repo, "third", ids[2])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = third.Remove(context.Background()) })
	if _, err := os.Lstat(filepath.Join(folder, ids[1])); err == nil {
		t.Fatal("the entry of a lost run was kept")
	}
}

// lockBase waits for the holder, and a lock file removed with its entry while a second process
// waits on it is replaced: the waiter ends up with the lock of the file that is there.
func TestBaseLockWaitsAndSurvivesTheRemovalOfItsFile(t *testing.T) {
	folder := t.TempDir()
	commit := strings.Repeat("a", 40)
	first, err := lockBase(context.Background(), folder, commit)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan *baseLock, 1)
	go func() {
		lock, err := lockBase(context.Background(), folder, commit)
		if err != nil {
			t.Error(err)
		}
		got <- lock
	}()
	select {
	case <-got:
		t.Fatal("the second lock was granted while the first was held")
	case <-time.After(100 * time.Millisecond):
	}
	first.remove()
	first.unlock()
	var second *baseLock
	select {
	case second = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the second lock was never granted")
	}
	if second == nil {
		return
	}
	held, _ := second.file.Stat()
	current, err := os.Stat(second.path)
	if err != nil || !os.SameFile(held, current) {
		t.Fatalf("the lock held is not the file at its path: %v", err)
	}
	second.unlock()
	// A waiter gives up when its context ends.
	third, _ := lockBase(context.Background(), folder, commit)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := lockBase(ctx, folder, commit); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lockBase with an ended context = %v", err)
	}
	third.unlock()
}

// churn creates and removes overlay runs of the given commits, checking after each start that the
// base checkout it mounts is whole and stays so until the run ends. It is what two servers on one
// repository do to the cache.
func churn(repo, id string, commits []string, rounds int) error {
	ctx := context.Background()
	for round := range rounds {
		commit := commits[(round+len(id))%len(commits)]
		want, err := run(ctx, testOptions(), repo, "show", commit+":a.txt")
		if err != nil {
			return err
		}
		tree, err := Create(ctx, overlayOptions(), repo, fmt.Sprintf("p%s-%d", id, round), commit)
		if err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
		layers, _ := tree.Layers()
		for range 3 {
			data, err := os.ReadFile(filepath.Join(layers.Lower[0], "a.txt"))
			if err != nil || strings.TrimRight(string(data), "\n") != want {
				return fmt.Errorf("round %d: the run's base checkout holds %q (%v), want %q", round, data, err, want)
			}
			if index, err := os.Stat(filepath.Join(filepath.Dir(layers.Lower[0]), baseIndexName)); err != nil || index.Size() == 0 {
				return fmt.Errorf("round %d: the run's base checkout has no index: %v", round, err)
			}
			time.Sleep(time.Millisecond)
		}
		if err := tree.Remove(ctx); err != nil {
			return fmt.Errorf("round %d: remove: %w", round, err)
		}
	}
	return nil
}

// TestBaseCacheChild is the other server of the two-process tests: the test binary run again
// with JAD_WORKTREE_CHILD naming what to do. Without it the test does nothing.
func TestBaseCacheChild(t *testing.T) {
	mode := os.Getenv("JAD_WORKTREE_CHILD")
	if mode == "" {
		t.Skip("run by the two-process tests")
	}
	switch mode {
	case "hold":
		lock, err := lockBase(context.Background(), os.Getenv("JAD_WORKTREE_FOLDER"), os.Getenv("JAD_WORKTREE_COMMIT"))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("locked")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		lock.unlock()
	case "churn":
		rounds, _ := strconv.Atoi(os.Getenv("JAD_WORKTREE_ROUNDS"))
		if err := churn(os.Getenv("JAD_WORKTREE_REPO"), os.Getenv("JAD_WORKTREE_ID"), strings.Split(os.Getenv("JAD_WORKTREE_COMMITS"), ","), rounds); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown mode %q", mode)
	}
}

func startChild(t *testing.T, env ...string) *exec.Cmd {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestBaseCacheChild$", "-test.count=1")
	child.Env = append(os.Environ(), env...)
	return child
}

// F2: a second process holding an entry's lock keeps a start of that commit waiting until it lets
// go, and the start then finds a whole entry.
func TestBaseCacheLockHoldsOffAStartInAnotherProcess(t *testing.T) {
	repo := repository(t)
	ids := commitsOf(t, repo, 1)
	folder := filepath.Join(commonOf(t, repo), DefaultDirName, baseDirName)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	child := startChild(t, "JAD_WORKTREE_CHILD=hold", "JAD_WORKTREE_FOLDER="+folder, "JAD_WORKTREE_COMMIT="+ids[0])
	stdin, _ := child.StdinPipe()
	stdout, _ := child.StdoutPipe()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = child.Wait() })
	locked := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "locked" {
				locked <- true
				return
			}
		}
		locked <- false
	}()
	select {
	case ok := <-locked:
		if !ok {
			t.Fatal("the other process did not take the lock")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the other process never took the lock")
	}
	started := make(chan error, 1)
	var tree *Tree
	go func() {
		var err error
		tree, err = Create(context.Background(), overlayOptions(), repo, "waits", ids[0])
		started <- err
	}()
	select {
	case err := <-started:
		t.Fatalf("a start went on while another process held the entry: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	_ = stdin.Close()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the start never went on")
	}
	if !baseComplete(filepath.Dir(tree.overlay.base)) {
		t.Fatal("the start found no whole entry")
	}
	_ = tree.Remove(context.Background())
}

// F2: this process and two others start and end overlay runs of three commits on one repository
// at once, so each entry is built, collected and built again while the others use it; every run
// finds its base checkout whole for as long as it holds it, and nothing is left over. Run with
// -race.
func TestBaseCacheSurvivesServersChurningTogether(t *testing.T) {
	repo := repository(t)
	ids := commitsOf(t, repo, 3)
	// ids[3] is HEAD, which is kept; the three older ones are built and collected over and over.
	commits := ids[:3]
	const rounds = 12
	var children []*exec.Cmd
	var outputs []*strings.Builder
	for _, id := range []string{"a", "bb"} {
		child := startChild(t, "JAD_WORKTREE_CHILD=churn", "JAD_WORKTREE_REPO="+repo, "JAD_WORKTREE_ID="+id, "JAD_WORKTREE_COMMITS="+strings.Join(commits, ","), "JAD_WORKTREE_ROUNDS="+strconv.Itoa(rounds))
		output := &strings.Builder{}
		child.Stdout, child.Stderr = output, output
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		children, outputs = append(children, child), append(outputs, output)
	}
	var wait sync.WaitGroup
	own := make([]error, 2)
	for index, id := range []string{"x", "yyy"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			own[index] = churn(repo, id, commits, rounds)
		}()
	}
	wait.Wait()
	for index, err := range own {
		if err != nil {
			t.Errorf("this process, goroutine %d: %v", index, err)
		}
	}
	for index, child := range children {
		if err := child.Wait(); err != nil {
			t.Errorf("child %d: %v\n%s", index, err, outputs[index])
		}
	}
	folder := filepath.Join(commonOf(t, repo), DefaultDirName, baseDirName)
	(&Tree{common: commonOf(t, repo), options: testOptions()}).sweepBases(context.Background())
	entries, _ := os.ReadDir(folder)
	for _, entry := range entries {
		// What is left is the HEAD entry and its lock.
		if !strings.HasPrefix(entry.Name(), ids[3]) {
			t.Errorf("%s is left in the base cache", entry.Name())
		}
	}
	if list := gitIn(t, repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Errorf("worktrees left:\n%s", list)
	}
}
