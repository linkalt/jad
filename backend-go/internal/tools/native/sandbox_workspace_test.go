package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func writeWorkspaceFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// In the workspace layout a real bwrap sandbox hides G/auth and the other workspace's .jad even
// when the profile's own sandbox sets hide_paths to nothing, and keeps the workspace's
// configuration read-only (docs/design_workspace_layout.md §4.6, design Q4).
func TestWorkspaceSandboxHidesFixedPathsAndKeepsConfigReadOnly(t *testing.T) {
	requireBwrap(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	global := filepath.Join(home, ".jad")
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	writeWorkspaceFile(t, filepath.Join(global, "auth", "key"), "hunter2")
	writeWorkspaceFile(t, filepath.Join(global, "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY"}`)
	writeWorkspaceFile(t, filepath.Join(global, "global.json"), `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}]}`)
	for _, dir := range []string{a, b} {
		writeWorkspaceFile(t, filepath.Join(dir, ".jad", "workspace.json"), `{}`)
		writeWorkspaceFile(t, filepath.Join(dir, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
		writeWorkspaceFile(t, filepath.Join(dir, ".jad", "profiles", "dev", "coder.json"),
			`{"models":["openai/default"],"settings":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"bwrap","hide_paths":[]}}}`)
	}
	writeWorkspaceFile(t, filepath.Join(b, ".jad", "secret"), "abc123")
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: a, Environment: map[string]string{"HOME": home}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := sessionfs.Resolve(loaded.Roots, "a:s1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	settings := bashTestSettings(t, a)
	settings.Sandbox = config.Sandbox
	useOwnTempDir(t)
	command := `echo "auth=[$(cat ` + global + `/auth/key 2>/dev/null)] other=[$(cat ` + b + `/.jad/secret 2>/dev/null)]"; ` +
		`echo changed > .jad/workspace.json 2>&1; echo changed > .jad/sessions/s1.json 2>&1; touch project.txt && echo wrote-project`
	output := executeSandboxBashTest(t, a, "workspace-call", command, "", settings)
	if !strings.Contains(output, "auth=[] other=[]") || strings.Contains(output, "hunter2") || strings.Contains(output, "abc123") {
		t.Fatalf("a fixed hidden path leaked:\n%s", output)
	}
	if !strings.Contains(output, "Read-only file system") || !strings.Contains(output, "wrote-project") {
		t.Fatalf("config must be read-only and the project writable:\n%s", output)
	}
	for _, path := range []string{filepath.Join(a, ".jad", "workspace.json"), filepath.Join(a, ".jad", "sessions", "s1.json")} {
		if data, _ := os.ReadFile(path); strings.Contains(string(data), "changed") {
			t.Fatalf("%s was changed from the sandbox", path)
		}
	}
}
