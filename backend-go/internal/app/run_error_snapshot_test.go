package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/httpapi"
	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/transcript"
)

func newRunErrorSnapshotFixture(t *testing.T) (*Resolver, sessionfs.Location, sessionfs.SessionConfig) {
	t.Helper()
	loaded, _ := layoutConfig(t, catalogSpec(), map[string]string{})
	roots := loaded.Roots
	location, err := sessionfs.Resolve(roots, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(config.MessagesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// A user turn with no answer: what a failed run leaves in the transcript.
	user := `{"type":"message","role":"user","content":[{"type":"input_text","text":"please fail"}]}` + "\n"
	if err := os.WriteFile(config.MessagesPath, []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	return resolver, location, config
}

func snapshotFailures(t *testing.T, body json.RawMessage) (present bool, failures []transcript.RunErrorEntry) {
	t.Helper()
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	raw, present := snapshot["failures"]
	if !present {
		return false, nil
	}
	if err := json.Unmarshal(raw, &failures); err != nil {
		t.Fatalf("decode failures: %v", err)
	}
	return true, failures
}

func TestSnapshotOmitsFailuresWhenNothingFailed(t *testing.T) {
	resolver, location, _ := newRunErrorSnapshotFixture(t)
	response, err := resolver.ReadSessionSnapshot(context.Background(), location.Reference, "", httpapi.SnapshotOptions{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if present, _ := snapshotFailures(t, response.Body); present {
		t.Fatalf("a session that never failed must keep its payload, got: %s", response.Body)
	}
}

func TestSnapshotCarriesFailuresFromTheRunErrorCache(t *testing.T) {
	resolver, location, config := newRunErrorSnapshotFixture(t)
	shared, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	want := transcript.RunErrorEntry{Turn: 1, MessageID: "session-user-o0", TurnID: "web-1", At: "2026-01-01T00:00:00.000Z", Message: "The run failed."}
	if err := shared.store.RecordRunError(want, config.Persistence.RunErrorCacheMaxEntries); err != nil {
		t.Fatal(err)
	}
	response, err := resolver.ReadSessionSnapshot(context.Background(), location.Reference, "", httpapi.SnapshotOptions{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	present, failures := snapshotFailures(t, response.Body)
	if !present || len(failures) != 1 || failures[0] != want {
		t.Fatalf("snapshot failures = %+v (present=%v), want [%+v]\n%s", failures, present, want, response.Body)
	}
	// The failure is data beside the transcript: the message list still holds only
	// the user's turn, and the failure text appears nowhere in it.
	var snapshot struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(response.Body, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 1 || strings.Contains(string(snapshot.Messages[0]), "The run failed.") {
		t.Fatalf("messages = %s", snapshot.Messages)
	}
	transcriptBytes, err := os.ReadFile(config.MessagesPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transcriptBytes), "The run failed.") || strings.Contains(string(transcriptBytes), "web-1") {
		t.Fatalf("messages.jsonl must not hold the failure: %s", transcriptBytes)
	}
}

func TestRunErrorCacheChangesTheSessionETag(t *testing.T) {
	resolver, location, config := newRunErrorSnapshotFixture(t)
	ctx := context.Background()
	before, err := resolver.ReadSessionSnapshot(ctx, location.Reference, "", httpapi.SnapshotOptions{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := resolver.ReadSessionSnapshot(ctx, location.Reference, "", httpapi.SnapshotOptions{}, before.ETag, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged.NotModified {
		t.Fatalf("an unchanged session must answer not-modified, got %+v", unchanged)
	}

	shared, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	entry := transcript.RunErrorEntry{Turn: 1, MessageID: "session-user-o0", TurnID: "web-1", At: "2026-01-01T00:00:00.000Z", Message: "The run failed."}
	if err := shared.store.RecordRunError(entry, config.Persistence.RunErrorCacheMaxEntries); err != nil {
		t.Fatal(err)
	}
	after, err := resolver.ReadSessionSnapshot(ctx, location.Reference, "", httpapi.SnapshotOptions{}, before.ETag, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after.NotModified {
		t.Fatal("a recorded failure must change the etag, or a reload answers 304 and never shows it")
	}
	if after.ETag == before.ETag {
		t.Fatalf("etag did not change: %s", after.ETag)
	}
	if present, failures := snapshotFailures(t, after.Body); !present || len(failures) != 1 {
		t.Fatalf("failures = %+v (present=%v)", failures, present)
	}
}
