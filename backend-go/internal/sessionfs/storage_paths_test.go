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
		ActiveProfile: "a/b__c",
		Profiles:      map[string]map[string]any{"a/b__c": {}, "a__b/c": {}},
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
