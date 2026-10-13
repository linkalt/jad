package app

import (
	"context"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// newTwoProfileLockFixture builds one session with two path profiles that keep
// separate transcripts: green (active) and blue.
func newTwoProfileLockFixture(t *testing.T) *Resolver {
	t.Helper()
	config, _ := layoutConfig(t, lockSpec(), map[string]string{})
	resolver, err := NewResolver(config.Roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

// lockSpec is the layout session the lock tests share: session-1 with profiles dev/green
// (active) and dev/blue, both offering model and model2.
func lockSpec() layouttest.Spec {
	return layouttest.Spec{
		SessionID: "session-1",
		Models:    map[string]map[string]any{"openai/model": {"provider": "openai", "model": "gpt-test"}, "openai/model2": {"provider": "openai", "model": "gpt-test"}},
		Profiles: map[string]map[string]any{
			"dev/green": {"models": []any{"openai/model", "openai/model2"}, "model_default": "openai/model"},
			"dev/blue":  {"models": []any{"openai/model", "openai/model2"}, "model_default": "openai/model"},
		},
		ActiveProfile: "dev/green",
	}
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
	releaseGreen, err := lockWithin(t, resolver, "dev/green", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseGreen()

	start := time.Now()
	releaseBlue, err := lockWithin(t, resolver, "dev/blue", time.Second)
	if err != nil {
		t.Fatalf("blue could not start while green runs (waited %v): %v", time.Since(start), err)
	}
	releaseBlue()
}

// The transcript lock is what keeps two runs of one profile from interleaving
// writes; moving the settings lock out of the run must not weaken it.
func TestLockSessionStillSerializesTwoRunsOfTheSameProfile(t *testing.T) {
	resolver := newTwoProfileLockFixture(t)
	releaseFirst, err := lockWithin(t, resolver, "dev/blue", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if release, err := lockWithin(t, resolver, "dev/blue", 300*time.Millisecond); err == nil {
		release()
		t.Fatal("a second run of the same profile got the transcript lock while the first holds it")
	}
	releaseFirst()
	release, err := lockWithin(t, resolver, "dev/blue", time.Second)
	if err != nil {
		t.Fatalf("the transcript lock was not released with the run: %v", err)
	}
	release()
}

// Settings writes (profile switch, model default) take LockSessionSettings.
// They only need to wait for a run to read its paths, not for the run to end.
func TestSettingsLockIsFreeWhileARunIsInProgress(t *testing.T) {
	resolver := newTwoProfileLockFixture(t)
	releaseRun, err := lockWithin(t, resolver, "dev/blue", time.Second)
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
	releaseBlue, err := lockWithin(t, resolver, "dev/blue", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBlue()

	if _, err := resolver.UpdateProfile(context.Background(), "session-1", "dev/blue"); err != nil {
		t.Fatalf("switching the active profile to the running one failed: %v", err)
	}
	if release, err := lockWithin(t, resolver, "dev/blue", 300*time.Millisecond); err == nil {
		release()
		t.Fatal("after the switch, blue resolved to a different transcript than the running blue")
	}
	// green, now not active, must not share blue's transcript either.
	releaseGreen, err := lockWithin(t, resolver, "dev/green", time.Second)
	if err != nil {
		t.Fatalf("green could not start after the switch: %v", err)
	}
	releaseGreen()
}
