package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// newTwoProfileLockFixture builds one session with two path profiles that keep
// separate transcripts: green (active) and blue.
func newTwoProfileLockFixture(t *testing.T) *Resolver {
	t.Helper()
	workspace := t.TempDir()
	rootDir := filepath.Join(workspace, "sessions")
	sessionDir := filepath.Join(rootDir, "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setting := fmt.Sprintf(`{"cwd":%q,"paths":{"active_profile":"green","profiles":{"green":{"models":["model.json"]},"blue":{"models":["model.json"]}}}}`, workspace)
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), []byte(`{"provider":"openai","model":"gpt-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := []sessionfs.Root{{ID: "primary", Label: "Primary", Directory: rootDir}}
	resolver, err := NewResolver(roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func lockWithin(t *testing.T, resolver *Resolver, profile string, wait time.Duration) (func(), error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return resolver.LockSession(ctx, "session-1", profile, "")
}

// A run holds LockSession for its whole length. Another profile of the same
// session has its own transcript and storage, so it must be able to start.
func TestLockSessionOfOneProfileDoesNotBlockAnotherProfile(t *testing.T) {
	resolver := newTwoProfileLockFixture(t)
	releaseGreen, err := lockWithin(t, resolver, "green", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseGreen()

	start := time.Now()
	releaseBlue, err := lockWithin(t, resolver, "blue", time.Second)
	if err != nil {
		t.Fatalf("blue could not start while green runs (waited %v): %v", time.Since(start), err)
	}
	releaseBlue()
}

// The transcript lock is what keeps two runs of one profile from interleaving
// writes; moving the settings lock out of the run must not weaken it.
func TestLockSessionStillSerializesTwoRunsOfTheSameProfile(t *testing.T) {
	resolver := newTwoProfileLockFixture(t)
	releaseFirst, err := lockWithin(t, resolver, "blue", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if release, err := lockWithin(t, resolver, "blue", 300*time.Millisecond); err == nil {
		release()
		t.Fatal("a second run of the same profile got the transcript lock while the first holds it")
	}
	releaseFirst()
	release, err := lockWithin(t, resolver, "blue", time.Second)
	if err != nil {
		t.Fatalf("the transcript lock was not released with the run: %v", err)
	}
	release()
}

// Settings writes (profile switch, model default) take LockSessionSettings.
// They only need to wait for a run to read its paths, not for the run to end.
func TestSettingsLockIsFreeWhileARunIsInProgress(t *testing.T) {
	resolver := newTwoProfileLockFixture(t)
	releaseRun, err := lockWithin(t, resolver, "blue", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRun()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	releaseSettings, err := resolver.LockSessionSettings(ctx, "session-1")
	if err != nil {
		t.Fatalf("the settings lock is still held by the running profile: %v", err)
	}
	releaseSettings()
}

// Switching the active profile while the other one runs pins the storage of
// both, so the running profile keeps its transcript and a new run on it still
// waits for the running one.
func TestSwitchingProfilesWhileBlueRunsKeepsBlueTranscript(t *testing.T) {
	resolver := newTwoProfileLockFixture(t)
	releaseBlue, err := lockWithin(t, resolver, "blue", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBlue()

	if _, err := resolver.UpdatePathProfile(context.Background(), "session-1", "blue"); err != nil {
		t.Fatalf("switching the active profile to the running one failed: %v", err)
	}
	if release, err := lockWithin(t, resolver, "blue", 300*time.Millisecond); err == nil {
		release()
		t.Fatal("after the switch, blue resolved to a different transcript than the running blue")
	}
	// green, now not active, must not share blue's transcript either.
	releaseGreen, err := lockWithin(t, resolver, "green", time.Second)
	if err != nil {
		t.Fatalf("green could not start after the switch: %v", err)
	}
	releaseGreen()
}
