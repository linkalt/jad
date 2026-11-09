package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Materialisers (Options.Materialize): how a run's worktree gets its files. The caller
// resolves sandbox.isolation.materialize "auto" to one of them, since only it can try the
// overlay on this host (docs/isolation-decisions.md D1, D14).
const (
	// MaterializeCheckout writes a full checkout of the start commit (also when empty).
	MaterializeCheckout = "checkout"
	// MaterializeOverlay makes the worktree an overlay of the run's changes over a shared,
	// read-only checkout of the start commit (the base cache). The merged files exist only
	// inside the mount namespace of the run's holder; on the host the worktree folder holds
	// the .git file alone, so every process of the run, and Finish's launcher, must work
	// inside that mount (docs/isolation-sandbox-design.md §2.3).
	MaterializeOverlay = "overlay"
)

// overlayDirName is the folder beside the runs' worktrees that holds the overlay runs' layers;
// baseDirName is the base cache's folder inside <git dir>/jad-worktrees (base.go). Their names
// start with a dot, which no run name does; inside the repository's git directory every run's
// sandbox hides them (S4).
const (
	overlayDirName = ".overlay"
	baseDirName    = ".base"
	// overlayStateName is the file an overlay run's layers are recorded in, so that Open can
	// mount them again after a restart.
	overlayStateName = "jad-run.json"
	overlayLockName  = "lock"
)

// ErrHeld is returned by Open for an overlay run whose layers a running process still holds (its
// holder may mount them): not a failure of the run, which is tried again later.
var ErrHeld = errors.New("the run's overlay is still held by a running process")

// Layers are an overlay run's mount: Lower (top first) read-only under Upper, whose work
// folder is Work, mounted at Target, the worktree's folder.
type Layers struct {
	Lower  []string
	Upper  string
	Work   string
	Target string
}

// overlay is what an overlay run has besides a checkout's folders.
type overlay struct {
	// dir holds upper, work, the lock and the state file.
	dir string
	// base is the base checkout's folder, the run's lower (<entry>/tree of the base cache),
	// start the commit it holds.
	base  string
	start string
	// lock is held (flock) while a mount of the layers may exist, so that a second mount of
	// the same upper, which the kernel leaves undefined, cannot happen (C2).
	lock *os.File
	// state is what the run's state file holds; mu guards it and the file, which the run's
	// holder, its collection and its recovery write from different goroutines.
	mu    sync.Mutex
	state overlayState
}

// Phases of an overlay run (overlayState.Phase): running from its start; collect-pending from
// the moment Finish starts to collect it until its folders are removed, so a collection that
// failed, or that a crash cut off, is found again and the layers, which hold the run's work, are
// never deleted meanwhile (docs/isolation-sandbox-design.md §10).
const (
	PhaseRunning        = "running"
	PhaseCollectPending = "collect-pending"
)

// overlayState is the content of overlayStateName.
type overlayState struct {
	Version int      `json:"version"`
	Mode    string   `json:"mode"`
	Start   string   `json:"start"`
	Lower   []string `json:"lower"`
	Phase   string   `json:"phase,omitempty"`
	// Holder is the identity of the run's holder while it runs, so that a later server can tell
	// a holder that survived from a process that reused its pid.
	Holder *HolderRef `json:"holder,omitempty"`
}

// HolderRef names a process by its pid and its start time (the starttime field of
// /proc/<pid>/stat, in clock ticks since boot): a pid alone can be reused.
type HolderRef struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
}

// Layers returns the overlay mount of an overlay run; false for a checkout.
func (t *Tree) Layers() (Layers, bool) {
	if t.overlay == nil {
		return Layers{}, false
	}
	return Layers{
		Lower:  []string{t.overlay.base},
		Upper:  filepath.Join(t.overlay.dir, "upper"),
		Work:   filepath.Join(t.overlay.dir, "work"),
		Target: t.Path,
	}, true
}

// OnCollected sets what Finish calls once it no longer starts anything inside the run's
// mount (after the collection and the pack), before it removes the folders, and also when it
// fails: the caller stops the mount of an overlay run there.
func (t *Tree) OnCollected(stop func()) { t.onCollected = stop }

// collected calls the OnCollected function once.
func (t *Tree) collected() {
	if stop := t.onCollected; stop != nil {
		t.onCollected = nil
		stop()
	}
}

// Remove deletes a tree that is not to be collected (one whose run never started): its
// folders, and for an overlay run its layers and its hold on the base checkout.
func (t *Tree) Remove(ctx context.Context) error {
	if t.Pending() {
		// The layers hold work whose collection did not finish: they are never deleted.
		return fmt.Errorf("the overlay run %s has a collection pending, so its layers are kept at %s", t.Branch, t.overlay.dir)
	}
	defer t.unlock()
	return t.remove(ctx, false)
}

// Phase is the phase of an overlay run's state file; empty for a checkout.
func (t *Tree) Phase() string {
	if t.overlay == nil {
		return ""
	}
	t.overlay.mu.Lock()
	defer t.overlay.mu.Unlock()
	return t.overlay.state.Phase
}

// Pending reports whether an overlay run's collection started and did not end: the run's work
// is in its layers, and Finish collects it again.
func (t *Tree) Pending() bool { return t.Phase() == PhaseCollectPending }

// Holder is the identity recorded for the holder of an overlay run, zero when none is.
func (t *Tree) Holder() HolderRef {
	if t.overlay == nil {
		return HolderRef{}
	}
	t.overlay.mu.Lock()
	defer t.overlay.mu.Unlock()
	if t.overlay.state.Holder == nil {
		return HolderRef{}
	}
	return *t.overlay.state.Holder
}

// SetHolder records the holder of an overlay run (a zero ref clears it) in its state file.
func (t *Tree) SetHolder(ref HolderRef) error {
	if t.overlay == nil {
		return nil
	}
	t.overlay.mu.Lock()
	defer t.overlay.mu.Unlock()
	if ref == (HolderRef{}) {
		t.overlay.state.Holder = nil
	} else {
		t.overlay.state.Holder = &ref
	}
	if err := t.saveOverlayState(); err != nil && !errors.Is(err, os.ErrNotExist) {
		// A run whose folders are already removed has nothing left to record.
		return err
	}
	return nil
}

// saveOverlayState writes the state file of an overlay run; the caller holds overlay.mu.
func (t *Tree) saveOverlayState() error {
	data, err := json.Marshal(t.overlay.state)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(t.overlay.dir, overlayStateName), data); err != nil {
		return fmt.Errorf("record the run's overlay: %w", err)
	}
	return nil
}

// markCollectPending persists that the collection of the run started.
func (t *Tree) markCollectPending() error {
	if t.overlay == nil {
		return nil
	}
	t.overlay.mu.Lock()
	defer t.overlay.mu.Unlock()
	if t.overlay.state.Phase == PhaseCollectPending {
		return nil
	}
	t.overlay.state.Phase = PhaseCollectPending
	return t.saveOverlayState()
}

// overlayFolder is where the layers of the run whose worktree is path live.
func overlayFolder(path string) string {
	return filepath.Join(filepath.Dir(path), overlayDirName, filepath.Base(path))
}

// addOverlay makes an overlay run: the base checkout of Start (made once and shared), a
// worktree registered with git but not checked out, whose index is the base's (so git status
// in the merged view starts clean, C5), and the run's empty upper and work folders. relative
// is the session's working directory inside the repository.
func (t *Tree) addOverlay(ctx context.Context, relative string) (err error) {
	lock := lockFor(t.common)
	lock.Lock()
	defer lock.Unlock()
	if BranchExists(ctx, t.options, t.top, t.Branch) {
		return fmt.Errorf("the branch %s already exists", t.Branch)
	}
	// The run's overlay folder is made first and locked: it is what the base cache's garbage
	// collection, in any server, counts as the run being alive (acquireBase).
	dir := overlayFolder(t.Path)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("create the overlay folder: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create the run's overlay folder: %w", err)
	}
	t.overlay = &overlay{dir: dir, start: t.Start}
	added := false
	defer func() {
		if err != nil {
			t.unlock()
			if added {
				_ = t.removeFolders()
				_, _ = run(ctx, t.options, t.top, "worktree", "prune")
			} else {
				_ = removeAllWritable(dir)
			}
			t.releaseBase(ctx)
			t.overlay = nil
		}
	}()
	if t.overlay.lock, err = lockOverlay(dir); err != nil {
		return err
	}
	base, baseIndex, err := t.acquireBase(ctx)
	if err != nil {
		return err
	}
	t.overlay.base = base
	if _, err := run(ctx, t.options, t.top, "worktree", "add", "--quiet", "--no-checkout", "--detach", t.Path, t.Start); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	added = true
	if t.Admin, err = t.adminOf(ctx, t.Path); err != nil {
		return fmt.Errorf("find the worktree's admin folder: %w", err)
	}
	t.Objects = filepath.Join(t.Admin, ObjectsDirName)
	if err := os.Mkdir(t.Objects, 0o755); err != nil {
		return fmt.Errorf("create the run's object folder: %w", err)
	}
	if err := copyFile(baseIndex, filepath.Join(t.Admin, "index")); err != nil {
		return fmt.Errorf("copy the base checkout's index: %w", err)
	}
	upper, work := filepath.Join(dir, "upper"), filepath.Join(dir, "work")
	for _, folder := range []string{upper, work} {
		if err := os.MkdirAll(folder, 0o700); err != nil {
			return fmt.Errorf("create the run's overlay folders: %w", err)
		}
	}
	// The base's files carry no .git file; the run's own is in its upper layer.
	if err := copyFile(filepath.Join(t.Path, ".git"), filepath.Join(upper, ".git")); err != nil {
		return fmt.Errorf("point the run's view at its admin folder: %w", err)
	}
	if relative != "." {
		if _, statErr := os.Stat(filepath.Join(base, relative)); statErr != nil {
			// The session's folder is not in the commit: the run gets it empty.
			if err := os.MkdirAll(filepath.Join(upper, relative), 0o755); err != nil {
				return fmt.Errorf("create the working directory in the worktree: %w", err)
			}
		}
		// On the host the folder only stands under the mount, so that what is resolved
		// there (the sandbox's writable roots) finds the run's working directory.
		if err := os.MkdirAll(t.Workspace, 0o755); err != nil {
			return fmt.Errorf("create the working directory in the worktree: %w", err)
		}
	}
	t.overlay.state = overlayState{Version: 1, Mode: MaterializeOverlay, Start: t.Start, Lower: []string{base}, Phase: PhaseRunning}
	if err := t.saveOverlayState(); err != nil {
		return err
	}
	// Garbage collection of the other entries, now that this run holds its own.
	t.sweepBases(ctx)
	return nil
}

// openOverlay finds the layers of an overlay run Open rebuilds and takes their lock; a tree
// without them is a checkout.
func (t *Tree) openOverlay() error {
	dir := overlayFolder(t.Path)
	data, err := os.ReadFile(filepath.Join(dir, overlayStateName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the run's overlay record: %w", err)
	}
	var state overlayState
	if err := json.Unmarshal(data, &state); err != nil || state.Mode != MaterializeOverlay || len(state.Lower) != 1 || !isCommitID(state.Start) {
		return fmt.Errorf("the run's overlay record %s is not readable", filepath.Join(dir, overlayStateName))
	}
	// The lower layer is a complete entry of this repository's base cache: nothing else is
	// ever mounted, or released, from a record.
	lower := state.Lower[0]
	entry := filepath.Dir(lower)
	if filepath.Base(lower) != baseTreeName || filepath.Dir(entry) != t.baseFolder() || !isCommitID(filepath.Base(entry)) || !baseComplete(entry) {
		return fmt.Errorf("the run's overlay record %s names a base checkout that is not in %s or is incomplete", filepath.Join(dir, overlayStateName), t.baseFolder())
	}
	lock, err := lockOverlay(dir)
	if err != nil {
		return err
	}
	t.overlay = &overlay{dir: dir, base: lower, start: state.Start, lock: lock, state: state}
	return nil
}

// unlock lets go of an overlay run's lock.
func (t *Tree) unlock() {
	if t.overlay != nil && t.overlay.lock != nil {
		_ = t.overlay.lock.Close()
		t.overlay.lock = nil
	}
}

// removeOverlay deletes the run's layers. The work folder's own work folder is made
// unreadable by the kernel, so the folders are made writable when a plain removal fails.
func (t *Tree) removeOverlay() error {
	if t.overlay == nil {
		return nil
	}
	if err := removeAllWritable(t.overlay.dir); err != nil {
		return fmt.Errorf("remove the run's overlay folders: %w", err)
	}
	return nil
}

func removeAllWritable(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(name string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(name, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// adminOf is the admin folder git made for the worktree at path, which must be inside the
// repository's worktrees folder.
func (t *Tree) adminOf(ctx context.Context, path string) (string, error) {
	admin, err := run(ctx, t.options, path, "rev-parse", "--absolute-git-dir")
	if err == nil {
		admin, err = filepath.EvalSymlinks(admin)
	}
	if err == nil && filepath.Dir(admin) != filepath.Join(t.common, "worktrees") {
		err = fmt.Errorf("git placed it at %s, outside %s", admin, filepath.Join(t.common, "worktrees"))
	}
	return admin, err
}

func copyFile(from, to string) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return err
	}
	return target.Close()
}

// writeFileAtomic writes data to path through a temporary file and a rename.
func writeFileAtomic(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporary.Name())
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporary.Name())
		return err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		_ = os.Remove(temporary.Name())
		return err
	}
	return nil
}
