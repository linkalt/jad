package app

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSessionListLogsAnInvalidSessionOncePerReason polls the list like the
// frontend does and checks the broken session is reported once, with its reason,
// while the healthy session is still listed.
func TestSessionListLogsAnInvalidSessionOncePerReason(t *testing.T) {
	workspace := t.TempDir()
	rootDir := filepath.Join(workspace, "sessions")
	for name, setting := range map[string]string{
		"good":   `{"cwd":"` + filepath.ToSlash(workspace) + `","persistence":{"durability":"process","max_item_bytes":1048576}}`,
		"broken": `{"cwd":"` + filepath.ToSlash(workspace) + `","tools":{"native":{"nonexistent_tool":{}}}}`,
	} {
		dir := filepath.Join(rootDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "setting.json"), []byte(setting), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "model.json"), []byte(`{"provider":"openai","model":"gpt-test"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	output := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(output, nil))
	roots := []sessionfs.Root{{ID: "primary", Label: "Primary", Directory: rootDir}}
	resolver, err := NewResolver(roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50}, WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()

	var body string
	for poll := 0; poll < 5; poll++ {
		response, err := resolver.ListSessions(context.Background(), "", nil)
		if err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
		body = string(response.Body)
	}
	if !strings.Contains(body, `"good"`) || strings.Contains(body, `"broken"`) {
		t.Fatalf("list must contain the healthy session only: %s", body)
	}
	logged := output.String()
	if count := strings.Count(logged, "skipping invalid session"); count != 1 {
		t.Fatalf("broken session logged %d times over 5 polls, want 1:\n%s", count, logged)
	}
	if !strings.Contains(logged, "session=broken") || !strings.Contains(logged, "nonexistent_tool") {
		t.Errorf("the skip must name the session and the reason:\n%s", logged)
	}
	if strings.Contains(logged, "session=good") {
		t.Errorf("a healthy session must not be logged:\n%s", logged)
	}
}
