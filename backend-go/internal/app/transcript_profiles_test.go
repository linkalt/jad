package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestTranscriptProfilesFindsOwnAndOtherProfilesWithoutCreatingFiles(t *testing.T) {
	f := newLayoutFixture(t)
	messenger := profileMessenger{resolver: f.service.resolver}
	state := filepath.Join(f.cwd, ".jad", "state", "demo")

	other, err := messenger.Profile(context.Background(), profileMessageOwner, "", "review/reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if other.Profile != "review/reviewer" || other.Files.Messages != filepath.Join(state, "review", "reviewer", "messages.jsonl") {
		t.Fatalf("other = %+v", other)
	}
	own, err := messenger.Profile(context.Background(), profileMessageOwner, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if own.Profile != "dev/coder" || own.Session != other.Session {
		t.Fatalf("own = %+v, other = %+v", own, other)
	}
	if _, err := messenger.Profile(context.Background(), profileMessageOwner, "", "bare"); err == nil {
		t.Fatal("a profile that is not <category>/<name> must be refused")
	}
	if _, err := messenger.Profile(context.Background(), profileMessageOwner, "no-such-session", "review/reviewer"); err == nil {
		t.Fatal("an unknown session must be refused")
	}
	if _, err := messenger.Profile(context.Background(), "local:demo", "", "review/reviewer"); err == nil {
		t.Fatal("an owner key without a separator must be refused")
	}
	if _, err := os.Stat(other.Files.Messages); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("looking a transcript up must not create it: %v", err)
	}
}

func TestTranscriptProfilesReportTheSubagentStorageOfTheProfile(t *testing.T) {
	f := newLayoutFixture(t)
	messenger := profileMessenger{resolver: f.service.resolver}
	plain, err := messenger.Profile(context.Background(), profileMessageOwner, "", "review/reviewer")
	if err != nil || plain.SubagentDir != "" || plain.Runs != nil {
		t.Fatalf("a profile without the subagent tool has no storage: %+v, %v", plain, err)
	}
	// The profile files are read again on every lookup, so the toolset can change after the start.
	subagent := []byte(`{"native":{"async":{},"subagent":{"agents":{"explore":{}}}},"mcp":{}}`)
	if err := os.WriteFile(filepath.Join(f.root, "tools", "coding", "none.json"), subagent, 0o600); err != nil {
		t.Fatal(err)
	}
	other, err := messenger.Profile(context.Background(), profileMessageOwner, "", "review/reviewer")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(f.cwd, ".jad", "state", "demo", "review", "reviewer", "tools", ".subagents")
	if other.SubagentDir != want {
		t.Fatalf("SubagentDir = %q, want %q", other.SubagentDir, want)
	}
	if other.Runs != nil {
		t.Fatal("a lookup must not build a manager")
	}
}

func TestTranscriptProfilesCandidatesListProfilesOfTheSessionOrTheSessions(t *testing.T) {
	f := newLayoutFixture(t)
	messenger := profileMessenger{resolver: f.service.resolver}
	profiles, sessions := messenger.Candidates(context.Background(), profileMessageOwner, "")
	if sessions != nil || !slices.Contains(profiles, "review/reviewer") || !slices.Contains(profiles, "dev/coder") {
		t.Fatalf("profiles = %v, sessions = %v", profiles, sessions)
	}
	profiles, sessions = messenger.Candidates(context.Background(), profileMessageOwner, "no-such-session")
	if profiles != nil || !slices.Contains(sessions, "demo") {
		t.Fatalf("profiles = %v, sessions = %v", profiles, sessions)
	}
}
