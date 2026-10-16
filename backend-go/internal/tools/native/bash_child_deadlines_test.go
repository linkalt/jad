package native

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// A descendant that outlives the shell keeps the output pipes open; drainAfterExit is how long
// the call waits for it before it cuts the output.
func TestRunBashCommandDrainAfterExitIsTheRequestsOwn(t *testing.T) {
	run := func(drain time.Duration) time.Duration {
		started := time.Now()
		child := runBashCommand(context.Background(), bashChildRequest{
			file:           "/bin/sh",
			args:           []string{"-c", "sleep 1 & echo hi"},
			cwd:            t.TempDir(),
			environment:    []string{"PATH=/usr/bin:/bin"},
			drainAfterExit: drain,
		})
		if child.spawnError != nil {
			t.Skip("cannot start /bin/sh: " + child.spawnError.Error())
		}
		return time.Since(started)
	}
	short := run(60 * time.Millisecond)
	if short >= 400*time.Millisecond {
		t.Fatalf("drainAfterExit 60ms returned after %v, want well under the 500ms default", short)
	}
	long := run(700 * time.Millisecond)
	if long < 700*time.Millisecond {
		t.Fatalf("drainAfterExit 700ms returned after %v, want at least 700ms", long)
	}
}

func TestRunBashCommandZeroDeadlinesTakeTheDefaults(t *testing.T) {
	started := time.Now()
	child := runBashCommand(context.Background(), bashChildRequest{
		file:        "/bin/sh",
		args:        []string{"-c", "sleep 1 & echo hi"},
		cwd:         t.TempDir(),
		environment: []string{"PATH=/usr/bin:/bin"},
	})
	if child.spawnError != nil {
		t.Skip("cannot start /bin/sh: " + child.spawnError.Error())
	}
	if elapsed := time.Since(started); elapsed < bashDrainAfterExit {
		t.Fatalf("a request without drainAfterExit returned after %v, want at least %v", elapsed, bashDrainAfterExit)
	}
}

// A descendant in its own session survives the group kill and keeps the output pipes open;
// killDeadline is how long the call waits for it after SIGKILL before it returns anyway.
func TestRunBashCommandKillDeadlineIsTheRequestsOwn(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not installed")
	}
	run := func(deadline time.Duration) (time.Duration, bool) {
		started := time.Now()
		child := runBashCommand(context.Background(), bashChildRequest{
			file:         "/bin/sh",
			args:         []string{"-c", "setsid sleep 3 & sleep 10"},
			cwd:          t.TempDir(),
			environment:  []string{"PATH=/usr/bin:/bin"},
			timeout:      50 * time.Millisecond,
			killGrace:    50 * time.Millisecond,
			killDeadline: deadline,
			// Longer than any killDeadline here, so the drain timer cannot end the call first.
			drainAfterExit: 30 * time.Second,
		})
		if child.spawnError != nil {
			t.Skip("cannot start /bin/sh: " + child.spawnError.Error())
		}
		return time.Since(started), child.leftRunning
	}
	short, leftShort := run(150 * time.Millisecond)
	if !leftShort || short >= time.Second {
		t.Fatalf("killDeadline 150ms: leftRunning=%v after %v, want true and well under 1s", leftShort, short)
	}
	long, leftLong := run(900 * time.Millisecond)
	if !leftLong || long < 900*time.Millisecond {
		t.Fatalf("killDeadline 900ms: leftRunning=%v after %v, want true and at least 900ms", leftLong, long)
	}
}

// Without a request value the call keeps the old 2 s wait.
func TestRunBashCommandZeroKillDeadlineTakesTheDefault(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not installed")
	}
	started := time.Now()
	child := runBashCommand(context.Background(), bashChildRequest{
		file:        "/bin/sh",
		args:        []string{"-c", "setsid sleep 4 & sleep 10"},
		cwd:         t.TempDir(),
		environment: []string{"PATH=/usr/bin:/bin"},
		timeout:     50 * time.Millisecond,
		killGrace:   50 * time.Millisecond,
		// Longer than the default kill deadline, so the drain timer cannot end the call first.
		drainAfterExit: 30 * time.Second,
	})
	if child.spawnError != nil {
		t.Skip("cannot start /bin/sh: " + child.spawnError.Error())
	}
	if elapsed := time.Since(started); !child.leftRunning || elapsed < bashKillDeadline {
		t.Fatalf("leftRunning=%v after %v, want true and at least %v", child.leftRunning, elapsed, bashKillDeadline)
	}
}
