package app

import (
	"context"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// Appends that land while a cold usage scan runs are past the snapshot end, so they must not
// change the result for the prefix the scan covered. A scan before the appends and a scan
// after them, on a transcript without the appended records, must agree.
func TestProfileInfoMatchesTheScanOfTheSamePrefixWhenAppendsRunAlongside(t *testing.T) {
	const appendItem = `{"type":"message","role":"user","content":[{"type":"input_text","text":"appended"}]}`
	resolver := newProfileTurnsBenchmarkResolver(t, 1, 200)
	location, err := sessionfs.Resolve(resolver.roots, "demo")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionSettings(location, "dev/p00", "")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	actions := profileMessenger{resolver: resolver}
	before, err := actions.ProfileInfo(context.Background(), "demo", "dev/p00")
	if err != nil || before == nil {
		t.Fatalf("scan before appends: tokens = %v, err = %v", before, err)
	}

	for range 50 {
		if _, err := shared.store.AppendInputItemsAt([]byte(appendItem)); err != nil {
			t.Fatal(err)
		}
	}
	after, err := actions.ProfileInfo(context.Background(), "demo", "dev/p00")
	if err != nil || after == nil || *after != *before {
		t.Fatalf("scan after appends: tokens = %v, err = %v; want %v (appends add no usage)", after, err, *before)
	}
}
