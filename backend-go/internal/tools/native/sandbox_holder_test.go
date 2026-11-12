package native

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/worktree"
)

// requireOverlay skips a test on a host without what an overlay run needs: bwrap with
// --overlay (0.10 or later) that can make namespaces, and nsenter with --wdns. Where all of
// them are there the trial start (ProbeOverlay) must pass.
func requireOverlay(t *testing.T) {
	t.Helper()
	requireBwrap(t)
	if help, _ := exec.Command("bwrap", "--help").CombinedOutput(); !strings.Contains(string(help), "--overlay-src") {
		t.Skip("bwrap has no --overlay (it needs 0.10 or later)")
	}
	if _, err := exec.LookPath("nsenter"); err != nil {
		t.Skip("nsenter is not installed")
	}
	if help, _ := exec.Command("nsenter", "--help").CombinedOutput(); !strings.Contains(string(help), "--wdns") {
		t.Skip("nsenter has no --wdns")
	}
	if err := ProbeOverlay(context.Background(), "bwrap", 30*time.Second); err != nil {
		t.Fatalf("this host has bwrap --overlay and nsenter --wdns, yet the overlay trial fails: %v", err)
	}
}

// overlayRun is an overlay run of a new repository with its holder, registered as a run's
// worktree is; the test's cleanup stops the holder and removes what is left.
func overlayRun(t *testing.T, name string, shares []string) (string, *worktree.Tree) {
	t.Helper()
	requireOverlay(t)
	root := gitRepository(t)
	tree, holder, err := openIsolatedTree(context.Background(), isolatedTestOptions(), sessionfs.SandboxIsolationMaterializeOverlay, shares, nil, root, name, "")
	if err != nil {
		t.Fatal(err)
	}
	if holder == nil {
		t.Fatal("materialize overlay made no holder")
	}
	registerIsolatedRun(tree)
	attachHolder(tree, holder)
	t.Cleanup(func() { forgetIsolatedRun(tree) })
	return root, tree
}

// holderBash runs command with bash in the run's workspace through executeNativeBash, the way
// exec starts a child: with sandbox, or without one.
func holderBash(t *testing.T, tree *worktree.Tree, sandbox *sessionfs.SandboxSettings, planMode bool, command string) string {
	t.Helper()
	settings := bashTestSettings(t, tree.Workspace)
	settings.Sandbox = sandbox
	return executeNativeBash(context.Background(), map[string]any{"command": command}, settings, "", tools.ExecutionContext{
		CallID: "holder-call", WorkspaceRoot: tree.Workspace, SessionDir: t.TempDir(), SessionKey: "holder", PlanMode: planMode,
	})
}

func TestHolderArgumentsMountTheOverlayThenTheShares(t *testing.T) {
	layers := worktree.Layers{Lower: []string{"/r/.base/c"}, Upper: "/r/.overlay/x/upper", Work: "/r/.overlay/x/work", Target: "/r/x"}
	got := strings.Join(holderArguments(layers, []sandboxShare{{source: "/s/node_modules", target: "/r/x/node_modules"}}, "/usr/bin/cat"), " ")
	want := "--die-with-parent --unshare-user --unshare-pid --dev-bind / / --proc /proc --overlay-src /r/.base/c --overlay /r/.overlay/x/upper /r/.overlay/x/work /r/x --ro-bind /s/node_modules /r/x/node_modules --chdir / --info-fd 3 -- /usr/bin/cat"
	if got != want {
		t.Fatalf("holder arguments\n got %s\nwant %s", got, want)
	}
}

func TestJoinHolderEntersTheHoldersNamespacesAtTheCallsFolder(t *testing.T) {
	if _, err := exec.LookPath("nsenter"); err != nil {
		t.Skip("nsenter is not installed")
	}
	// A holder that is a live process (this one stands in for it).
	self, err := identify(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(self.close)
	launch, err := joinHolder(&sandboxHolder{pid: os.Getpid(), identity: self}, "/r/x/sub", sandboxLaunch{configured: "bwrap", resolved: "/usr/bin/bwrap", args: []string{"--", "bash"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(launch.args, " "); got != "-t "+strconv.Itoa(os.Getpid())+" -U -m -p --preserve-credentials --wdns=/r/x/sub -- /usr/bin/bwrap -- bash" || launch.dir != "/" || launch.configured != "nsenter" {
		t.Fatalf("launch = %+v", launch)
	}
}

// sandboxKey differs per holder, so a standby or language server never serves another run.
func TestSandboxKeyNamesTheHolder(t *testing.T) {
	tree := &worktree.Tree{Path: filepath.Join(t.TempDir(), "run"), Workspace: ""}
	tree.Workspace = tree.Path
	registerIsolatedRun(tree)
	t.Cleanup(func() { forgetIsolatedRun(tree) })
	place := sandboxPlace{workspace: tree.Workspace}
	plain, unsandboxed := sandboxKey(bwrapSandbox(), place), sandboxKey(nil, place)
	isolatedRuns.Lock()
	run := isolatedRuns.byPath[filepath.Clean(tree.Path)]
	run.holder = &sandboxHolder{pid: 4242}
	isolatedRuns.byPath[filepath.Clean(tree.Path)] = run
	isolatedRuns.Unlock()
	held, heldUnsandboxed := sandboxKey(bwrapSandbox(), place), sandboxKey(nil, place)
	isolatedRuns.Lock()
	run.holder = nil
	isolatedRuns.byPath[filepath.Clean(tree.Path)] = run
	isolatedRuns.Unlock()
	if plain == held || unsandboxed != "" || heldUnsandboxed == "" || !strings.Contains(held, "4242") {
		t.Fatalf("keys: %q %q %q %q", plain, held, unsandboxed, heldUnsandboxed)
	}
}

// materialize auto takes the overlay only where it works; overlay where it does not fails with
// the reason, and a holder that failed makes later auto runs check out.
func TestResolveMaterializeFallsBackOnlyForAuto(t *testing.T) {
	const missing = "/nonexistent/bwrap-for-the-test"
	saved := autoOverlay
	autoOverlay = true
	t.Cleanup(func() { autoOverlay = saved; overlayProbes.Delete(missing) })
	if mode, err := resolveMaterialize(context.Background(), "", nil, missing, time.Second); err != nil || mode != worktree.MaterializeCheckout {
		t.Fatalf("auto = %q, %v", mode, err)
	}
	if _, err := resolveMaterialize(context.Background(), sessionfs.SandboxIsolationMaterializeOverlay, nil, missing, time.Second); err == nil || !strings.Contains(err.Error(), "materialize is overlay") {
		t.Fatalf("overlay where it does not work: %v", err)
	}
	if mode, err := resolveMaterialize(context.Background(), sessionfs.SandboxIsolationMaterializeCheckout, nil, missing, time.Second); err != nil || mode != worktree.MaterializeCheckout {
		t.Fatalf("checkout = %q, %v", mode, err)
	}
	const failed = "/nonexistent/bwrap-that-failed"
	t.Cleanup(func() { overlayProbes.Delete(failed) })
	markOverlayUnavailable(failed, os.ErrPermission)
	if ok, reason := overlayAvailable(context.Background(), failed, time.Second); ok || reason == "" {
		t.Fatalf("after a failed holder: %v %q", ok, reason)
	}
}

// F1: share_paths are never dropped. With materialize checkout they are refused; with auto
// on a host where the overlay trial fails the run is refused naming the failed probe (a run
// without share_paths still checks out); autoOverlay off is the same refusal.
func TestShareEntriesNeverFallBackToACheckout(t *testing.T) {
	const missing = "/nonexistent/bwrap-for-the-share-test"
	saved := autoOverlay
	autoOverlay = true
	t.Cleanup(func() { autoOverlay = saved; overlayProbes.Delete(missing) })
	shares := []string{"node_modules"}
	if _, err := resolveMaterialize(context.Background(), sessionfs.SandboxIsolationMaterializeCheckout, shares, missing, time.Second); err == nil || !strings.Contains(err.Error(), "share_paths") || !strings.Contains(err.Error(), "checkout") {
		t.Fatalf("checkout with shares: %v", err)
	}
	for _, materialize := range []string{"", sessionfs.SandboxIsolationMaterializeAuto} {
		_, err := resolveMaterialize(context.Background(), materialize, shares, missing, time.Second)
		if err == nil || !strings.Contains(err.Error(), "share_paths") || !strings.Contains(err.Error(), "overlay trial") || !strings.Contains(err.Error(), missing) {
			t.Fatalf("auto %q with shares: %v", materialize, err)
		}
		// The reason is the probe's own.
		_, reason := overlayAvailable(context.Background(), missing, time.Second)
		if reason == "" || !strings.Contains(err.Error(), reason) {
			t.Fatalf("the refusal %q does not carry the probe's reason %q", err, reason)
		}
		if mode, err := resolveMaterialize(context.Background(), materialize, nil, missing, time.Second); err != nil || mode != worktree.MaterializeCheckout {
			t.Fatalf("auto without shares = %q, %v", mode, err)
		}
	}
	autoOverlay = false
	if _, err := resolveMaterialize(context.Background(), "", shares, missing, time.Second); err == nil || !strings.Contains(err.Error(), "turned off") {
		t.Fatalf("auto with the overlay turned off: %v", err)
	}
	// openIsolatedTree is the one chokepoint of sub-agent and delegated runs: no worktree is made.
	root := gitRepository(t)
	if _, _, err := openIsolatedTree(context.Background(), isolatedTestOptions(), "", shares, nil, root, "refused", ""); err == nil {
		t.Fatal("openIsolatedTree made a run without its share_paths")
	}
	if list := gitCommand(t, root, "worktree", "list"); strings.Count(list, "\n") != 0 {
		t.Fatalf("a refused run left a worktree:\n%s", list)
	}
}

// A share_paths folder that is missing, is a file, or resolves outside the working directory
// fails the run; none is left out.
func TestShareBindsRefuseWhatTheyCannotBind(t *testing.T) {
	root := gitRepository(t)
	outside := t.TempDir()
	for name, setup := range map[string]func(){
		"file": func() { _ = os.WriteFile(filepath.Join(root, "file"), nil, 0o644) },
		"link": func() { _ = os.Symlink(outside, filepath.Join(root, "link")) },
	} {
		setup()
		if _, err := shareBinds([]string{"keep", name}, root); err == nil || !strings.Contains(err.Error(), `"`+name+`"`) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, share := range []string{"missing", "keep/missing", "../outside"} {
		if _, err := shareBinds([]string{"keep", share}, root); err == nil || !strings.Contains(err.Error(), share) {
			t.Fatalf("%s: %v", share, err)
		}
	}
	binds, err := shareBinds([]string{"keep"}, root)
	if err != nil || len(binds) != 1 || binds[0].source != filepath.Join(root, "keep") || binds[0].target != "keep" {
		t.Fatalf("binds = %+v, %v", binds, err)
	}
	if placed := bindShares(binds, "/run/ws"); placed[0].target != "/run/ws/keep" {
		t.Fatalf("placed = %+v", placed)
	}
}

// A run refused for a share it cannot bind is refused before its worktree is made.
func TestRefusedShareLeavesNoOverlayRun(t *testing.T) {
	requireOverlay(t)
	root := gitRepository(t)
	if _, _, err := openIsolatedTree(context.Background(), isolatedTestOptions(), sessionfs.SandboxIsolationMaterializeOverlay, []string{"keep", "missing"}, nil, root, "overlay-refused", ""); err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Fatalf("openIsolatedTree = %v", err)
	}
	if list := gitCommand(t, root, "worktree", "list"); strings.Count(list, "\n") != 0 {
		t.Fatalf("a refused run left a worktree:\n%s", list)
	}
	if _, err := os.Lstat(filepath.Join(root, ".git", worktree.DefaultDirName, ".overlay", "overlay-refused")); err == nil {
		t.Fatal("a refused run left its layers")
	}
}

// §11 overlay: the run's files exist in its holder only; two joins see one another's writes;
// the copied index leaves git status clean; in-process readers and the shell_changes scan see
// the run's files through the holder; a sandboxed call works in there as in a checkout.
func TestOverlayRunWorksInsideItsHolder(t *testing.T) {
	useOwnTempDir(t)
	_, tree := overlayRun(t, "overlay-run", nil)
	if entries, _ := os.ReadDir(tree.Path); len(entries) != 1 || entries[0].Name() != ".git" {
		t.Fatalf("on the host the worktree holds %v, want only .git", entries)
	}
	if out := holderBash(t, tree, nil, false, "git status --porcelain --untracked-files=all; cat a.txt; echo first > made.txt"); strings.TrimSpace(out) == "" || !strings.Contains(out, "a\n") || strings.Contains(out, "?? ") || strings.Contains(out, " M ") {
		t.Fatalf("first join:\n%s", out)
	}
	requireOutput(t, holderBash(t, tree, nil, false, "cat made.txt"), "first")
	if _, err := os.Stat(filepath.Join(tree.Path, "made.txt")); err == nil {
		t.Fatal("the run's file is on the host")
	}
	// An in-process reader (read_image and code_context read like this).
	target, failure := resolveExistingTarget("made.txt", tree.Workspace, sessionfs.NativePathPolicy{}, "c")
	if failure != "" {
		t.Fatal(failure)
	}
	info, err := os.Lstat(target.openPath)
	if err != nil {
		t.Fatal(err)
	}
	if data, _, err := readVerifiedTarget(target, info, 1024); err != nil || string(data) != "first\n" || target.realPath != filepath.Join(tree.Workspace, "made.txt") {
		t.Fatalf("read %q %v (real path %s)", data, err, target.realPath)
	}
	// The shell_changes scan.
	before, _ := scanWorkspaceFiles(viewPath(tree.Workspace), sessionfs.HooksSettings{ShellScanMaxFiles: 100})
	holderBash(t, tree, nil, false, "echo second > second.txt")
	after, _ := scanWorkspaceFiles(viewPath(tree.Workspace), sessionfs.HooksSettings{ShellScanMaxFiles: 100})
	if changes := diffWorkspaceFiles(before, after); len(changes) != 1 || changes[0].path != "second.txt" || changes[0].action != "created" {
		t.Fatalf("changes = %+v", changes)
	}
	// A sandboxed call starts its own bwrap inside the holder: it writes the workspace, a
	// hide path that exists only in the overlay is hidden, and a plan-mode call cannot write.
	sandbox := bwrapSandbox()
	sandbox.HidePaths = []string{"{workspace}/made.txt"}
	requireOutput(t, holderBash(t, tree, sandbox, false, "echo boxed > boxed.txt && echo wrote; cat made.txt 2>&1 | head -c 20; echo; cat second.txt"), "wrote", "second")
	if out := holderBash(t, tree, sandbox, false, "cat made.txt"); strings.Contains(out, "first") {
		t.Fatalf("a hidden file of the overlay was readable:\n%s", out)
	}
	sandbox.PlanWritablePaths = []string{"{tmp}"}
	requireOutput(t, holderBash(t, tree, sandbox, true, "echo plan > plan.txt 2>&1 || echo refused"), "refused")
	requireOutput(t, holderBash(t, tree, nil, false, "cat boxed.txt; test -e plan.txt || echo no-plan"), "boxed", "no-plan")
}

// §11 overlay: the lock keeps a second mount of the run's layers from starting while the run
// holds them; Finish collects the run's work inside a holder of its own, imports it as a
// branch, removes every folder and keeps the base checkout of HEAD for the next run.
func TestOverlayRunIsLockedAndFinishesToABranch(t *testing.T) {
	useOwnTempDir(t)
	root, tree := overlayRun(t, "overlay-finish", nil)
	if _, err := worktree.Open(context.Background(), isolatedTestOptions(), tree.State()); err == nil || !strings.Contains(err.Error(), "still held") {
		t.Fatalf("Open of a held run: %v", err)
	}
	holderBash(t, tree, nil, false, "echo changed > a.txt; echo new > new.txt; rm keep/secret.txt")
	// A process of the run that is still going when the run ends dies with the holder.
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not installed")
	}
	launch, err := joinHolder(holderFor(tree.Workspace), tree.Workspace, sandboxLaunch{configured: "sh", resolved: shell, args: []string{"-c", "echo up; exec sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}
	left := exec.Command(launch.resolved, launch.args...)
	left.Dir = launch.dir
	output, err := left.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := left.Start(); err != nil {
		t.Fatal(err)
	}
	if up := make([]byte, 3); func() bool { _, err := io.ReadFull(output, up); return err != nil || string(up) != "up\n" }() {
		t.Fatal("the run's process did not start in its holder")
	}
	ended := make(chan struct{})
	go func() {
		_ = left.Wait()
		close(ended)
	}()
	t.Cleanup(func() { _ = left.Process.Kill() })
	outcome, err := finishIsolatedTree(context.Background(), tree, "overlay run", func() worktree.Launcher {
		return collectLauncher(tools.ExecutionContext{WorkspaceRoot: tree.Workspace}, tree)
	}, "bwrap", 30*time.Second)
	if err != nil || !outcome.Changed {
		t.Fatalf("outcome %+v, err %v", outcome, err)
	}
	if got := gitCommand(t, root, "show", tree.Branch+":a.txt"); got != "changed" {
		t.Fatalf("a.txt on the branch = %q", got)
	}
	if files := gitCommand(t, root, "ls-tree", "-r", "--name-only", tree.Branch); files != "a.txt\nnew.txt" {
		t.Fatalf("files on the branch:\n%s", files)
	}
	for _, path := range []string{tree.Path, tree.Admin, filepath.Join(filepath.Dir(tree.Path), ".overlay", filepath.Base(tree.Path))} {
		if _, err := os.Lstat(path); err == nil {
			t.Fatalf("%s is left", path)
		}
	}
	head := gitCommand(t, root, "rev-parse", "HEAD")
	if _, err := os.Stat(filepath.Join(tree.Common(), worktree.DefaultDirName, ".base", head, "tree", "a.txt")); err != nil {
		t.Fatalf("the base checkout of HEAD is gone: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(30 * time.Second):
		t.Fatal("a process the run left outlived its holder")
	}
}

// §10.3: an overlay run whose server stopped is recovered at the next start: its layers are
// mounted again in a holder of their own and its work becomes a branch.
func TestOverlayRunIsRecoveredAfterTheServerStopped(t *testing.T) {
	useOwnTempDir(t)
	root, tree := overlayRun(t, "overlay-lost", nil)
	holderBash(t, tree, nil, false, "echo lost > lost.txt")
	// The server stops: its holder dies and the lock goes with it. A Finish without a
	// launcher keeps everything and lets the lock go, as the end of the process does.
	detachHolder(tree)
	if outcome, err := tree.Finish(context.Background(), "x", nil); err == nil || !outcome.Kept {
		t.Fatalf("an overlay collected without its mount: %+v %v", outcome, err)
	}
	forgetIsolatedRun(tree)
	reopened, err := worktree.Open(context.Background(), isolatedTestOptions(), tree.State())
	if err != nil {
		t.Fatal(err)
	}
	if _, overlay := reopened.Layers(); !overlay {
		t.Fatal("Open did not find the run's overlay")
	}
	outcome, err := recoverIsolatedTree(context.Background(), reopened, "recovered", 30*time.Second)
	if err != nil || !outcome.Changed {
		t.Fatalf("outcome %+v, err %v", outcome, err)
	}
	if got := gitCommand(t, root, "show", tree.Branch+":lost.txt"); got != "lost" {
		t.Fatalf("lost.txt on the branch = %q", got)
	}
	if _, err := os.Lstat(tree.Path); err == nil {
		t.Fatal("the recovered run's folder is left")
	}
	if _, ok := isolatedRunOf(tree.Workspace); ok {
		t.Fatal("the recovered run is still registered")
	}
}

// D6: a share_paths folder is bound read-only from the session into the run; the run cannot
// write it and its content is never part of the branch.
func TestOverlayRunSeesItsSharesReadOnly(t *testing.T) {
	useOwnTempDir(t)
	requireOverlay(t)
	root := gitRepository(t)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("deps/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, root, "add", ".gitignore")
	gitCommand(t, root, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "ignore deps")
	if err := os.MkdirAll(filepath.Join(root, "deps"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deps", "lib.js"), []byte("lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A shared folder git tracks shows the session's own files, which never reach the branch.
	if err := os.WriteFile(filepath.Join(root, "keep", "session-only.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree, holder, err := openIsolatedTree(context.Background(), isolatedTestOptions(), sessionfs.SandboxIsolationMaterializeOverlay, []string{"deps", "keep"}, nil, root, "overlay-share", "")
	if err != nil {
		t.Fatal(err)
	}
	registerIsolatedRun(tree)
	attachHolder(tree, holder)
	t.Cleanup(func() { forgetIsolatedRun(tree) })
	requireOutput(t, holderBash(t, tree, nil, false, "cat deps/lib.js; echo x > deps/new.js 2>/dev/null || echo refused; echo work > work.txt"), "lib", "refused")
	// A sandboxed call, whose workspace is writable, cannot write the share either.
	requireOutput(t, holderBash(t, tree, bwrapSandbox(), false, "cat deps/lib.js; echo x > deps/boxed.js 2>/dev/null || echo boxed-refused"), "lib", "boxed-refused")
	// Collected in the run's sandbox, inside a holder without the shares.
	execution := tools.ExecutionContext{WorkspaceRoot: tree.Workspace, SessionKey: "holder", Environment: &tools.Environment{Sandbox: bwrapSandbox()}}
	outcome, err := finishIsolatedTree(context.Background(), tree, "share run", func() worktree.Launcher { return collectLauncher(execution, tree) }, "bwrap", 30*time.Second)
	if err != nil || !outcome.Changed {
		t.Fatalf("outcome %+v, err %v", outcome, err)
	}
	if files := gitCommand(t, root, "ls-tree", "-r", "--name-only", tree.Branch); !slices.Equal(strings.Fields(files), []string{".gitignore", "a.txt", "keep/secret.txt", "work.txt"}) {
		t.Fatalf("files on the branch: %s", files)
	}
	if _, err := os.Stat(filepath.Join(root, "deps", "new.js")); err == nil {
		t.Fatal("the run wrote the session's shared folder")
	}
}

// A holder that cannot start fails the run, removes what Create made and makes later auto
// runs check out (§10.1).
func TestOverlayRunWhoseHolderFailsIsRemoved(t *testing.T) {
	requireOverlay(t)
	root := gitRepository(t)
	saved := holderProgram
	holderProgram = "false"
	t.Cleanup(func() { holderProgram = saved; overlayProbes.Delete("bwrap") })
	// The trial has passed (requireOverlay): recorded as such, so overlay is tried.
	overlayProbes.Delete("bwrap")
	entry, _ := overlayProbes.LoadOrStore("bwrap", &overlayProbeResult{})
	entry.(*overlayProbeResult).once.Do(func() {})
	if _, _, err := openIsolatedTree(context.Background(), isolatedTestOptions(), sessionfs.SandboxIsolationMaterializeOverlay, nil, nil, root, "overlay-fails", ""); err == nil || !strings.Contains(err.Error(), "holder") {
		t.Fatalf("openIsolatedTree = %v", err)
	}
	dir := filepath.Join(root, ".git", worktree.DefaultDirName)
	for _, name := range []string{"overlay-fails", filepath.Join(".overlay", "overlay-fails")} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("%s is left", name)
		}
	}
	// The base checkout of HEAD stays for the next run (base.go); it is no worktree.
	if list := gitCommand(t, root, "worktree", "list"); strings.Count(list, "\n") != 0 {
		t.Fatalf("worktrees left:\n%s", list)
	}
	head := gitCommand(t, root, "rev-parse", "HEAD")
	if _, err := os.Stat(filepath.Join(dir, ".base", head, "tree", "a.txt")); err != nil {
		t.Fatalf("the base checkout of HEAD is gone: %v", err)
	}
	if ok, _ := overlayAvailable(context.Background(), "bwrap", time.Second); ok {
		t.Fatal("a holder that failed left the overlay marked as working")
	}
}

// What jad itself reads or writes of an overlay run's workspace goes through its holder: the
// hooks.shell_changes scan, the exec/tool_smith function library, a post-edit hook's nearest
// marker and an exec spill file the run is pointed at.
func TestOverlayRunIsReadAndWrittenByJadThroughItsHolder(t *testing.T) {
	useOwnTempDir(t)
	_, tree := overlayRun(t, "overlay-jad", nil)
	holderBash(t, tree, nil, false, "mkdir -p fns/sh deep/er && printf '# says hi\\necho hi\\n' > fns/sh/hi.sh && touch deep/go.mod")

	hooks := sessionfs.HooksSettings{Enabled: true, ShellChanges: sessionfs.HookShellChangesScan, ShellScanMaxFiles: 100, ShellMaxChangedFiles: 100}
	scanned := WithShellChangeScan(tools.Tool{Execute: func(_ context.Context, _ map[string]any, execution tools.ExecutionContext) (string, error) {
		return holderBash(t, tree, nil, false, "echo made > made.txt"), nil
	}}, hooks)
	var changed []string
	if _, err := scanned.Execute(context.Background(), nil, tools.ExecutionContext{WorkspaceRoot: tree.Workspace, OnFileChange: func(path, action string) { changed = append(changed, action+" "+path) }}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{"created made.txt"}) {
		t.Fatalf("changes reported = %q", changed)
	}

	dir, realRoot, err := resolveToolSmithFunctionsDir("fns", tree.Workspace, nil)
	if err != nil || dir != filepath.Join(tree.Workspace, "fns") {
		t.Fatalf("functions dir %q: %v", dir, err)
	}
	file, _, problem := openLibraryFile(dir, realRoot, "sh/hi.sh")
	if problem != "" {
		t.Fatal(problem)
	}
	data, _ := io.ReadAll(file)
	_ = file.Close()
	if !strings.Contains(string(data), "echo hi") {
		t.Fatalf("library file = %q", data)
	}

	hook := sessionfs.PostEditHook{Cwd: sessionfs.HookCwdNearestPrefix + "go.mod"}
	if cwd, found := hookWorkingDirectory(hook, tree.Workspace, filepath.Join(tree.Workspace, "deep", "er", "x.go")); !found || cwd != filepath.Join(tree.Workspace, "deep") {
		t.Fatalf("nearest go.mod = %q, %v", cwd, found)
	}

	plan := planBashSpill(sessionfs.NativeBashSpillSettings{Enabled: true, Keep: "always", CapBytes: 1 << 20, StorageDir: filepath.Join(tree.Workspace, ".spill")}, nil, tree.Workspace, t.TempDir(), "spill-call")
	writer := plan.writers[bashStdout].(*bashSpillWriter)
	writer.write([]byte("spilled\n"))
	outcome := writer.close()
	if outcome.err != "" || !strings.HasPrefix(outcome.path, tree.Workspace) {
		t.Fatalf("spill outcome %+v", outcome)
	}
	relative, _ := filepath.Rel(tree.Workspace, outcome.path)
	requireOutput(t, holderBash(t, tree, nil, false, "cat '"+relative+"'"), "spilled")
}

// The trial's result is kept for the process, so a run stopped while it goes does not decide it.
func TestOverlayTrialIgnoresAStoppedRun(t *testing.T) {
	requireOverlay(t)
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap is not installed")
	}
	t.Cleanup(func() { overlayProbes.Delete(bwrap) })
	overlayProbes.Delete(bwrap)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, reason := overlayAvailable(stopped, bwrap, 30*time.Second); !ok {
		t.Fatalf("a stopped run decided the trial: %s", reason)
	}
}
