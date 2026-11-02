package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// A cold scan after the transcript was replaced by a copy with the same bytes (a new inode)
// must report the same usage as the scan before the replacement. The snapshot read has no
// lock, so this checks the identity path end to end without double-counting.
func TestProfileInfoIsStableAcrossAnInodeReplacementWithTheSameBytes(t *testing.T) {
	resolver := newProfileTurnsBenchmarkResolver(t, 1, 50)
	location, err := sessionfs.Resolve(resolver.roots, "demo")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionSettings(location, "dev/p00", "")
	if err != nil {
		t.Fatal(err)
	}
	actions := profileMessenger{resolver: resolver}
	first, err := actions.ProfileInfo(context.Background(), "demo", "dev/p00")
	if err != nil || first == nil {
		t.Fatalf("first scan: tokens = %v, err = %v", first, err)
	}

	data, err := os.ReadFile(config.MessagesPath)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(filepath.Dir(config.MessagesPath), "copy.jsonl")
	if err := os.WriteFile(replacement, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, config.MessagesPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(config.UsageCachePath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	second, err := actions.ProfileInfo(context.Background(), "demo", "dev/p00")
	if err != nil || second == nil || *second != *first {
		t.Fatalf("after inode replacement: tokens = %v, err = %v; want %v", second, err, *first)
	}
}
