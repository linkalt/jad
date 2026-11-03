package app

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"jad/backend-go/internal/httpapi"
)

func TestUsageCacheWriteFailureIsLoggedOnceAndIgnored(t *testing.T) {
	f := newUsageTimestampFixture(t)
	var logs bytes.Buffer
	f.resolver.logger = slog.New(slog.NewTextHandler(&logs, nil))
	if err := os.MkdirAll(f.config.UsageCachePath, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := f.resolver.ReadSessionSnapshot(context.Background(), f.location.Reference, "", httpapi.SnapshotOptions{}, "", nil); err != nil {
			t.Fatalf("snapshot %d failed on the optional usage cache: %v", i, err)
		}
		if _, err := f.resolver.ListSessions(context.Background(), "", nil); err != nil {
			t.Fatalf("list %d failed on the optional usage cache: %v", i, err)
		}
	}
	if got := strings.Count(logs.String(), "usage cache write failed"); got != 1 {
		t.Fatalf("usage cache failure logged %d times, want 1:\n%s", got, logs.String())
	}
}

func TestFrontCompactSucceedsWhenUsageCacheCannotBeWritten(t *testing.T) {
	f := newUsageTimestampFixture(t)
	first := `{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}`
	response := `{"type":"response.completed","session_turn":1,"session_step":1,"response":{"id":"usage-response-1","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7},"output":[]}}`
	second := `{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}`
	if err := os.WriteFile(f.config.MessagesPath, []byte(first+"\n"+response+"\n"+second+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Outside staging, so the compaction's cache invalidation does not clear the obstacle.
	f.config.UsageCachePath = filepath.Join(t.TempDir(), "usage-cache.json")
	if err := os.MkdirAll(f.config.UsageCachePath, 0o755); err != nil {
		t.Fatal(err)
	}
	shared, _, err := f.resolver.sharedStore(f.config)
	if err != nil {
		t.Fatal(err)
	}
	messageID := "session-user-o" + strconv.Itoa(len(first)+len(response)+2)
	if _, err := shared.frontCompactPreservingUsage(f.config, messageID, "permanent"); err != nil {
		t.Fatalf("front compaction failed on the optional usage cache: %v", err)
	}
	current, err := os.ReadFile(f.config.MessagesPath)
	if err != nil || string(current) != first+"\n"+response+"\n" {
		t.Fatalf("transcript = %q err=%v", current, err)
	}
}
