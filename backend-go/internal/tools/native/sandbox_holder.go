package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/worktree"
)

// The overlay materialiser's holder (docs/isolation-sandbox-design.md §2.3, §9.5, D1, D14).
//
// An overlay run's files exist only in the mount namespace of its holder: a bwrap that shares
// the host's whole file system (so the holder confines nothing by itself), mounts the run's
// overlay on its worktree folder and the run's share_paths read-only inside it, and then waits
// for jad to close its input. Every child process of the run joins that namespace with
// nsenter, the user, mount and pid namespaces of the holder; a sandboxed call then starts its
// own bwrap in there exactly as it does for a checkout, so both materialisers enforce the same
// rules and differ only in cost. In-process readers see the run's files through
// /proc/<holder>/root. The holder has a pid namespace of its own, so when it stops every
// process the run left in there is killed with it and the mount goes with the namespace; the
// worktree package's lock keeps a second mount of the same layers from starting meanwhile.

// sandboxHolder is a running holder.
type sandboxHolder struct {
	// pid is the holder's first process (the namespace's init), whose namespaces and root a
	// joining process and the in-process readers use.
	pid     int
	command *exec.Cmd
	input   *os.File
	timeout time.Duration
	once    sync.Once
	// identity is pid with its start time and pidfd (sandbox_holder_identity.go): what a join,
	// a kill and a read through the root check first. mu keeps stop from closing the pidfd under
	// a check.
	mu       sync.RWMutex
	identity processIdentity
}

// sandboxShare is one share_paths folder: source in the session, bound read-only at target
// in the run's worktree.
type sandboxShare struct {
	source string
	target string
}

// holderProgram is what the holder runs: a program that waits until its input closes.
var holderProgram = "cat"

// holderJoinProgram joins a holder's namespaces (D14).
var holderJoinProgram = "nsenter"

// holderArguments is the bwrap command line of a holder for layers and shares.
func holderArguments(layers worktree.Layers, shares []sandboxShare, program string) []string {
	arguments := []string{"--die-with-parent", "--unshare-user", "--unshare-pid", "--dev-bind", "/", "/", "--proc", "/proc"}
	for _, lower := range layers.Lower {
		arguments = append(arguments, "--overlay-src", lower)
	}
	arguments = append(arguments, "--overlay", layers.Upper, layers.Work, layers.Target)
	for _, share := range shares {
		arguments = append(arguments, "--ro-bind", share.source, share.target)
	}
	// The holder works from the root: a process that joins it gets its working directory
	// from there (nsenter --wdns), and the folder jad was started in may be gone.
	return append(arguments, "--chdir", "/", "--info-fd", "3", "--", program)
}

// startHolder starts a holder of layers with bwrap (the executable) and waits, at most
// timeout, until its program runs: bwrap reports the namespace's first process before it has
// mounted anything, so the holder's program (cat) echoes a line jad writes, which it can
// only do once every mount is in place.
func startHolder(ctx context.Context, bwrap string, layers worktree.Layers, shares []sandboxShare, timeout time.Duration) (*sandboxHolder, error) {
	executable, err := exec.LookPath(bwrap)
	if err != nil {
		return nil, fmt.Errorf("the holder needs %s: %w", bwrap, err)
	}
	program, err := exec.LookPath(holderProgram)
	if err != nil {
		return nil, fmt.Errorf("the holder needs %s: %w", holderProgram, err)
	}
	var opened []*os.File
	closeAll := func() {
		for _, file := range opened {
			_ = file.Close()
		}
	}
	pipe := func() (*os.File, *os.File, error) {
		reader, writer, err := os.Pipe()
		if err == nil {
			opened = append(opened, reader, writer)
		}
		return reader, writer, err
	}
	infoReader, infoWriter, err := pipe()
	if err != nil {
		closeAll()
		return nil, err
	}
	inputReader, inputWriter, err := pipe()
	if err != nil {
		closeAll()
		return nil, err
	}
	outputReader, outputWriter, err := pipe()
	if err != nil {
		closeAll()
		return nil, err
	}
	defer infoReader.Close()
	defer outputReader.Close()
	command := exec.Command(executable, holderArguments(layers, shares, program)...)
	command.Dir = string(filepath.Separator)
	command.Stdin = inputReader
	command.Stdout = outputWriter
	var stderr strings.Builder
	command.Stderr = &limitedWriter{builder: &stderr, limit: 4096}
	command.ExtraFiles = []*os.File{infoWriter}
	err = command.Start()
	_ = infoWriter.Close()
	_ = inputReader.Close()
	_ = outputWriter.Close()
	if err != nil {
		_ = inputWriter.Close()
		return nil, fmt.Errorf("start the holder: %w", err)
	}
	holder := &sandboxHolder{command: command, input: inputWriter, timeout: timeout, identity: processIdentity{pidfd: -1}}
	const readyLine = "ready\n"
	ready := make(chan int, 1)
	go func() {
		var reported struct {
			ChildPID int `json:"child-pid"`
		}
		if json.NewDecoder(infoReader).Decode(&reported) != nil || reported.ChildPID <= 0 {
			ready <- 0
			return
		}
		echoed := make([]byte, len(readyLine))
		if _, err := io.ReadFull(outputReader, echoed); err != nil || string(echoed) != readyLine {
			ready <- 0
			return
		}
		ready <- reported.ChildPID
	}()
	if _, err := io.WriteString(inputWriter, readyLine); err != nil {
		holder.stop()
		return nil, fmt.Errorf("the holder did not start: %w", err)
	}
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case pid := <-ready:
		if pid > 0 {
			identity, err := identify(pid)
			if err != nil {
				holder.stop()
				return nil, fmt.Errorf("the holder's process %d cannot be identified: %w", pid, err)
			}
			holder.pid, holder.identity = pid, identity
			return holder, nil
		}
		holder.stop()
		reason, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		if reason == "" {
			reason = "it stopped before its program ran"
		}
		return nil, fmt.Errorf("the holder did not start: %s", reason)
	case <-deadline:
		holder.stop()
		return nil, fmt.Errorf("the holder did not start within %s", timeout)
	case <-ctx.Done():
		holder.stop()
		return nil, ctx.Err()
	}
}

// stop ends the holder (closing its input ends its program, and with the namespace's init
// every process in it) and waits for it, at most the holder's timeout before killing it.
func (h *sandboxHolder) stop() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		_ = h.input.Close()
		done := make(chan struct{})
		go func() {
			_ = h.command.Wait()
			close(done)
		}()
		var deadline <-chan time.Time
		if h.timeout > 0 {
			timer := time.NewTimer(h.timeout)
			defer timer.Stop()
			deadline = timer.C
		}
		select {
		case <-done:
		case <-deadline:
			h.kill()
			<-done
		}
		h.mu.Lock()
		h.identity.close()
		h.mu.Unlock()
	})
}

// alive reports whether the holder is still the process it was started as: its pid exists and
// has the start time it had (so a pid another process reused does not count), and its pidfd has
// not seen it exit.
func (h *sandboxHolder) alive() bool {
	if h == nil {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.identity.alive()
}

// kill ends the holder's namespace by killing its first process, and the bwrap that started it,
// but only while the holder is the process it was started as.
func (h *sandboxHolder) kill() {
	h.mu.RLock()
	_ = h.identity.kill()
	h.mu.RUnlock()
	// bwrap is jad's own child, not yet waited for: its pid cannot have been reused.
	_ = h.command.Process.Kill()
}

// root is the run's file system as the holder sees it; holderGoneRoot, which no read can
// succeed in, once the holder is gone or its pid is another process's.
func (h *sandboxHolder) root() string {
	if !h.alive() {
		return holderGoneRoot
	}
	return "/proc/" + strconv.Itoa(h.pid) + "/root"
}

// limitedWriter keeps the first limit bytes written to it.
type limitedWriter struct {
	builder *strings.Builder
	limit   int
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if room := w.limit - w.builder.Len(); room > 0 {
		if len(data) > room {
			w.builder.Write(data[:room])
		} else {
			w.builder.Write(data)
		}
	}
	return len(data), nil
}

// errHolderGone is what a join of a holder that is gone, or whose pid another process has, fails
// with.
var errHolderGone = errors.New("the overlay run's holder is gone, so nothing can join it")

// joinHolder is launch started inside holder's namespaces with cwd as its working directory
// there; the process itself starts from the host's root, since cwd may exist only inside.
func joinHolder(holder *sandboxHolder, cwd string, launch sandboxLaunch) (sandboxLaunch, error) {
	if !holder.alive() {
		return sandboxLaunch{}, errHolderGone
	}
	program, err := exec.LookPath(holderJoinProgram)
	if err != nil {
		return sandboxLaunch{}, fmt.Errorf("an overlay run's process joins its holder with %s: %w", holderJoinProgram, err)
	}
	arguments := []string{"-t", strconv.Itoa(holder.pid), "-U", "-m", "-p", "--preserve-credentials"}
	if cwd != "" {
		arguments = append(arguments, "--wdns="+cwd)
	}
	arguments = append(append(arguments, "--", launch.resolved), launch.args...)
	return sandboxLaunch{configured: holderJoinProgram, resolved: program, args: arguments, dir: string(filepath.Separator)}, nil
}

// holderExecutable is the bwrap an overlay run's holder starts with: the run's sandbox's when
// that is a bwrap, else the default one.
func holderExecutable(sandbox *sessionfs.SandboxSettings) string {
	return HolderExecutable(sandbox)
}

// HolderExecutable is holderExecutable for jad check, which tries the overlay with it.
func HolderExecutable(sandbox *sessionfs.SandboxSettings) string {
	if sandbox != nil && sandbox.Backend == sessionfs.SandboxBackendBwrap && sandbox.Executable != "" {
		return sandbox.Executable
	}
	return sessionfs.DefaultSandboxExecutable
}

// overlayProbes caches, per bwrap executable, whether an overlay run works on this host
// (probed once per process; a holder that fails to start later marks it as not working).
var overlayProbes sync.Map // executable -> *overlayProbeResult

type overlayProbeResult struct {
	once   sync.Once
	mu     sync.Mutex
	reason string
}

// overlayProbe is the trial an overlay run must pass.
var overlayProbe = ProbeOverlay

// autoOverlay lets materialize auto take the overlay where it works. The package's tests turn
// it off, so their runs check out unless a test asks for overlay by name.
var autoOverlay = true

// overlayAvailable reports whether overlay runs work with bwrap on this host, and why not.
func overlayAvailable(ctx context.Context, bwrap string, timeout time.Duration) (bool, string) {
	entry, _ := overlayProbes.LoadOrStore(bwrap, &overlayProbeResult{})
	result := entry.(*overlayProbeResult)
	result.once.Do(func() {
		// The result is kept for the process: a run that is stopped meanwhile must not
		// decide it.
		if err := overlayProbe(context.WithoutCancel(ctx), bwrap, timeout); err != nil {
			result.mu.Lock()
			result.reason = err.Error()
			result.mu.Unlock()
			slog.Default().Info("isolated runs use the checkout materialiser: the overlay one does not work on this host", "executable", bwrap, "reason", err)
		}
	})
	result.mu.Lock()
	defer result.mu.Unlock()
	return result.reason == "", result.reason
}

// markOverlayUnavailable records that a holder failed to start, so later auto runs check out.
func markOverlayUnavailable(bwrap string, err error) {
	entry, _ := overlayProbes.LoadOrStore(bwrap, &overlayProbeResult{})
	result := entry.(*overlayProbeResult)
	result.once.Do(func() {})
	result.mu.Lock()
	result.reason = err.Error()
	result.mu.Unlock()
}

// resolveMaterialize turns sandbox.isolation.materialize into the worktree's materialiser:
// auto (also empty) takes overlay where it works here and checkout elsewhere, a fallback that
// changes cost, not confinement; overlay where it does not work fails the run with why. A run
// with share_paths needs the overlay one and is never given a checkout without them: it fails,
// naming the probe that failed, instead of running without the folders.
func resolveMaterialize(ctx context.Context, materialize string, shares []string, bwrap string, timeout time.Duration) (string, error) {
	switch materialize {
	case sessionfs.SandboxIsolationMaterializeCheckout:
		if len(shares) > 0 {
			return "", fmt.Errorf("sandbox.isolation.share_paths %q need the overlay materialiser and cannot be set with materialize %s", shares, sessionfs.SandboxIsolationMaterializeCheckout)
		}
		return worktree.MaterializeCheckout, nil
	case sessionfs.SandboxIsolationMaterializeOverlay:
		if ok, reason := overlayAvailable(ctx, bwrap, timeout); !ok {
			return "", fmt.Errorf("sandbox.isolation.materialize is overlay, which does not work on this host: %s", reason)
		}
		return worktree.MaterializeOverlay, nil
	default:
		reason := "overlay runs are turned off"
		if autoOverlay {
			var ok bool
			if ok, reason = overlayAvailable(ctx, bwrap, timeout); ok {
				return worktree.MaterializeOverlay, nil
			}
		}
		if len(shares) > 0 {
			return "", fmt.Errorf("sandbox.isolation.share_paths %q need the overlay materialiser, and materialize auto would check out because the overlay trial with %s failed: %s", shares, bwrap, reason)
		}
		return worktree.MaterializeCheckout, nil
	}
}

// ProbeOverlay tries what an overlay run needs, end to end, in a scratch folder: a holder of
// a one-file overlay started with bwrap (bubblewrap 0.10 or later, a kernel that lets an
// unprivileged user mount an overlay), a process that joins it with nsenter and reads the
// file in its working directory there, and an in-process read through the holder's root
// (openat2). timeout bounds the holder's start and stop. jad check reports its error.
func ProbeOverlay(ctx context.Context, bwrap string, timeout time.Duration) error {
	if runtime.GOOS != "linux" {
		return errors.New("overlay runs need Linux")
	}
	scratch, err := os.MkdirTemp("", "jad-overlay-probe-")
	if err != nil {
		return err
	}
	defer func() { _ = removeAllWritable(scratch) }()
	layers := worktree.Layers{Lower: []string{filepath.Join(scratch, "lower")}, Upper: filepath.Join(scratch, "upper"), Work: filepath.Join(scratch, "work"), Target: filepath.Join(scratch, "view")}
	for _, folder := range []string{layers.Lower[0], layers.Upper, layers.Work, layers.Target} {
		if err := os.Mkdir(folder, 0o700); err != nil {
			return err
		}
	}
	const probeFile, probeText = "probe", "jad overlay probe\n"
	if err := os.WriteFile(filepath.Join(layers.Lower[0], probeFile), []byte(probeText), 0o600); err != nil {
		return err
	}
	holder, err := startHolder(ctx, bwrap, layers, nil, timeout)
	if err != nil {
		return err
	}
	defer holder.stop()
	reader, err := exec.LookPath(holderProgram)
	if err != nil {
		return err
	}
	launch, err := joinHolder(holder, layers.Target, sandboxLaunch{configured: holderProgram, resolved: reader, args: []string{probeFile}})
	if err != nil {
		return err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	command := exec.CommandContext(ctx, launch.resolved, launch.args...)
	command.Dir = launch.dir
	var stderr strings.Builder
	command.Stderr = &limitedWriter{builder: &stderr, limit: 4096}
	output, err := command.Output()
	if err != nil || string(output) != probeText {
		reason, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		if reason == "" && err != nil {
			reason = err.Error()
		}
		return fmt.Errorf("a process could not join the holder with %s: %s", holderJoinProgram, reason)
	}
	file, err := openInRoot(holder.root(), filepath.Join(layers.Target, probeFile))
	if err != nil {
		return fmt.Errorf("the holder's files cannot be read through its root: %w", err)
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || string(data) != probeText {
		return fmt.Errorf("the holder's files cannot be read through its root: %v", err)
	}
	return nil
}

// removeAllWritable removes path, making its folders writable first when that fails (the
// kernel leaves an overlay's work folder unreadable).
func removeAllWritable(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(name string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(name, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// shareBinds are share_paths as binds: each folder of the session's working directory origin,
// resolved inside it, to be bound at the same place in the run's workspace (see bindShares).
// A folder that is missing, is not a folder, or resolves outside the working directory (a link
// out of it, or the directory itself) is an error: a share is never dropped, since a run
// without its node_modules would fail in ways that look like its own.
func shareBinds(shares []string, origin string) ([]sandboxShare, error) {
	if len(shares) == 0 {
		return nil, nil
	}
	root, err := filepath.EvalSymlinks(origin)
	if err != nil {
		return nil, fmt.Errorf("sandbox.isolation.share_paths: resolve the working directory %s: %w", origin, err)
	}
	binds := make([]sandboxShare, 0, len(shares))
	for _, share := range shares {
		source, err := filepath.EvalSymlinks(filepath.Join(origin, filepath.FromSlash(share)))
		if err != nil {
			return nil, fmt.Errorf("sandbox.isolation.share_paths entry %q: %w", share, err)
		}
		if info, err := os.Stat(source); err != nil || !info.IsDir() || !pathInside(source, root) || source == root {
			return nil, fmt.Errorf("sandbox.isolation.share_paths entry %q is not a folder inside the session's working directory %s", share, origin)
		}
		binds = append(binds, sandboxShare{source: source, target: filepath.FromSlash(share)})
	}
	return binds, nil
}

// bindShares puts the binds' targets, relative to the session's working directory, at
// workspace.
func bindShares(binds []sandboxShare, workspace string) []sandboxShare {
	placed := make([]sandboxShare, len(binds))
	for index, bind := range binds {
		placed[index] = sandboxShare{source: bind.source, target: filepath.Join(workspace, bind.target)}
	}
	return placed
}

// openIsolatedTree makes a run's worktree with the materialiser isolation asks for (auto
// resolved here) and, for an overlay, starts its holder with the run's share_paths. A holder
// that fails to start removes the new worktree, fails the run and makes later auto runs
// check out (design §10.1). The caller registers the run and attaches the holder.
func openIsolatedTree(ctx context.Context, options worktree.Options, materialize string, shares []string, sandbox *sessionfs.SandboxSettings, origin, name, from string) (*worktree.Tree, *sandboxHolder, error) {
	bwrap := holderExecutable(sandbox)
	mode, err := resolveMaterialize(ctx, materialize, shares, bwrap, options.Timeout)
	if err != nil {
		return nil, nil, err
	}
	// Checked before anything is made, so a refused run leaves no worktree.
	binds, err := shareBinds(shares, origin)
	if err != nil {
		return nil, nil, err
	}
	options.Materialize = mode
	tree, err := worktree.Create(ctx, options, origin, name, from)
	if err != nil {
		return nil, nil, err
	}
	layers, overlay := tree.Layers()
	if !overlay {
		return tree, nil, nil
	}
	holder, err := startHolder(ctx, bwrap, layers, bindShares(binds, tree.Workspace), options.Timeout)
	if err != nil {
		if ctx.Err() == nil {
			markOverlayUnavailable(bwrap, err)
		}
		if removeErr := tree.Remove(context.WithoutCancel(ctx)); removeErr != nil {
			return nil, nil, fmt.Errorf("start the overlay run's holder: %w (and the new worktree at %s could not be removed: %v)", err, tree.Path, removeErr)
		}
		return nil, nil, fmt.Errorf("start the overlay run's holder: %w", err)
	}
	return tree, holder, nil
}

// finishIsolatedTree collects and removes a run's worktree (worktree.Tree.Finish) once the run
// is over, with launcher the run's collect launcher. An overlay run's own holder is stopped
// first (with it every process the run left), and the collection runs in a holder of its
// own without the share_paths binds, so it sees only the run's files; that holder stops
// before the folders are removed. bwrap and timeout start it.
func finishIsolatedTree(ctx context.Context, tree *worktree.Tree, message string, launcher func() worktree.Launcher, bwrap string, timeout time.Duration) (worktree.Outcome, error) {
	layers, overlay := tree.Layers()
	if !overlay {
		return tree.Finish(ctx, message, launcher())
	}
	detachHolder(tree)
	holder, err := startHolder(ctx, bwrap, layers, nil, timeout)
	if err != nil {
		outcome, _ := tree.Finish(ctx, message, nil)
		return outcome, fmt.Errorf("start a holder to collect the overlay run: %w", err)
	}
	attachHolder(tree, holder)
	tree.OnCollected(func() { detachHolder(tree) })
	return tree.Finish(ctx, message, launcher())
}

// reapStaleHolder ends the holder a lost server left running on tree's layers, which its state
// file names by pid and start time: a process with that pid and that start time is killed (through
// its pidfd where there is one) and waited for, so no second mount of the upper layer exists
// while the run is collected; a pid with another start time is another process's and is left
// alone, as is one that is gone. The record is cleared afterwards.
func reapStaleHolder(tree *worktree.Tree, timeout time.Duration) error {
	ref := tree.Holder()
	if ref.PID <= 0 {
		return nil
	}
	identity, err := identityOf(ref.PID, ref.Start)
	if err == nil {
		defer identity.close()
		if err := identity.kill(); err != nil {
			return fmt.Errorf("end the holder %d the lost server left: %w", ref.PID, err)
		}
		if !identity.waitGone(timeout) {
			return fmt.Errorf("the holder %d the lost server left did not end within %s", ref.PID, timeout)
		}
	}
	return tree.SetHolder(worktree.HolderRef{})
}

// recoverIsolatedTree finishes the tree of a run a stopped server lost, on the host or, for an
// overlay run, inside a holder of its own; no sandbox of the run is known at startup.
func recoverIsolatedTree(ctx context.Context, tree *worktree.Tree, message string, timeout time.Duration) (worktree.Outcome, error) {
	if _, overlay := tree.Layers(); !overlay {
		return tree.Finish(ctx, message, nil)
	}
	registerIsolatedRun(tree)
	defer forgetIsolatedRun(tree)
	if err := reapStaleHolder(tree, timeout); err != nil {
		return worktree.Outcome{Branch: tree.Branch, Base: tree.Base, Kept: true}, err
	}
	return finishIsolatedTree(ctx, tree, message, func() worktree.Launcher {
		return collectLauncher(tools.ExecutionContext{WorkspaceRoot: tree.Workspace}, tree)
	}, sessionfs.DefaultSandboxExecutable, timeout)
}

// executionSandbox is the sandbox of the run execution belongs to; nil without one.
func executionSandbox(execution tools.ExecutionContext) *sessionfs.SandboxSettings {
	if execution.Environment == nil {
		return nil
	}
	return execution.Environment.Sandbox
}
