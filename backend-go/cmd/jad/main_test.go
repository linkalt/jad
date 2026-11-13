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

func useFixture(t *testing.T, spec layouttest.Spec) layouttest.Fixture {
	t.Helper()
	fixture := layouttest.Write(t, spec)
	t.Chdir(fixture.Workspace)
	t.Setenv("JAD_GLOBAL_DIR", fixture.GlobalDir)
	t.Setenv("JAD_API_TOKEN", "")
	t.Setenv("AGENT_API_TOKEN", "")
	return fixture
}

func TestCheckCommandNeedsNoTokenAndAcceptsValidFiles(t *testing.T) {
	useFixture(t, layouttest.Spec{})
	var out bytes.Buffer
	if err := runCommand([]string{"check"}, &out); err != nil {
		t.Fatalf("runCommand: %v\n%s", err, out.String())
	}
	if !strings.HasSuffix(out.String(), " files, 0 problems\n") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestCheckCommandFailsWithTheSentinelWhenAFileIsInvalid(t *testing.T) {
	fixture := useFixture(t, layouttest.Spec{})
	model := filepath.Join(fixture.GlobalDir, "models", "openai", "default.json")
	if err := os.WriteFile(model, []byte(`{"provider":"openai","model":"m","base_url":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runCommand([]string{"check"}, &out)
	if !errors.Is(err, errCheckFailed) {
		t.Fatalf("error = %v, want errCheckFailed", err)
	}
	if !strings.Contains(out.String(), "model openai/default:") || !strings.Contains(out.String(), "1 problems") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestCheckCommandRefusesOtherArguments(t *testing.T) {
	useFixture(t, layouttest.Spec{})
	for _, args := range [][]string{{"bogus"}, {"check", "extra"}, {"--check"}} {
		var out bytes.Buffer
		err := runCommand(args, &out)
		if err == nil || errors.Is(err, errCheckFailed) || !strings.Contains(err.Error(), "unknown arguments") {
			t.Errorf("runCommand(%q) error = %v, want an unknown-arguments error", args, err)
		}
		if out.Len() != 0 {
			t.Errorf("runCommand(%q) wrote %q", args, out.String())
		}
	}
}

func TestCheckCommandFailsWithoutAWorkspace(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("JAD_GLOBAL_DIR", t.TempDir())
	var out bytes.Buffer
	err := runCommand([]string{"check"}, &out)
	if err == nil || errors.Is(err, errCheckFailed) || !strings.Contains(err.Error(), ".jad/workspace.json") {
		t.Fatalf("error = %v, want the no-workspace error naming .jad/workspace.json", err)
	}
}

// T-041: a directory that still has a v1 setting.json fails with the way to migrate it, and the
// file is left as it was.
func TestCheckCommandRefusesAV1SettingWithTheMigrationDirection(t *testing.T) {
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".jad"), 0o755); err != nil {
		t.Fatal(err)
	}
	setting := filepath.Join(project, ".jad", "setting.json")
	const content = `{"session_roots":[{"id":"local","path":"sessions"}]}`
	if err := os.WriteFile(setting, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	t.Setenv("JAD_GLOBAL_DIR", t.TempDir())
	var out bytes.Buffer
	err := runCommand([]string{"check"}, &out)
	if err == nil || errors.Is(err, errCheckFailed) || !strings.Contains(err.Error(), "6ac57497") || !strings.Contains(err.Error(), "jad migrate workspace") {
		t.Fatalf("error = %v, want the migration direction", err)
	}
	if got, readErr := os.ReadFile(setting); readErr != nil || string(got) != content {
		t.Fatalf("the v1 setting changed: %q, %v", got, readErr)
	}
}

// T-041c: an unknown command lists the commands that exist; migrate is not one of them (its own
// notice names the build that still has it), and the notice is the same with or without a workspace.
func TestUnknownCommandListsOnlyLiveCommands(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("JAD_GLOBAL_DIR", t.TempDir())
	var out bytes.Buffer
	err := runCommand([]string{"--help"}, &out)
	if err == nil || !strings.Contains(err.Error(), `"transcript"`) || strings.Contains(err.Error(), "migrate") {
		t.Fatalf("error = %v, want the live commands without migrate", err)
	}
	err = runCommand([]string{"migrate", "workspace"}, &out)
	if err == nil || !strings.Contains(err.Error(), "6ac57497") {
		t.Fatalf("error = %v, want the migration notice", err)
	}
}
