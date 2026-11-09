package native

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func TestSandboxWritableRootsLeaveOutNamesACallControls(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"build", "real"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	// A path outside the workspace that leads into it through a symlink is controlled too.
	via := filepath.Join(outside, "via")
	if err := os.Symlink(filepath.Join(root, "real"), via); err != nil {
		t.Fatal(err)
	}
	place := sandboxPlace{workspace: root}
	got := sandboxWritableRoots([]string{"{workspace}", "{workspace}/build", "{workspace}/link", via, "{workspace}", "{workspace}/missing"}, sandboxExpander(place))
	if want := []string{root}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roots = %q, want %q", got, want)
	}
	// A path no other writable path covers stays, resolved.
	got = sandboxWritableRoots([]string{outside, "{workspace}/build"}, sandboxExpander(place))
	if want := []string{outside, filepath.Join(root, "build")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roots = %q, want %q", got, want)
	}
}

func TestSeatbeltProfileHasTheFixedRules(t *testing.T) {
	useOwnTempDir(t)
	root := gitRepository(t)
	tmp, err := callTempDir(bwrapSandbox(), tools.ExecutionContext{WorkspaceRoot: root, SessionKey: "mac"})
	if err != nil {
		t.Fatal(err)
	}
	tmpRoot, _ := filepath.EvalSymlinks(sandboxTempRoot())
	tmp, _ = filepath.EvalSymlinks(tmp)
	common := filepath.Join(root, ".git")
	sandbox := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendSeatbelt, Executable: "sandbox-exec", WritablePaths: []string{"{workspace}", "{git_common_dir}", "{tmp}"}, HidePaths: []string{"{workspace}/a.txt"}}
	place := sandboxPlace{workspace: root, cwd: root, tmp: tmp}
	profile := seatbeltProfile(sandbox, place, sandboxExpander(place))
	quote := seatbeltString
	order := []string{
		"(allow file-write* ",
		"(deny file-read* file-write* (subpath " + quote(tmpRoot) + "))",
		"(allow file-write* (subpath " + quote(tmp) + "))",
		"(deny file-write* (literal " + quote(common) + "))",
		"(deny file-write* (literal " + quote(filepath.Join(common, "hooks")) + ") (subpath " + quote(filepath.Join(common, "hooks")) + "))",
		"(deny file-write* (literal " + quote(filepath.Join(common, "config")) + "))",
		"(deny file-read* file-write* (literal " + quote(filepath.Join(root, "a.txt")) + "))",
	}
	at := 0
	for _, want := range order {
		index := strings.Index(profile[at:], want)
		if index < 0 {
			t.Fatalf("missing, or out of order, %q in:\n%s", want, profile)
		}
		at += index + len(want)
	}
}

func TestCallTempDirIsPrivateAndPerSessionWorkspaceAndForm(t *testing.T) {
	useOwnTempDir(t)
	if dir, err := callTempDir(nil, tools.ExecutionContext{}); dir != "" || err != nil {
		t.Fatalf("no sandbox = %q, %v", dir, err)
	}
	sandbox := bwrapSandbox()
	base := tools.ExecutionContext{WorkspaceRoot: "/w", SessionKey: "a"}
	dirs := map[string]bool{}
	for _, execution := range []tools.ExecutionContext{base, {WorkspaceRoot: "/w", SessionKey: "b"}, {WorkspaceRoot: "/x", SessionKey: "a"}, {WorkspaceRoot: "/w", SessionKey: "a", PlanMode: true}} {
		dir, err := callTempDir(sandbox, execution)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(dir)
		if err != nil || info.Mode().Perm() != 0o700 || filepath.Dir(dir) != sandboxTempRoot() {
			t.Fatalf("%s: %v %v", dir, info, err)
		}
		dirs[dir] = true
	}
	if len(dirs) != 4 {
		t.Fatalf("temp folders shared between sessions, workspaces or forms: %v", dirs)
	}
	if again, _ := callTempDir(sandbox, base); !dirs[again] {
		t.Fatalf("the same call got another folder %s", again)
	}
	// A folder another user (or a loose mode) made is refused.
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(loose); err == nil {
		t.Fatal("a folder with mode 0755 was accepted")
	}
}

func TestSweepStaleSandboxTemp(t *testing.T) {
	root := filepath.Join(t.TempDir(), sandboxTempDirName+"0")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	old, fresh := filepath.Join(root, "old"), filepath.Join(root, "fresh")
	for _, dir := range []string{old, fresh} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if err := os.Chtimes(old, now.Add(-8*time.Hour), now.Add(-8*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if removed := sweepStaleSandboxTemp(root, now, staleScratchAge); removed != 1 {
		t.Fatalf("removed %d, want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the stale folder is still there")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("the fresh folder was removed")
	}
}
