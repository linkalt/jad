package native

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestSandboxDescriptionNamesWhatTheSandboxAllows(t *testing.T) {
	if got := sandboxDescription(nil); got != "" {
		t.Fatalf("no sandbox adds %q", got)
	}
	sandbox := bwrapSandbox()
	sandbox.HidePaths = []string{"{home}/.ssh"}
	got := sandboxDescription(sandbox)
	for _, want := range []string{"bubblewrap sandbox", "Writable: <workspace>, <repository git directory>, " + os.TempDir(), "Read-only file system", "Hidden (empty): ~/.ssh.", "Network: off"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	sandbox.Network, sandbox.HidePaths = true, nil
	if got := sandboxDescription(sandbox); !strings.Contains(got, "Network: on.") || strings.Contains(got, "Hidden") {
		t.Errorf("network on = %q", got)
	}
	command := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendCommand, Command: []string{"firejail", "--quiet"}}
	if got := sandboxDescription(command); !strings.Contains(got, "runs under firejail,") {
		t.Errorf("command backend = %q", got)
	}
}

// The catalog appends the notice to the manifest's description only when the tool is sandboxed.
func TestSandboxedToolDescriptionInTheCatalog(t *testing.T) {
	config := toolSmithConfig(t)
	catalog, err := BuildCatalog(config)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Definitions()[0].Description; strings.Contains(got, "Sandbox:") {
		t.Fatalf("unsandboxed description mentions a sandbox: %q", got)
	}
	settings := *config.NativeToolSmith
	settings.Child.Sandbox = bwrapSandbox()
	config.NativeToolSmith = &settings
	catalog, err = BuildCatalog(config)
	if err != nil {
		t.Fatal(err)
	}
	got := catalog.Definitions()[0].Description
	if !strings.HasSuffix(got, sandboxDescription(settings.Child.Sandbox)) || !strings.Contains(got, "full interpreter privileges") {
		t.Fatalf("sandboxed description = %q", got)
	}
}

func TestSandboxedHookCannotWriteOutsideTheWorkspace(t *testing.T) {
	requireBwrap(t)
	root, outside, _ := sandboxDirs(t)
	hooks := hookTestSettings()
	hooks.Sandbox = bwrapSandbox()
	script := `touch in.txt && echo wrote-in; touch ` + outside + `/out.txt`
	section := runHookChild(context.Background(), hooks, "", root, hookChild{name: "probe", setting: "hooks.pre_tool", argv: []string{"sh", "-c", script}, cwd: root})
	if !strings.Contains(section, "wrote-in") || !strings.Contains(section, "Read-only file system") {
		t.Fatalf("hook report:\n%s", section)
	}
	if _, err := os.Stat(filepath.Join(outside, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("the hook wrote outside the workspace: %v", err)
	}
	hooks.Sandbox = nil
	section = runHookChild(context.Background(), hooks, "", root, hookChild{name: "probe", setting: "hooks.pre_tool", argv: []string{"sh", "-c", script}, cwd: root})
	if strings.Contains(section, "Read-only file system") {
		t.Fatalf("an unsandboxed hook was limited:\n%s", section)
	}
}

func TestProbeSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell and true")
	}
	dir := t.TempDir()
	if err := ProbeSandbox(context.Background(), nil, dir); err != nil {
		t.Fatalf("no sandbox: %v", err)
	}
	command := func(script string, timeout int64) *sessionfs.SandboxSettings {
		return &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendCommand, Command: []string{"sh", "-c", script, "sandbox"}, CheckTimeoutMs: timeout}
	}
	if err := ProbeSandbox(context.Background(), command(`exec "$@"`, 5000), dir); err != nil {
		t.Fatalf("a working wrapper: %v", err)
	}
	err := ProbeSandbox(context.Background(), command(`echo "no namespaces for you" >&2; exit 1`, 5000), dir)
	if err == nil || !strings.Contains(err.Error(), "every sandboxed call would fail: no namespaces for you") {
		t.Fatalf("a failing wrapper: %v", err)
	}
	err = ProbeSandbox(context.Background(), command(`sleep 5`, 100), dir)
	if err == nil || !strings.Contains(err.Error(), "within 100 ms (setting: sandbox.check_timeout_ms)") {
		t.Fatalf("a hanging wrapper: %v", err)
	}
	missing := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap, Executable: "jad-no-such-bwrap", CheckTimeoutMs: 5000}
	if err := ProbeSandbox(context.Background(), missing, dir); err == nil || !strings.Contains(err.Error(), "cannot be run") {
		t.Fatalf("a missing program: %v", err)
	}
}

func TestProbeSandboxWithBwrap(t *testing.T) {
	requireBwrap(t)
	sandbox := bwrapSandbox()
	sandbox.CheckTimeoutMs = 10000
	if err := ProbeSandbox(context.Background(), sandbox, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
