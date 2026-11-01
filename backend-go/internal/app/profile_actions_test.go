package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// ProfileInfo reads the same transcript the catalog reads. On the layout fixture of this package
// (catalogSpec: a real messages.jsonl written to the profile's storage path), it reports the input
// tokens of the last completed response, and nil ("no usage yet") when none carried usage.
func TestProfileInfoReportsTheLastCompletedResponseOrNoUsage(t *testing.T) {
	loaded, _ := layoutConfig(t, catalogSpec(), map[string]string{})
	location, err := sessionfs.Resolve(loaded.Roots, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	sessionConfig, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(loaded.Roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	actions := profileMessenger{resolver: resolver}
	write := func(transcript string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(sessionConfig.MessagesPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sessionConfig.MessagesPath, []byte(transcript), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	user := `{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`
	noUsage := `{"type":"response.completed","session_turn":1,"session_step":1,"response":{"id":"resp-1","status":"completed","output":[]}}`
	withUsage := `{"type":"response.completed","session_turn":2,"session_step":1,"response":{"id":"resp-2","status":"completed","model":"gpt-test","output":[],"usage":{"input_tokens":1234,"output_tokens":5,"total_tokens":1239}}}`

	// No transcript yet: no response has completed.
	if tokens, err := actions.ProfileInfo(context.Background(), "session-1", "dev/coder"); err != nil || tokens != nil {
		t.Fatalf("no transcript: tokens = %v, err = %v; want nil (no usage yet)", tokens, err)
	}
	// A transcript with a user message but no completed response.
	write(user + "\n")
	if tokens, err := actions.ProfileInfo(context.Background(), "session-1", "dev/coder"); err != nil || tokens != nil {
		t.Fatalf("no completed response: tokens = %v, err = %v; want nil", tokens, err)
	}
	// A completed response without a usage block: Current is set, InputTokens is not.
	write(user + "\n" + noUsage + "\n")
	if tokens, err := actions.ProfileInfo(context.Background(), "session-1", "dev/coder"); err != nil || tokens != nil {
		t.Fatalf("completed response without usage: tokens = %v, err = %v; want nil", tokens, err)
	}
	// A completed response with usage after one without: its input tokens, the newest.
	write(strings.Join([]string{user, noUsage, withUsage}, "\n") + "\n")
	tokens, err := actions.ProfileInfo(context.Background(), "session-1", "dev/coder")
	if err != nil || tokens == nil || *tokens != 1234 {
		t.Fatalf("with usage: tokens = %v, err = %v; want 1234", tokens, err)
	}
}
