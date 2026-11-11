package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// seededRequest is a child request that resumes from seed, as startResume makes one.
func seededRequest(seed string) RunSpec {
	return RunSpec{Kind: RunEphemeral, CallID: "call-2", Seed: seed}
}

func TestResumedRunCopiesTheSourceTranscriptAndLeavesTheSourceAlone(t *testing.T) {
	storage := t.TempDir()
	source := filepath.Join(storage, "call-1-aaaaaaaa")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	history := `{"type":"message","role":"user","content":[{"type":"input_text","text":"first question"}]}` + "\n"
	if err := os.WriteFile(filepath.Join(source, "messages.jsonl"), []byte(history), 0o644); err != nil {
		t.Fatal(err)
	}
	// The journal settings are the production defaults (normalizePersistence): turn durability and an item limit.
	config := sessionfs.SessionConfig{
		NativeSubagent: &sessionfs.NativeSubagentSettings{StorageDir: storage},
		Persistence:    sessionfs.Persistence{Durability: "turn", MaxItemBytes: 1 << 20, ResponseMetadata: "minimal", CursorCacheCheckpointBytes: 1 << 20},
	}
	store, dir, err := (&Resolver{}).openEphemeralStore(config, seededRequest(filepath.Join(source, "messages.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	_ = store
	if dir == source {
		t.Fatal("the resumed run reused the source folder")
	}
	copied, err := os.ReadFile(filepath.Join(dir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(copied) != history {
		t.Fatalf("the new journal does not start with the seeded history:\n%s", copied)
	}
	after, err := os.ReadFile(filepath.Join(source, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != history {
		t.Fatalf("the source transcript changed:\n%s", after)
	}
}

func TestResumedRunNeverOverwritesAFileInItsOwnFolder(t *testing.T) {
	// A copy goes to a new folder; an existing file there is an error, not a silent overwrite.
	storage := t.TempDir()
	source := filepath.Join(storage, "call-1-bbbbbbbb")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "messages.jsonl"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFileExclusive(filepath.Join(source, "messages.jsonl"), filepath.Join(source, "messages.jsonl")); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("copy over an existing file = %v, want an exists error", err)
	}
}
