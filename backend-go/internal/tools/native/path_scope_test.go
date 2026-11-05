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

func sessionfsDeny(path string) sessionfs.NativePathRule {
	return sessionfs.NativePathRule{Action: "deny", Path: path}
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
