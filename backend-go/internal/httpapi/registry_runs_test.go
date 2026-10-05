package httpapi

import (
	"testing"
	"time"
)

// TestSessionRunsReportsProfileLessRunOnlyAsDefault mirrors the TypeScript
// describeAll: a run without a named profile is the session's `run`, while
// runs_by_profile lists only runs of named profiles.
func TestSessionRunsReportsProfileLessRunOnlyAsDefault(t *testing.T) {
	registry := newRunRegistry(time.Minute, 4096)
	if _, err := registry.begin("alpha\x00", "turn-plain"); err != nil {
		t.Fatalf("begin profile-less run: %v", err)
	}
	if _, err := registry.begin("alpha\x00green", "turn-green"); err != nil {
		t.Fatalf("begin named-profile run: %v", err)
	}

	summaries := registry.SessionRuns("alpha", "")
	if summaries.Default == nil || summaries.Default.TurnID != "turn-plain" {
		t.Fatalf("default run = %+v, want turn-plain", summaries.Default)
	}
	if len(summaries.ByProfile) != 1 {
		t.Fatalf("runs by profile = %+v, want only the named profile", summaries.ByProfile)
	}
	if green, ok := summaries.ByProfile["green"]; !ok || green.TurnID != "turn-green" {
		t.Fatalf("green run = %+v, ok=%v", green, ok)
	}

	named := registry.SessionRuns("alpha", "green")
	if named.Default == nil || named.Default.TurnID != "turn-green" {
		t.Fatalf("default run for stored profile green = %+v", named.Default)
	}
}
