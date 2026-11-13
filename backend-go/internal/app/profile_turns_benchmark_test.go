package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// newProfileTurnsBenchmarkResolver builds one catalog-layout session whose scope holds
// profileCount profiles, each with turnsPerProfile completed turns in its own transcript.
func newProfileTurnsBenchmarkResolver(b testing.TB, profileCount, turnsPerProfile int) *Resolver {
	b.Helper()
	cwd := b.TempDir()
	root := filepath.Join(cwd, ".jad")
	global := filepath.Join(b.TempDir(), "global")
	write := func(rel string, value any) {
		base := root
		if strings.HasPrefix(rel, "models/") || strings.HasPrefix(rel, "tools/") || rel == "global.json" {
			base = global
		}
		path := filepath.Join(base, rel)
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
	write("workspace.json", map[string]any{"schema_version": 2, "id": "local"})
	write("global.json", map[string]any{"schema_version": 2})
	write("models/openai/small.json", map[string]any{"provider": "openai", "model": "m", "api_key_env": "FIXTURE_KEY", "base_url": "http://127.0.0.1:1/v1"})
	write("tools/coding/none.json", map[string]any{"native": map[string]any{}, "mcp": map[string]any{}})

	text := strings.Repeat("x", 600)
	var body strings.Builder
	for turn := 1; turn <= turnsPerProfile; turn++ {
		fmt.Fprintf(&body, `{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d %s"}]}`+"\n", turn, text)
		fmt.Fprintf(&body, `{"type":"response.completed","session_turn":%d,"response":{"id":"resp_%d","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"%s"}]}]}}`+"\n", turn, turn, text)
	}
	for index := 0; index < profileCount; index++ {
		name := fmt.Sprintf("p%02d", index)
		write("profiles/dev/"+name+".json", map[string]any{"models": []string{"openai/small"}, "model_default": "openai/small", "toolset": "coding/none"})
		dir := filepath.Join(cwd, ".jad", "state", "demo", "dev", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"), []byte(body.String()), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	write("sessions/demo.json", map[string]any{
		"schema_version": 1, "cwd": cwd, "category": "work",
		"scope": map[string]any{"profiles": []string{"dev"}},
	})

	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir: cwd, GlobalDir: global,
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

// BenchmarkListSessionsProfiles measures a session list for one session as the number of
// profiles in its scope grows: "conditional" is the 304 path, "unconditional" rebuilds the
// body and counts the turns of every profile.
func BenchmarkListSessionsProfiles(b *testing.B) {
	ctx := context.Background()
	for _, turns := range []int{50, 2000} {
		for _, profiles := range []int{1, 4, 8} {
			resolver := newProfileTurnsBenchmarkResolver(b, profiles, turns)
			first, err := resolver.ListSessions(ctx, "", nil)
			if err != nil {
				b.Fatal(err)
			}
			label := fmt.Sprintf("%d-profiles/%d-turns", profiles, turns)
			b.Run("conditional/"+label, func(b *testing.B) {
				b.ReportAllocs()
				for iteration := 0; iteration < b.N; iteration++ {
					response, err := resolver.ListSessions(ctx, first.ETag, nil)
					if err != nil || !response.NotModified {
						b.Fatalf("conditional list: notModified=%v err=%v", response.NotModified, err)
					}
				}
			})
			b.Run("unconditional/"+label, func(b *testing.B) {
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
}
