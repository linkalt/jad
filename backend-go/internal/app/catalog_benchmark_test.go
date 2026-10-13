package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
)

// newCatalogBenchmarkResolver builds a root holding sessionCount sessions, each
// with turnsPerSession completed turns in messages.jsonl.
func newCatalogBenchmarkResolver(b *testing.B, sessionCount, turnsPerSession int) *Resolver {
	b.Helper()
	workspace := b.TempDir()
	rootDir := filepath.Join(workspace, "sessions")
	text := strings.Repeat("x", 600)
	var transcriptBody strings.Builder
	for turn := 1; turn <= turnsPerSession; turn++ {
		fmt.Fprintf(&transcriptBody, `{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d %s"}]}`+"\n", turn, text)
		fmt.Fprintf(&transcriptBody, `{"type":"response.completed","session_turn":%d,"response":{"id":"resp_%d","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"%s"}]}]}}`+"\n", turn, turn, text)
	}
	setting := fmt.Sprintf(`{"cwd":%q,"persistence":{"durability":"process","max_item_bytes":1048576,"response_metadata":"minimal"}}`, workspace)
	for index := 0; index < sessionCount; index++ {
		dir := filepath.Join(rootDir, fmt.Sprintf("session-%04d", index))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "setting.json"), []byte(setting), 0o600); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "model.json"), []byte(`{"provider":"openai","model":"gpt-test"}`), 0o600); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"), []byte(transcriptBody.String()), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	roots := []sessionfs.Root{{ID: "primary", Label: "Primary", Directory: rootDir}}
	resolver, err := NewResolver(roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 1024})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(resolver.Close)
	return resolver
}

// BenchmarkListSessions measures the session list a polling frontend issues:
// "conditional" repeats the request with the current ETag (the 304 path) and
// "unconditional" rebuilds the whole body.
func BenchmarkListSessions(b *testing.B) {
	for _, sessionCount := range []int{4, 20, 200} {
		resolver := newCatalogBenchmarkResolver(b, sessionCount, 50)
		ctx := context.Background()
		first, err := resolver.ListSessions(ctx, "", nil)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("conditional/%d-sessions", sessionCount), func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				response, err := resolver.ListSessions(ctx, first.ETag, nil)
				if err != nil || !response.NotModified {
					b.Fatalf("conditional list: notModified=%v err=%v", response.NotModified, err)
				}
			}
		})
		b.Run(fmt.Sprintf("unconditional/%d-sessions", sessionCount), func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				response, err := resolver.ListSessions(ctx, "", nil)
				if err != nil || len(response.Body) == 0 {
					b.Fatalf("unconditional list: err=%v", err)
				}
			}
		})
	}
}

func BenchmarkReadSessionSnapshotConditional(b *testing.B) {
	resolver := newCatalogBenchmarkResolver(b, 1, 50)
	ctx := context.Background()
	first, err := resolver.ReadSessionSnapshot(ctx, "session-0000", "", httpapi.SnapshotOptions{}, "", nil)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		response, err := resolver.ReadSessionSnapshot(ctx, "session-0000", "", httpapi.SnapshotOptions{}, first.ETag, nil)
		if err != nil || !response.NotModified {
			b.Fatalf("conditional snapshot: notModified=%v err=%v", response.NotModified, err)
		}
	}
}
