package app

import (
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestCanonicalSessionProfileNamesTheDefaultForAnEmptyProfile(t *testing.T) {
	resolver := newTwoProfileLockFixture(t)
	t.Cleanup(resolver.Close)

	defaultProfile, err := resolver.CanonicalSessionProfile("session-1", "")
	if err != nil || defaultProfile == "" {
		t.Fatalf("default = %q, %v; want the session's default profile", defaultProfile, err)
	}
	location, err := sessionfs.Resolve(resolver.roots, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := sessionfs.LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if defaultProfile != loaded.Profile {
		t.Fatalf("canonical empty profile = %q, the loader picks %q", defaultProfile, loaded.Profile)
	}
	named, err := resolver.CanonicalSessionProfile("session-1", defaultProfile)
	if err != nil || named != defaultProfile {
		t.Fatalf("explicit default = %q, %v; want %q", named, err, defaultProfile)
	}
	other := "dev/green"
	if defaultProfile == other {
		other = "dev/blue"
	}
	if got, err := resolver.CanonicalSessionProfile("session-1", other); err != nil || got != other {
		t.Fatalf("named profile = %q, %v; want it unchanged", got, err)
	}
	if got, err := resolver.CanonicalSessionProfile("no-such-session", ""); err != nil || got != "" {
		t.Fatalf("a session that does not exist = %q, %v; want the empty profile back", got, err)
	}
}
