package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestPluginCommandInstallsListsAndRemoves(t *testing.T) {
	fixture := useFixture(t, layouttest.Spec{})
	source := t.TempDir()
	for path, contents := range map[string]string{
		"plugin.json":                `{"schema_version":1,"name":"acme","version":"1.0.0"}`,
		"tools/review.json":          `{"schema_version":1,"native":{"read_image":{}}}`,
		"skills/acme-notes/SKILL.md": "---\nname: acme-notes\ndescription: Notes.\n---\nBody.\n",
		"commands/review.md":         "Review $ARGUMENTS.\n",
	} {
		full := filepath.Join(source, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := dispatchCommand(args, commandStreams{stdout: &out, stderr: &out})
		return out.String(), err
	}
	if out, err := run("plugin", "list"); err != nil || out != "No plugins are installed.\n" {
		t.Fatalf("list before install = %q, %v", out, err)
	}
	out, err := run("plugin", "install", source)
	if err != nil || !strings.HasPrefix(out, "Installed acme 1.0.0: 3 parts\n") || !strings.Contains(out, "toolset  acme/review") || !strings.Contains(out, "skill    acme-notes") || !strings.Contains(out, "commands acme") {
		t.Fatalf("install = %q, %v", out, err)
	}
	// With no libraries.skill_dirs, the first skill folder is the default <root>/.agent/skills.
	if _, err := os.Stat(filepath.Join(filepath.Dir(fixture.RootSetting), ".agent", "skills", "acme-notes", "SKILL.md")); err != nil {
		t.Fatalf("skill not in the first skill folder: %v", err)
	}
	// And the first command folder is the default <root>/.agent/commands.
	if _, err := os.Stat(filepath.Join(filepath.Dir(fixture.RootSetting), ".agent", "commands", "acme", "review.md")); err != nil {
		t.Fatalf("commands not in the first command folder: %v", err)
	}
	if out, err := run("plugin", "list"); err != nil || !strings.HasPrefix(out, "acme 1.0.0: 3 parts (from ") {
		t.Fatalf("list = %q, %v", out, err)
	}
	if out, err := run("plugin", "install", source); err != nil || !strings.HasPrefix(out, "Upgraded acme 1.0.0") {
		t.Fatalf("reinstall = %q, %v", out, err)
	}
	if out, err := run("plugin", "remove", "acme"); err != nil || out != "Removed acme 1.0.0: 3 parts\n" {
		t.Fatalf("remove = %q, %v", out, err)
	}
	for _, test := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"plugin"}, exitUsage, "usage: jad plugin install"},
		{[]string{"plugin", "add", source}, exitUsage, "usage: jad plugin install"},
		{[]string{"plugin", "install"}, exitUsage, "usage: jad plugin install"},
		{[]string{"plugin", "list", "x"}, exitUsage, "usage: jad plugin install"},
		{[]string{"plugin", "remove", "acme"}, exitFailed, "plugin acme is not installed"},
		{[]string{"plugin", "install", t.TempDir()}, exitFailed, "plugin.json"},
		{[]string{"plugin", "install", "--ref", "v1", source}, exitUsage, "--ref and --subdir go with a git URL"},
		{[]string{"plugin", "install", "--timeout", "-1s", "file:///nonexistent/r"}, exitUsage, "--timeout cannot be negative"},
		{[]string{"plugin", "remove", "--ref", "v1", "acme"}, exitUsage, "flag provided but not defined: -ref"},
	} {
		_, err := run(test.args...)
		var exit *exitError
		if !errors.As(err, &exit) || exit.code != test.code || exit.err == nil || !strings.Contains(exit.err.Error(), test.want) {
			t.Errorf("%v: %v, want exit %d with %q", test.args, err, test.code, test.want)
		}
	}
}

// A git URL is cloned and installed; the list shows where it came from.
func TestPluginCommandInstallsFromAGitURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	useFixture(t, layouttest.Spec{})
	repo := t.TempDir()
	for path, contents := range map[string]string{
		"acme/plugin.json":       `{"schema_version":1,"name":"acme","version":"1.2.0"}`,
		"acme/tools/review.json": `{"schema_version":1,"native":{"read_image":{}}}`,
	} {
		full := filepath.Join(repo, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "one"}, {"tag", "v1.2.0"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	var out bytes.Buffer
	url := "file://" + filepath.ToSlash(repo)
	if err := dispatchCommand([]string{"plugin", "install", "--ref", "v1.2.0", "--subdir", "acme", url}, commandStreams{stdout: &out, stderr: &out}); err != nil || !strings.HasPrefix(out.String(), "Installed acme 1.2.0: 1 parts") {
		t.Fatalf("install = %q, %v", out.String(), err)
	}
	out.Reset()
	if err := dispatchCommand([]string{"plugin", "list"}, commandStreams{stdout: &out, stderr: &out}); err != nil || !strings.Contains(out.String(), "(from "+url+"#v1.2.0 acme @ ") {
		t.Fatalf("list = %q, %v", out.String(), err)
	}
}

// Without --timeout the clone is bounded by cli.plugin_install_timeout_ms of the root
// setting; --timeout still overrides it.
func TestPluginInstallTimeoutComesFromTheRootSetting(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	fixture := useFixture(t, layouttest.Spec{})
	setting, err := os.ReadFile(fixture.RootSetting)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(setting, &fields); err != nil {
		t.Fatal(err)
	}
	fields["cli"] = map[string]any{"plugin_install_timeout_ms": 1}
	if setting, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.RootSetting, setting, 0o644); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	for path, contents := range map[string]string{
		"plugin.json":       `{"schema_version":1,"name":"acme","version":"1.0.0"}`,
		"tools/review.json": `{"schema_version":1,"native":{"read_image":{}}}`,
	} {
		full := filepath.Join(repo, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "one"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	url := "file://" + filepath.ToSlash(repo)
	var out bytes.Buffer
	err = dispatchCommand([]string{"plugin", "install", url}, commandStreams{stdout: &out, stderr: &out})
	if err == nil || !strings.Contains(err.Error(), "did not finish within 1ms") {
		t.Fatalf("install with the 1ms setting = %q, %v", out.String(), err)
	}
	out.Reset()
	if err := dispatchCommand([]string{"plugin", "install", "--timeout", "1m", url}, commandStreams{stdout: &out, stderr: &out}); err != nil || !strings.HasPrefix(out.String(), "Installed acme 1.0.0") {
		t.Fatalf("install with --timeout = %q, %v", out.String(), err)
	}
}
