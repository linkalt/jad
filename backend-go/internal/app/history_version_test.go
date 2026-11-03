package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func historyTestConfig(t *testing.T, backup string) sessionfs.SessionConfig {
	t.Helper()
	directory := t.TempDir()
	config := sessionfs.SessionConfig{BackupTurns: 5}
	config.MessagesPath = filepath.Join(directory, "messages.jsonl")
	config.MessagesBackupPath = filepath.Join(directory, "messages.backup.jsonl")
	if err := os.WriteFile(config.MessagesPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.MessagesBackupPath, []byte(backup), 0o600); err != nil {
		t.Fatal(err)
	}
	return config
}

const historyUserLine = `{"type":"message","role":"user","content":[{"type":"input_text","text":"archived"}]}` + "\n"

// TestHistoryRouteAnswersAMatchingETagWithoutReadingTheBackup: the history ETag depends on
// file stamps only, so a client that holds the block gets 304 before the backup is read.
// The backup here cannot be read (its record is not JSON): a 304 proves it was not.
func TestHistoryRouteAnswersAMatchingETagWithoutReadingTheBackup(t *testing.T) {
	config := historyTestConfig(t, "not a json record\n")
	resolver := &Resolver{}
	if _, err := resolver.historyResponse(context.Background(), config, ""); err == nil {
		t.Fatal("reading the unreadable backup did not fail")
	}
	version, ok, err := historyVersion(config)
	if err != nil || !ok {
		t.Fatalf("historyVersion = %q, %v, %v", version, ok, err)
	}
	response, err := resolver.historyResponse(context.Background(), config, `"`+version+`"`)
	if err != nil {
		t.Fatalf("a matching ETag read the backup: %v", err)
	}
	if !response.NotModified || response.ETag != `"`+version+`"` {
		t.Fatalf("response = %+v, want 304 with ETag %q", response, version)
	}
}

// TestHistoryBlockETagIsTheStampVersion: the block a full read returns carries the same
// ETag the stamp check compares against, so a client that stored it gets 304 next time.
func TestHistoryBlockETagIsTheStampVersion(t *testing.T) {
	config := historyTestConfig(t, historyUserLine)
	resolver := &Resolver{}
	response, err := resolver.historyResponse(context.Background(), config, "")
	if err != nil || response.NotModified || response.ETag == "" {
		t.Fatalf("first read = %+v, %v", response, err)
	}
	again, err := resolver.historyResponse(context.Background(), config, response.ETag)
	if err != nil || !again.NotModified {
		t.Fatalf("revalidation = %+v, %v", again, err)
	}
}

// TestSnapshotHistoryReferenceKeepsTheCountWithItsETag: a snapshot reads the backup only
// when its ETag changed. A count kept under the current ETag is used as it is; an edited
// backup has a new ETag and is counted again.
func TestSnapshotHistoryReferenceKeepsTheCountWithItsETag(t *testing.T) {
	config := historyTestConfig(t, historyUserLine)
	resolver := &Resolver{}
	shared := &sharedSession{}
	first, err := resolver.historyReferenceFor(context.Background(), config, shared)
	if err != nil || first.ETag == nil || first.Count != 1 {
		t.Fatalf("first reference = %+v, %v", first, err)
	}
	// A count kept under the current ETag is answered without a read.
	shared.keepHistoryCount(*first.ETag, 42)
	cached, err := resolver.historyReferenceFor(context.Background(), config, shared)
	if err != nil || cached.Count != 42 || cached.ETag == nil || *cached.ETag != *first.ETag {
		t.Fatalf("cached reference = %+v, %v; want the kept count 42", cached, err)
	}
	// Another turn in the backup changes the ETag, so the backup is read again.
	if err := os.WriteFile(config.MessagesBackupPath, []byte(historyUserLine+historyUserLine), 0o600); err != nil {
		t.Fatal(err)
	}
	edited, err := resolver.historyReferenceFor(context.Background(), config, shared)
	if err != nil || edited.Count != 2 || edited.ETag == nil || *edited.ETag == *first.ETag {
		t.Fatalf("reference after an edit = %+v, %v; want 2 messages under a new ETag", edited, err)
	}
	// No backup: no ETag and no count, as before.
	if err := os.Remove(config.MessagesBackupPath); err != nil {
		t.Fatal(err)
	}
	none, err := resolver.historyReferenceFor(context.Background(), config, shared)
	if err != nil || none.ETag != nil || none.Count != 0 {
		t.Fatalf("reference without a backup = %+v, %v", none, err)
	}
}
