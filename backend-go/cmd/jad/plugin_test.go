package main

import (
	"bytes"
	"errors"
	"os"
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
		"tools/review.json":          `{"schema_version":1,"native":{"glob":{}}}`,
		"skills/acme-notes/SKILL.md": "---\nname: acme-notes\ndescription: Notes.\n---\nBody.\n",
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
	if err != nil || !strings.HasPrefix(out, "Installed acme 1.0.0: 2 parts\n") || !strings.Contains(out, "toolset  acme/review") || !strings.Contains(out, "skill    acme-notes") {
		t.Fatalf("install = %q, %v", out, err)
	}
	// With no libraries.skill_dirs, the first skill folder is the default <root>/.agent/skills.
	if _, err := os.Stat(filepath.Join(filepath.Dir(fixture.RootSetting), ".agent", "skills", "acme-notes", "SKILL.md")); err != nil {
		t.Fatalf("skill not in the first skill folder: %v", err)
	}
	if out, err := run("plugin", "list"); err != nil || !strings.HasPrefix(out, "acme 1.0.0: 2 parts (from ") {
		t.Fatalf("list = %q, %v", out, err)
	}
	if out, err := run("plugin", "install", source); err != nil || !strings.HasPrefix(out, "Upgraded acme 1.0.0") {
		t.Fatalf("reinstall = %q, %v", out, err)
	}
	if out, err := run("plugin", "remove", "acme"); err != nil || out != "Removed acme 1.0.0: 2 parts\n" {
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
	} {
		_, err := run(test.args...)
		var exit *exitError
		if !errors.As(err, &exit) || exit.code != test.code || exit.err == nil || !strings.Contains(exit.err.Error(), test.want) {
			t.Errorf("%v: %v, want exit %d with %q", test.args, err, test.code, test.want)
		}
	}
}
