package native

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// requireBwrap skips unless bubblewrap is installed and can make namespaces here.
func requireBwrap(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("bwrap runs on Linux only")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap is not installed")
	}
	if out, err := exec.Command("bwrap", "--unshare-all", "--ro-bind", "/", "/", "true").CombinedOutput(); err != nil {
		t.Skipf("bwrap cannot create namespaces here: %v %s", err, out)
	}
}

func bwrapSandbox() *sessionfs.SandboxSettings {
	return &sessionfs.SandboxSettings{
		Tools: append([]string(nil), sessionfs.SandboxTools...), Backend: sessionfs.SandboxBackendBwrap, Executable: "bwrap",
		WritablePaths: []string{"{workspace}", "{git_common_dir}", "{tmp}"},
	}
}

// sandboxDirs are a workspace, a folder outside it and the temp folder, all apart: the
// temp folder is writable in the sandbox, so the other two must not lie inside it.
func sandboxDirs(t *testing.T) (root, outside, tmp string) {
	t.Helper()
	root, outside = t.TempDir(), t.TempDir()
	return root, outside, useOwnTempDir(t)
}

func TestSandboxedBashWritesOnlyTheWorkspaceAndHasNoNetwork(t *testing.T) {
	requireBwrap(t)
	root, outside, _ := sandboxDirs(t)
	settings := bashTestSettings(t, root)
	settings.Sandbox = bwrapSandbox()
	command := `touch in.txt && echo wrote-in; touch ` + outside + `/out.txt 2>&1; ` +
		`touch "$TMPDIR/t.txt" && echo wrote-tmp; echo "nets=$(tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d ' ' | tr '\n' ',')"; exit 7`
	output := executeBashTest(t, root, "sandbox-call", command, "", settings)
	for _, want := range []string{"wrote-in", "Read-only file system", "wrote-tmp", "nets=lo,", "exit_code: 7"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in:\n%s", want, output)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "in.txt")); err != nil {
		t.Fatalf("the workspace write did not land: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "out.txt")); err == nil {
		t.Fatal("the sandbox wrote outside its writable paths")
	}
}

func TestSandboxOffRunsTheCommandAsBefore(t *testing.T) {
	root, outside, _ := sandboxDirs(t)
	settings := bashTestSettings(t, root)
	output := executeBashTest(t, root, "plain-call", "touch "+outside+"/out.txt && echo wrote-out", "", settings)
	if !strings.Contains(output, "wrote-out") {
		t.Fatalf("without a sandbox the write must work:\n%s", output)
	}
}

func TestSandboxHidesPathsAndKeepsTheNetworkWhenAsked(t *testing.T) {
	requireBwrap(t)
	root, _, _ := sandboxDirs(t)
	secret := filepath.Join(root, "secret")
	if err := os.Mkdir(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "key"), []byte("hunter2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "token"), []byte("abc123"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := bashTestSettings(t, root)
	settings.Sandbox = bwrapSandbox()
	settings.Sandbox.HidePaths = []string{"{workspace}/secret", "{workspace}/token", "{workspace}/missing"}
	settings.Sandbox.Network = true
	output := executeBashTest(t, root, "hide-call", `echo "dir=[$(ls secret)] file=[$(cat token)]"; test -e /proc/net/dev && echo has-net-proc`, "", settings)
	if !strings.Contains(output, "dir=[] file=[]") || strings.Contains(output, "hunter2") || strings.Contains(output, "abc123") || !strings.Contains(output, "has-net-proc") {
		t.Fatalf("hidden paths leaked or the call failed:\n%s", output)
	}
}

func TestSandboxedExecRunsFreshAndInAStandby(t *testing.T) {
	requireBwrap(t)
	requireInterpreters(t, "python3")
	root, outside, _ := sandboxDirs(t)
	isolateStandbyPool(t)
	bash := bashTestSettings(t, root)
	bash.Sandbox = bwrapSandbox()
	code := "import os\nopen(os.environ['JAD_RESULT'], 'w').write('answer 42')\n" +
		"try:\n    open('" + outside + "/x', 'w')\nexcept OSError as e:\n    print('refused', e.errno)\n"
	fresh := runExec(t, root, execTestSettings(), bash, map[string]any{"lang": "python", "code": code})
	if !strings.Contains(fresh, "answer 42") || !strings.Contains(fresh, "refused 30") {
		t.Fatalf("fresh exec:\n%s", fresh)
	}
	standby := standbyExecSettings()
	runExec(t, root, standby, bash, map[string]any{"lang": "python", "code": "print(1)"})
	waitForReadyStandby(t, execStandby, 1)
	warm := runExec(t, root, standby, bash, map[string]any{"lang": "python", "code": code})
	if !strings.Contains(warm, "answer 42") || !strings.Contains(warm, "refused 30") {
		t.Fatalf("standby exec:\n%s", warm)
	}
}

func TestSandboxedToolSmithRuns(t *testing.T) {
	requireBwrap(t)
	root, outside, _ := sandboxDirs(t)
	settings := toolSmithTestSettings(t, root)
	settings.Child.Sandbox = bwrapSandbox()
	script := "const fs = require('fs'); let refused = '';\n" +
		"try { fs.writeFileSync('" + outside + "/x', 'x'); } catch (e) { refused = e.code; }\n" +
		"return { sum: input.a + input.b, refused };"
	result := runToolSmith(context.Background(), root, settings, smithRun(script, map[string]any{"a": 1, "b": 2}))
	if !strings.Contains(result, `"status":"ok"`) || !strings.Contains(execContent(result), `{"sum":3,"refused":"EROFS"}`) {
		t.Fatalf("result:\n%s", result)
	}
}

func TestSandboxedProcessCannotWriteOutsideTheWorkspace(t *testing.T) {
	requireBwrap(t)
	root, outside, _ := sandboxDirs(t)
	manager := NewProcessManager(filepath.Join(root, "processes"))
	t.Cleanup(func() { _ = manager.Close() })
	request := processTestRequest(root, "touch in.txt && echo in; touch "+outside+"/x 2>&1", 1<<16)
	request.Workspace = root
	request.Policy.Sandbox = bwrapSandbox()
	snapshot, err := manager.Start(request)
	if err != nil {
		t.Fatal(err)
	}
	waited := manager.Wait(snapshot.ID, 10_000)
	if !waited.Found || waited.Snapshot == nil || waited.Snapshot.State != ProcessExited {
		t.Fatalf("Wait() = %#v", waited)
	}
	output, err := os.ReadFile(filepath.Join(root, "processes", snapshot.ID, "stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "in\n") || !strings.Contains(string(output), "Read-only file system") {
		t.Fatalf("process output = %q", output)
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Fatal("the sandboxed process wrote outside its writable paths")
	}
}

// The command backend puts its argv in front of the child; env stands in for a sandbox program.
func TestSandboxCommandBackendWrapsTheChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses env and a POSIX shell")
	}
	root := t.TempDir()
	settings := bashTestSettings(t, root)
	settings.Sandbox = &sessionfs.SandboxSettings{Tools: []string{"bash"}, Backend: sessionfs.SandboxBackendCommand, Executable: "env", Command: []string{"env", "WRAPPED_IN={workspace}"}}
	output := executeBashTest(t, root, "wrap-call", `echo "wrapped=$WRAPPED_IN"`, "", settings)
	if !strings.Contains(output, "wrapped="+root) {
		t.Fatalf("output:\n%s", output)
	}
}

func TestBwrapArgumentsOrderAndPlaceholders(t *testing.T) {
	root := t.TempDir()
	hidden := filepath.Join(root, "hidden")
	if err := os.Mkdir(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	sandbox := &sessionfs.SandboxSettings{
		Backend: sessionfs.SandboxBackendBwrap, Executable: "bwrap",
		WritablePaths: []string{"{workspace}", "{cwd}/out", "{git_common_dir}", "{home}/.cache"},
		HidePaths:     []string{"{workspace}/hidden", "{workspace}/absent"},
	}
	t.Setenv("HOME", "/home/someone")
	place := sandboxPlace{workspace: root, cwd: root + "/sub"}
	got := bwrapArguments(sandbox, place, sandboxExpander(place), "bash", "/usr/bin/bash", []string{"-c", "true"})
	want := []string{
		"--die-with-parent", "--unshare-all", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		"--bind-try", root, root, "--bind-try", root + "/sub/out", root + "/sub/out", "--bind-try", "/home/someone/.cache", "/home/someone/.cache",
		"--tmpfs", hidden, "--chdir", root + "/sub", "--argv0", "bash", "--", "/usr/bin/bash", "-c", "true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments\n got %q\nwant %q", got, want)
	}
	sandbox.Network = true
	if got := bwrapArguments(sandbox, place, sandboxExpander(place), "bash", "/usr/bin/bash", nil); got[2] != "--share-net" {
		t.Fatalf("network true must keep the network: %q", got)
	}
}

func TestGitCommonDirOfACheckoutAndAWorktree(t *testing.T) {
	repo := t.TempDir()
	common := filepath.Join(repo, ".git")
	worktreeGit := filepath.Join(common, "worktrees", "w1")
	tree := filepath.Join(common, "jad-worktrees", "w1")
	for _, dir := range []string{worktreeGit, filepath.Join(repo, "src"), filepath.Join(tree, "src")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, ".git"), []byte("gitdir: "+worktreeGit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeGit, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{
		filepath.Join(repo, "src"): common,
		filepath.Join(tree, "src"): common,
		t.TempDir():                "",
	} {
		if got := gitCommonDir(dir); got != want {
			t.Errorf("gitCommonDir(%s) = %q, want %q", dir, got, want)
		}
	}
}

func TestStandbyKeyIncludesTheSandbox(t *testing.T) {
	spawn := standbySpawn{file: "python3", argv: []string{"-c", "pass"}, cwd: "/w", workspace: "/w"}
	plain := standbyKey(spawn)
	spawn.sandbox = bwrapSandbox()
	boxed := standbyKey(spawn)
	spawn.workspace = "/other"
	moved := standbyKey(spawn)
	if plain == boxed || boxed == moved {
		t.Fatal("a standby must not be shared across sandboxes or workspaces")
	}
}

// A timeout stops the whole sandbox: bwrap, its init and the command's own children.
func TestSandboxedBashTimeoutStopsTheSandbox(t *testing.T) {
	requireBwrap(t)
	root, _, _ := sandboxDirs(t)
	settings := bashTestSettings(t, root)
	settings.TimeoutMs = 1_000
	settings.Sandbox = bwrapSandbox()
	marker := "jad-sandbox-timeout-" + filepath.Base(root)
	started := time.Now()
	output := executeBashTest(t, root, "timeout-call", "sleep 30 & exec -a "+marker+" sleep 30", "", settings)
	if elapsed := time.Since(started); elapsed > 8*time.Second || !strings.Contains(output, "timed_out") {
		t.Fatalf("after %s:\n%s", elapsed, output)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		out, _ := exec.Command("pgrep", "-f", marker).Output()
		if strings.TrimSpace(string(out)) == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sandboxed command still runs: %s", out)
		}
	}
}
