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
	t.Setenv("JAD_ROOT_SETTING_PATH", fixture.RootSetting)
	t.Setenv("JAD_SESSIONS_DIR", "")
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
	model := filepath.Join(filepath.Dir(fixture.RootSetting), "models", "openai", "default.json")
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

func TestCheckCommandFailsWithoutTheCatalogLayout(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("JAD_ROOT_SETTING_PATH", "")
	t.Setenv("JAD_SESSIONS_DIR", "")
	var out bytes.Buffer
	err := runCommand([]string{"check"}, &out)
	if err == nil || errors.Is(err, errCheckFailed) || !strings.Contains(err.Error(), "catalog layout") {
		t.Fatalf("error = %v, want the no-layout error", err)
	}
}
