package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/session"
)

// Listing and resolving a session never create its messages directory; the
// first run does, before the input is appended to the transcript.
func TestFirstRunCreatesTheMessagesDirectoryThatResolveLeftAbsent(t *testing.T) {
	f := newLayoutFixture(t)
	bound := boundRunnerOf(t, f, "review/reviewer")
	directory := filepath.Dir(bound.config.MessagesPath)
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolve created the messages directory: stat err = %v", err)
	}

	if _, err := bound.Run(context.Background(), json.RawMessage(`"hi"`), session.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		t.Fatalf("run did not create the messages directory: stat err = %v", err)
	}
}
