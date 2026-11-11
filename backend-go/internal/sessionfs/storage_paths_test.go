package sessionfs

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// Two profile IDs can map to one internal storage key ("/" becomes "__"); the layout loader
// refuses the pair, so two profiles never share a messages file.
func TestLoadSessionConfigRejectsProfilesThatShareAStorageKey(t *testing.T) {
	_, err := loadLayoutConfig(t, layouttest.Spec{
		Profiles: map[string]map[string]any{"a/b__c": {}, "a__b/c": {}},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot both be used") {
		t.Fatalf("error = %v, want the shared storage key refused", err)
	}
}

// Every profile of a session gets its own state folder, so no storage path is shared.
func TestLoadSessionConfigGivesEachProfileItsOwnStorage(t *testing.T) {
	spec := layouttest.Spec{Profiles: map[string]map[string]any{"dev/one": {}, "dev/two": {}}}
	location, _ := layoutLocation(t, spec)
	one, err := LoadSessionConfig(location, "dev/one", "")
	if err != nil {
		t.Fatal(err)
	}
	two, err := LoadSessionConfig(location, "dev/two", "")
	if err != nil {
		t.Fatal(err)
	}
	if one.MessagesPath == two.MessagesPath || one.StagingDir == two.StagingDir || one.StoragePaths.ToolStorage == two.StoragePaths.ToolStorage {
		t.Fatalf("profiles share storage: %q vs %q", one.MessagesPath, two.MessagesPath)
	}
}

// The session list names a profile's transcript without loading the profile; the path
// must be the one the loader reports, or its validator would watch the wrong file.
func TestProfileMessagesPathMatchesTheLoadedTranscript(t *testing.T) {
	spec := layouttest.Spec{Profiles: map[string]map[string]any{"dev/one": {}, "review/two.x": {}}}
	location, _ := layoutLocation(t, spec)
	for _, profile := range []string{"dev/one", "review/two.x"} {
		config, err := LoadSessionConfig(location, profile, "")
		if err != nil {
			t.Fatal(err)
		}
		got, ok := location.ProfileMessagesPath(profile)
		if !ok || got != config.MessagesPath {
			t.Fatalf("ProfileMessagesPath(%q) = %q, %v; the loader reports %q", profile, got, ok, config.MessagesPath)
		}
	}
	for _, id := range []string{"", "dev", "/two", "dev/"} {
		if got, ok := location.ProfileMessagesPath(id); ok {
			t.Fatalf("ProfileMessagesPath(%q) = %q, want it refused", id, got)
		}
	}
}

func TestUsageBaselinePathFollowsTheUsageCache(t *testing.T) {
	if got := UsageBaselinePath("/state/staging/usage-cache.json"); got != "/state/staging/usage-cache.json.baseline" {
		t.Fatalf("baseline path = %q", got)
	}
	if got := UsageBaselinePath(""); got != "" {
		t.Fatalf("baseline path of no usage cache = %q, want none", got)
	}
}
