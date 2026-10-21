package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

func backdateTree(t testing.TB, roots ...string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(path, old, old)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// newManySessionsResolver is a catalog-layout root with sessionCount sessions that all
// use the same profile, model and toolset files, each with turnsPerSession turns.
func newManySessionsResolver(b testing.TB, sessionCount, turnsPerSession int) *Resolver {
	b.Helper()
	root, cwd := b.TempDir(), b.TempDir()
	write := func(rel string, value any) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			b.Fatal(err)
		}
		data, err := json.Marshal(value)
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			b.Fatal(err)
		}
	}
	write("setting.json", map[string]any{
		"schema_version": 1,
		"session_roots":  []any{map[string]any{"id": "local", "path": "sessions"}},
		"catalogs":       map[string]any{"profiles": "profiles", "models": "models", "tools": "tools", "mcp": "mcp", "state": filepath.Join(cwd, ".jad", "state")},
	})
	write("models/openai/small.json", map[string]any{"provider": "openai", "model": "m", "api_key_env": "FIXTURE_KEY", "base_url": "http://127.0.0.1:1/v1"})
	write("tools/coding/none.json", map[string]any{"native": map[string]any{}, "mcp": map[string]any{}})
	for _, category := range []string{"dev", "ops", "docs"} {
		write("profiles/"+category+"/main.json", map[string]any{"models": []string{"openai/small"}, "model_default": "openai/small", "toolset": "coding/none"})
	}
	body := ""
	for turn := 1; turn <= turnsPerSession; turn++ {
		body += fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d"}]}`+"\n", turn)
		body += fmt.Sprintf(`{"type":"response.completed","session_turn":%d,"response":{"id":"resp_%d","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}}`+"\n", turn, turn)
	}
	for index := 0; index < sessionCount; index++ {
		id := fmt.Sprintf("session-%04d", index)
		write("sessions/"+id+".json", map[string]any{"schema_version": 1, "cwd": cwd, "category": "work", "scope": map[string]any{"profiles": []string{"dev"}}})
		dir := filepath.Join(cwd, ".jad", "state", id, "dev", "main")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"), []byte(body), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	// A settings stamp never trusts a file changed within the last two seconds, so the
	// fixture is dated like a store that has been there for a while.
	backdateTree(b, root)
	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir: root, RootSettingPath: filepath.Join(root, "setting.json"),
		Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_KEY": "k"},
	})
	if err != nil {
		b.Fatal(err)
	}
	resolver, err := NewResolver(config.Roots, config.Environment, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 1024})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(resolver.Close)
	return resolver
}

// BenchmarkListManySessions is the session list the frontend polls every few seconds:
// "conditional" is the 304 path, "unconditional" rebuilds the body.
func BenchmarkListManySessions(b *testing.B) {
	ctx := context.Background()
	for _, sessions := range []int{1, 10, 50, 200} {
		resolver := newManySessionsResolver(b, sessions, 20)
		first, err := resolver.ListSessions(ctx, "", nil)
		if err != nil {
			b.Fatal(err)
		}
		if listed := strings.Count(string(first.Body), `"session_name"`); listed != sessions {
			b.Fatalf("list holds %d sessions, want %d (a skipped session makes the numbers meaningless)", listed, sessions)
		}
		b.Run(fmt.Sprintf("conditional/%d", sessions), func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				if response, err := resolver.ListSessions(ctx, first.ETag, nil); err != nil || !response.NotModified {
					b.Fatalf("conditional list: notModified=%v err=%v", response.NotModified, err)
				}
			}
		})
		b.Run(fmt.Sprintf("unconditional/%d", sessions), func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				if response, err := resolver.ListSessions(ctx, "", nil); err != nil || len(response.Body) == 0 {
					b.Fatalf("unconditional list: err=%v", err)
				}
			}
		})
	}
}
