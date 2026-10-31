package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func scopeRun(t *testing.T, tool tools.Tool, root string, scope *tools.PathScope, arguments map[string]any) string {
	t.Helper()
	output, err := tool.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root, PathScope: scope})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func scopeWorkspace(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"src", "docs", "src/secret"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func pathExists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestPathScopeWriteStaysInsideAllow(t *testing.T) {
	root := scopeWorkspace(t)
	tool, err := NewWriteTool(testWriteSettings(root, 1<<20), NewPathLocks())
	if err != nil {
		t.Fatal(err)
	}
	scope := &tools.PathScope{Allow: []string{"src"}}
	assertMutationStatus(t, scopeRun(t, tool, root, scope, map[string]any{"path": "src/a.txt", "content": "ok"}), "ok")
	assertMutationStatus(t, scopeRun(t, tool, root, scope, map[string]any{"path": "src/new/deep.txt", "content": "ok"}), "ok")
	for name, path := range map[string]string{
		"sibling":        "docs/a.txt",
		"workspace root": "top.txt",
		"dotdot escape":  "src/../docs/b.txt",
		"prefix sibling": "src2/c.txt",
		"absolute":       filepath.Join(root, "docs", "d.txt"),
		"absolute /tmp":  filepath.Join(os.TempDir(), "scope-outside-"+filepath.Base(root)),
	} {
		out := scopeRun(t, tool, root, scope, map[string]any{"path": path, "content": "no"})
		if !strings.Contains(out, "path_outside_agent_paths") {
			t.Errorf("%s (%s): got %q, want path_outside_agent_paths", name, path, out)
		}
	}
	for _, leaked := range []string{"docs/a.txt", "top.txt", "docs/b.txt", "src2/c.txt", "docs/d.txt"} {
		if pathExists(filepath.Join(root, leaked)) {
			t.Errorf("%s was written outside the allowed paths", leaked)
		}
	}
}

func TestPathScopeDenyBeatsAllow(t *testing.T) {
	root := scopeWorkspace(t)
	tool, _ := NewWriteTool(testWriteSettings(root, 1<<20), NewPathLocks())
	scope := &tools.PathScope{Allow: []string{"src"}, Deny: []string{"src/secret"}}
	assertMutationStatus(t, scopeRun(t, tool, root, scope, map[string]any{"path": "src/ok.txt", "content": "ok"}), "ok")
	out := scopeRun(t, tool, root, scope, map[string]any{"path": "src/secret/key.txt", "content": "no"})
	if !strings.Contains(out, "path_denied") || pathExists(filepath.Join(root, "src/secret/key.txt")) {
		t.Fatalf("a denied path inside an allowed one was written: %q", out)
	}
	// Deny alone, without an allow list, leaves everything else open.
	only := &tools.PathScope{Deny: []string{"docs"}}
	assertMutationStatus(t, scopeRun(t, tool, root, only, map[string]any{"path": "top.txt", "content": "ok"}), "ok")
	if out := scopeRun(t, tool, root, only, map[string]any{"path": "docs/x.txt", "content": "no"}); !strings.Contains(out, "path_denied") {
		t.Fatalf("deny only: %q", out)
	}
}

func TestPathScopeEditAndSymlinkEscape(t *testing.T) {
	root := scopeWorkspace(t)
	write, _ := NewWriteTool(testWriteSettings(root, 1<<20), NewPathLocks())
	edit, err := NewEditTool(testEditSettings(root, 1<<20), NewPathLocks())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "d.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "s.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := &tools.PathScope{Allow: []string{"src"}}
	out := scopeRun(t, edit, root, scope, map[string]any{"path": "docs/d.txt", "oldText": "old", "newText": "new"})
	if !strings.Contains(out, "path_outside_agent_paths") {
		t.Fatalf("edit outside allow: %q", out)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "docs", "d.txt")); string(data) != "old" {
		t.Fatalf("docs/d.txt changed to %q", data)
	}
	assertMutationStatus(t, scopeRun(t, edit, root, scope, map[string]any{"path": "src/s.txt", "oldText": "old", "newText": "new"}), "ok")

	// A symlink inside src that points at docs must not carry a write out of src.
	if err := os.Symlink(filepath.Join(root, "docs", "d.txt"), filepath.Join(root, "src", "link.txt")); err != nil {
		t.Fatal(err)
	}
	if out := scopeRun(t, write, root, scope, map[string]any{"path": "src/link.txt", "content": "through"}); strings.Contains(out, `"status":"ok"`) {
		t.Fatalf("a symlink carried a write out of the allowed paths: %q", out)
	}
	if err := os.Symlink(filepath.Join(root, "docs"), filepath.Join(root, "src", "dir")); err != nil {
		t.Fatal(err)
	}
	if out := scopeRun(t, write, root, scope, map[string]any{"path": "src/dir/e.txt", "content": "through"}); strings.Contains(out, `"status":"ok"`) || pathExists(filepath.Join(root, "docs", "e.txt")) {
		t.Fatalf("a symlinked directory carried a write out of the allowed paths: %q", out)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "docs", "d.txt")); string(data) != "old" {
		t.Fatalf("docs/d.txt changed through the link to %q", data)
	}
}

func TestPathScopeNilChangesNothingAndNeverWidens(t *testing.T) {
	root := scopeWorkspace(t)
	settings := testWriteSettings(root, 1<<20)
	tool, _ := NewWriteTool(settings, NewPathLocks())
	assertMutationStatus(t, scopeRun(t, tool, root, nil, map[string]any{"path": "docs/free.txt", "content": "ok"}), "ok")

	// The session's own deny rule still holds under a scope that allows the path.
	settings.Policy.Paths = append(settings.Policy.Paths, sessionfsDeny(filepath.Join(root, "docs")))
	tool, _ = NewWriteTool(settings, NewPathLocks())
	scope := &tools.PathScope{Allow: []string{"docs", "src"}}
	if out := scopeRun(t, tool, root, scope, map[string]any{"path": "docs/x.txt", "content": "no"}); !strings.Contains(out, "path_denied") {
		t.Fatalf("the session deny rule was lifted by the agent's allow: %q", out)
	}
	assertMutationStatus(t, scopeRun(t, tool, root, scope, map[string]any{"path": "src/x.txt", "content": "ok"}), "ok")

	// The scope is per call: the tool's own policy is not changed for the next call.
	assertMutationStatus(t, scopeRun(t, tool, root, nil, map[string]any{"path": "top.txt", "content": "ok"}), "ok")
}

func sessionfsDeny(path string) sessionfs.NativePathRule {
	return sessionfs.NativePathRule{Action: "deny", Path: path}
}

// The workspace root may itself be reached through a symlink. The path checks compare
// resolved paths, so an allow entry taken from the unresolved root must be resolved too.
func TestPathScopeWorksWhenTheWorkspaceRootIsASymlink(t *testing.T) {
	real := scopeWorkspace(t)
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	tool, _ := NewWriteTool(testWriteSettings(real, 1<<20), NewPathLocks())
	scope := &tools.PathScope{Allow: []string{"src"}, Deny: []string{"src/secret"}}
	assertMutationStatus(t, scopeRun(t, tool, link, scope, map[string]any{"path": "src/a.txt", "content": "ok"}), "ok")
	if out := scopeRun(t, tool, link, scope, map[string]any{"path": "docs/a.txt", "content": "no"}); !strings.Contains(out, "path_outside_agent_paths") {
		t.Fatalf("outside allow through a symlinked root: %q", out)
	}
	if out := scopeRun(t, tool, link, scope, map[string]any{"path": "src/secret/a.txt", "content": "no"}); !strings.Contains(out, "path_denied") {
		t.Fatalf("deny through a symlinked root: %q", out)
	}
	if pathExists(filepath.Join(real, "docs", "a.txt")) || pathExists(filepath.Join(real, "src", "secret", "a.txt")) {
		t.Fatal("a refused write reached the disk")
	}
}

func TestResolveExistingTargetDeniesRuleThroughSymlink(t *testing.T) {
	real := scopeWorkspace(t)
	if err := os.WriteFile(filepath.Join(real, "src", "secret", "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	policy := sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Join(link, "src", "secret")}}}
	if _, denied := resolveExistingTarget(filepath.Join(real, "src", "secret", "a.txt"), real, policy, "c"); !strings.Contains(denied, "path_denied") {
		t.Fatalf("a deny rule through a symlink did not refuse the real path: %q", denied)
	}
}

// A deny rule written through a symlink must also stop writes and edits into the real directory.
func TestDenyRuleThroughSymlinkRefusesWritesAndEdits(t *testing.T) {
	real := scopeWorkspace(t)
	if err := os.WriteFile(filepath.Join(real, "src", "secret", "a.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	rule := sessionfs.NativePathRule{Action: "deny", Path: filepath.Join(link, "src", "secret")}

	writeSettings := testWriteSettings(real, 1<<20)
	writeSettings.Policy.Paths = []sessionfs.NativePathRule{rule}
	write, err := NewWriteTool(writeSettings, NewPathLocks())
	if err != nil {
		t.Fatal(err)
	}
	if out := scopeRun(t, write, real, nil, map[string]any{"path": "src/secret/new.txt", "content": "no"}); !strings.Contains(out, "path_denied") || pathExists(filepath.Join(real, "src", "secret", "new.txt")) {
		t.Fatalf("write under a symlinked deny rule: %q", out)
	}

	editSettings := testEditSettings(real, 1<<20)
	editSettings.Policy.Paths = []sessionfs.NativePathRule{rule}
	edit, err := NewEditTool(editSettings, NewPathLocks())
	if err != nil {
		t.Fatal(err)
	}
	if out := scopeRun(t, edit, real, nil, map[string]any{"path": "src/secret/a.txt", "oldText": "old", "newText": "new"}); !strings.Contains(out, "path_denied") {
		t.Fatalf("edit under a symlinked deny rule: %q", out)
	}
	if data, _ := os.ReadFile(filepath.Join(real, "src", "secret", "a.txt")); string(data) != "old" {
		t.Fatalf("a refused edit changed the file to %q", data)
	}
}

// A cwd under a symlinked deny rule is refused before the bash command runs.
func TestDenyRuleThroughSymlinkRefusesBashCWD(t *testing.T) {
	real := scopeWorkspace(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	settings := bashTestSettings(t, real)
	settings.Policy.Paths = []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Join(link, "src", "secret")}}
	if out := executeBashTest(t, real, "bash-cwd", "pwd", "src/secret", settings); !strings.Contains(out, "path_denied") {
		t.Fatalf("bash cwd under a symlinked deny rule: %s", out)
	}
}

// A cwd under a symlinked deny rule is refused before the managed process starts.
func TestDenyRuleThroughSymlinkRefusesProcessCWD(t *testing.T) {
	real := scopeWorkspace(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	manager := NewProcessManager(filepath.Join(real, "processes"))
	t.Cleanup(func() { _ = manager.Close() })
	settings := processTestRequest(real, "", 1<<20).Policy
	settings.StorageDir = filepath.Join(real, "processes")
	settings.Policy.Paths = []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Join(link, "src", "secret")}}
	process, err := NewProcessToolWithAsync(settings, manager, "KEY", nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := process.Execute(context.Background(), map[string]any{"command": "pwd", "cwd": "src/secret", "block_ms": json.Number("0")}, tools.ExecutionContext{CallID: "p-cwd", WorkspaceRoot: real})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "path_denied") {
		t.Fatalf("process cwd under a symlinked deny rule: %s", out)
	}
}
