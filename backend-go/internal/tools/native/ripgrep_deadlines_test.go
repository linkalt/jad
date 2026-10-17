package native

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func fakeRipgrep(t *testing.T, script string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell fixture")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not installed")
	}
	directory := t.TempDir()
	binary := filepath.Join(directory, "fake-rg")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, directory
}

// A descendant in its own session survives the group kill and keeps the pipes open; the
// settings' drain deadline is how long the call waits for it after ripgrep exited.
func TestRunRipgrepDrainDeadlineIsTheSettingsOwn(t *testing.T) {
	binary, directory := fakeRipgrep(t, "setsid sleep 3 &\nsleep 0.3\nexit 1\n")
	run := func(drainMs int64) time.Duration {
		settings := nativeGrepSettings(binary)
		settings.DrainAfterExitMs = drainMs
		started := time.Now()
		runRipgrep(context.Background(), settings, nil, "unused", []string{"."}, directory)
		return time.Since(started)
	}
	if short := run(100); short >= 600*time.Millisecond {
		t.Fatalf("drain 100ms returned after %v, want well under the 500ms default", short)
	}
	if long := run(900); long < 900*time.Millisecond {
		t.Fatalf("drain 900ms returned after %v, want at least 900ms", long)
	}
}

// TERM is ignored, so only the SIGKILL ends the call: the settings' kill grace is the wait.
func TestRunRipgrepKillGraceIsTheSettingsOwn(t *testing.T) {
	binary, directory := fakeRipgrep(t, "trap '' TERM\nsleep 10\n")
	run := func(graceMs int64) time.Duration {
		settings := nativeGrepSettings(binary)
		settings.TimeoutMs = 50
		settings.KillGraceMs = graceMs
		settings.KillDeadlineMs = 30_000
		started := time.Now()
		runRipgrep(context.Background(), settings, nil, "unused", []string{"."}, directory)
		return time.Since(started)
	}
	if short := run(100); short >= 230*time.Millisecond+500*time.Millisecond {
		t.Fatalf("kill grace 100ms returned after %v, want well under 1s", short)
	}
	if long := run(900); long < 900*time.Millisecond {
		t.Fatalf("kill grace 900ms returned after %v, want at least 900ms", long)
	}
}

// A descendant in its own session survives the group kill; the settings' kill deadline is how
// long the call waits for it after a stop request.
func TestRunRipgrepKillDeadlineIsTheSettingsOwn(t *testing.T) {
	binary, directory := fakeRipgrep(t, "setsid sleep 3 &\nsleep 10\n")
	run := func(deadlineMs int64) time.Duration {
		settings := nativeGrepSettings(binary)
		settings.TimeoutMs = 50
		settings.KillGraceMs = 50
		settings.KillDeadlineMs = deadlineMs
		settings.DrainAfterExitMs = 30_000
		started := time.Now()
		runRipgrep(context.Background(), settings, nil, "unused", []string{"."}, directory)
		return time.Since(started)
	}
	if short := run(150); short >= 900*time.Millisecond {
		t.Fatalf("kill deadline 150ms returned after %v, want well under 1s", short)
	}
	if long := run(1_500); long < 1_500*time.Millisecond {
		t.Fatalf("kill deadline 1500ms returned after %v, want at least 1.5s", long)
	}
}

// Without settings values the call keeps the old 250 ms grace, 1 s kill deadline and 500 ms drain.
func TestRunRipgrepZeroSettingsTakeTheDefaults(t *testing.T) {
	binary, directory := fakeRipgrep(t, "setsid sleep 3 &\nsleep 0.3\nexit 1\n")
	settings := nativeGrepSettings(binary)
	started := time.Now()
	runRipgrep(context.Background(), settings, nil, "unused", []string{"."}, directory)
	if elapsed := time.Since(started); elapsed < ripgrepDrainDeadline {
		t.Fatalf("zero DrainAfterExitMs returned after %v, want at least %v", elapsed, ripgrepDrainDeadline)
	}
}
