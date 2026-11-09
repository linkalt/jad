package worktree

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The base cache: one read-only checkout of each start commit that overlay runs mount their
// changes over (docs/isolation-sandbox-design.md §2.3, C1). It is a folder of its own in the
// repository's git directory, <git dir>/jad-worktrees/.base, that no worktree is registered
// for, so neither `git worktree list` nor `git status` shows it; it is built with
// read-tree and checkout-index, which write the files and the index and register nothing.
//
// Each commit has an entry, <commit>/, holding tree/ (the files, an overlay's lower layer),
// index (git's index of them, copied to each run) and refs (the runs holding it, one overlay
// folder per line), and a lock file <commit>.lock. Every change of an entry is made under an
// exclusive flock on its lock file, which also keeps two servers on one repository apart; the
// in-process lockFor does not reach them. An entry is built in <commit>.build-*, given its
// complete marker last and renamed into place, so an entry a reader finds without the marker
// is a leftover that nothing may use and that garbage collection removes.
const (
	baseTreeName     = "tree"
	baseIndexName    = "index"
	baseRefsName     = "refs"
	baseCompleteName = "complete"
	baseBuildInfix   = ".build-"
	baseGCInfix      = ".gc-"
	baseLockSuffix   = ".lock"
)

// baseLockPoll is how often a waiting process tries the lock of an entry again.
const baseLockPoll = 10 * time.Millisecond

// baseFolder is the base cache of the repository. It is always inside the git directory,
// whatever Options.Dir says, so it is never in the session's working directory and the
// sandbox's rule for <git dir>/jad-worktrees hides it from every run (S4).
func (t *Tree) baseFolder() string {
	return filepath.Join(t.common, DefaultDirName, baseDirName)
}

// baseComplete reports whether entry was built to the end.
func baseComplete(entry string) bool {
	info, err := os.Stat(filepath.Join(entry, baseCompleteName))
	return err == nil && info.Mode().IsRegular()
}

// baseLock is the exclusive lock of one entry.
type baseLock struct {
	file *os.File
	path string
}

// lockBase takes the lock of commit's entry in folder, waiting for it until ctx ends. The lock
// file is removed with the entry (baseLock.remove), so after locking the file is checked to
// still be the one at its path; otherwise it is opened again.
func lockBase(ctx context.Context, folder, commit string) (*baseLock, error) {
	path := filepath.Join(folder, commit+baseLockSuffix)
	for {
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open the base checkout's lock: %w", err)
		}
		for {
			locked, err := tryFlock(file)
			if err != nil {
				_ = file.Close()
				return nil, fmt.Errorf("lock the base checkout: %w", err)
			}
			if locked {
				break
			}
			select {
			case <-ctx.Done():
				_ = file.Close()
				return nil, fmt.Errorf("wait for the base checkout's lock: %w", ctx.Err())
			case <-time.After(baseLockPoll):
			}
		}
		held, heldErr := file.Stat()
		current, currentErr := os.Stat(path)
		if heldErr == nil && currentErr == nil && os.SameFile(held, current) {
			return &baseLock{file: file, path: path}, nil
		}
		_ = file.Close()
	}
}

func (l *baseLock) unlock() { _ = l.file.Close() }

// remove deletes the lock file, which the caller holds: a process that opened it meanwhile
// finds it replaced after it gets the lock, and opens the new one (lockBase).
func (l *baseLock) remove() { _ = os.Remove(l.path) }

// acquireBase returns the base checkout of Start (its files and its index), built when
// missing, and records the run as holding it. The run's overlay folder must already exist: it is
// what tells garbage collection, in this process or another, that the run is going.
func (t *Tree) acquireBase(ctx context.Context) (string, string, error) {
	folder := t.baseFolder()
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return "", "", fmt.Errorf("create the base checkout folder: %w", err)
	}
	lock, err := lockBase(ctx, folder, t.Start)
	if err != nil {
		return "", "", err
	}
	defer lock.unlock()
	entry := filepath.Join(folder, t.Start)
	if !baseComplete(entry) {
		// Left over by a build that did not finish (or by an interrupted removal).
		if err := removeBaseLeftovers(folder, t.Start, true); err != nil {
			return "", "", fmt.Errorf("remove an incomplete base checkout: %w", err)
		}
		if err := t.buildBase(ctx, folder, entry); err != nil {
			return "", "", err
		}
	}
	refs := filepath.Join(entry, baseRefsName)
	holders, err := readBaseRefs(refs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("read the holders of the base checkout: %w", err)
	}
	if !slices.Contains(holders, t.overlay.dir) {
		holders = append(holders, t.overlay.dir)
	}
	if err := writeBaseRefs(refs, holders); err != nil {
		return "", "", fmt.Errorf("record the run on the base checkout: %w", err)
	}
	return filepath.Join(entry, baseTreeName), filepath.Join(entry, baseIndexName), nil
}

// buildBase makes the entry of Start: the commit's files and index are written into a folder
// of their own, which gets its complete marker and is then renamed to entry. The caller
// holds the entry's lock and has removed what an earlier build left.
func (t *Tree) buildBase(ctx context.Context, folder, entry string) (err error) {
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return fmt.Errorf("name the base checkout's build folder: %w", err)
	}
	build := filepath.Join(folder, t.Start+baseBuildInfix+hex.EncodeToString(random))
	if err := os.Mkdir(build, 0o755); err != nil {
		return fmt.Errorf("create the base checkout's build folder: %w", err)
	}
	defer func() {
		if err != nil {
			_ = removeAllWritable(build)
		}
	}()
	tree, index := filepath.Join(build, baseTreeName), filepath.Join(build, baseIndexName)
	if err := os.Mkdir(tree, 0o755); err != nil {
		return fmt.Errorf("create the base checkout's folder: %w", err)
	}
	// The repository's git directory, the new index and the new folder are named outright;
	// read-tree and checkout-index touch neither the worktree list nor any ref. Sparse
	// checkout is off: the folder is the whole commit.
	command := func(arguments ...string) gitCommand {
		return gitCommand{dir: t.common, flags: append(slices.Clone(hostGitFlags), "-c", "core.sparseCheckout=false"), args: arguments,
			env: []string{"GIT_DIR=" + t.common, "GIT_INDEX_FILE=" + index, "GIT_WORK_TREE=" + tree}}
	}
	if _, _, err := runGit(ctx, t.options, command("read-tree", t.Start)); err != nil {
		return fmt.Errorf("read the base commit's tree: %w", err)
	}
	if _, _, err := runGit(ctx, t.options, command("checkout-index", "--all", "--force", "-u")); err != nil {
		return fmt.Errorf("check out the base commit: %w", err)
	}
	if err := writeBaseRefs(filepath.Join(build, baseRefsName), nil); err != nil {
		return fmt.Errorf("write the base checkout's holders: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(build, baseCompleteName), []byte(t.Start+"\n")); err != nil {
		return fmt.Errorf("mark the base checkout complete: %w", err)
	}
	if err := os.Rename(build, entry); err != nil {
		return fmt.Errorf("put the base checkout in place: %w", err)
	}
	return nil
}

// releaseBase is what a run's end does for the base cache: garbage collection. The run's own
// overlay folder is already gone, which is what ends its hold on the entry.
func (t *Tree) releaseBase(ctx context.Context) {
	t.sweepBases(ctx)
}

// sweepBases collects the base cache: for each commit it removes what an interrupted build or
// removal left, drops the holders whose overlay folder is gone (a run lost without a record,
// or one that ended), and removes each entry nobody holds that is not the repository's HEAD
// (kept for the next run). Each entry is changed under its own lock, one at a time.
func (t *Tree) sweepBases(ctx context.Context) {
	folder := t.baseFolder()
	entries, err := os.ReadDir(folder)
	if err != nil {
		return
	}
	var commits []string
	for _, entry := range entries {
		commit, _, _ := strings.Cut(entry.Name(), ".")
		if isCommitID(commit) && !slices.Contains(commits, commit) {
			commits = append(commits, commit)
		}
	}
	if len(commits) == 0 {
		return
	}
	head, _ := run(ctx, t.options, t.common, "rev-parse", "--verify", "HEAD^{commit}")
	for _, commit := range commits {
		sweepBase(ctx, folder, commit, commit == head)
	}
}

// sweepBase collects one entry; keep is set for the entry of the repository's HEAD.
func sweepBase(ctx context.Context, folder, commit string, keep bool) {
	lock, err := lockBase(ctx, folder, commit)
	if err != nil {
		return
	}
	defer lock.unlock()
	entry := filepath.Join(folder, commit)
	if !baseComplete(entry) {
		// Nobody builds it: the builder holds the lock.
		if removeBaseLeftovers(folder, commit, true) == nil {
			lock.remove()
		}
		return
	}
	_ = removeBaseLeftovers(folder, commit, false)
	refs := filepath.Join(entry, baseRefsName)
	holders, err := readBaseRefs(refs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	kept := slices.DeleteFunc(slices.Clone(holders), func(holder string) bool {
		_, err := os.Lstat(holder)
		return !filepath.IsAbs(holder) || err != nil
	})
	if len(kept) > 0 || keep {
		if len(kept) != len(holders) {
			_ = writeBaseRefs(refs, kept)
		}
		return
	}
	if removeBaseLeftovers(folder, commit, true) == nil {
		lock.remove()
	}
}

// removeBaseLeftovers removes, from the cache folder, the build and removal leftovers of commit
// and, with entry set, its entry (complete or not). An entry is first renamed away, so it never
// stands half removed under its own name. The caller holds the commit's lock.
func removeBaseLeftovers(folder, commit string, entry bool) error {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return err
	}
	var failed error
	for _, found := range entries {
		name := found.Name()
		switch {
		case strings.HasPrefix(name, commit+baseBuildInfix), strings.HasPrefix(name, commit+baseGCInfix):
		case entry && name == commit:
			gone := filepath.Join(folder, commit+baseGCInfix+"removing")
			_ = removeAllWritable(gone)
			if err := os.Rename(filepath.Join(folder, name), gone); err != nil {
				failed = err
				continue
			}
			name = filepath.Base(gone)
		default:
			continue
		}
		if err := removeAllWritable(filepath.Join(folder, name)); err != nil {
			failed = err
		}
	}
	return failed
}

// readBaseRefs reads a refs file as a list of non-empty holder names, one per line. A missing
// file is an error for its callers (os.ErrNotExist).
func readBaseRefs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var holders []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			holders = append(holders, line)
		}
	}
	return holders, nil
}

func writeBaseRefs(path string, holders []string) error {
	return writeFileAtomic(path, []byte(strings.Join(holders, "\n")+"\n"))
}
