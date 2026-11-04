package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"jad/backend-go/internal/tools/native"
)

// A message's model runs the target on that model for that message only; without one the
// target runs its model default. A model the target does not list is refused before any run.
func TestProfileMessageModelPicksTheTargetsModelForThatMessage(t *testing.T) {
	f := newLayoutFixture(t)
	messenger := profileMessenger{resolver: f.service.resolver}
	ctx := callerContextOf(t, f, "review/reviewer")
	const owner = "local:demo\x00review/reviewer"

	if _, err := messenger.send(ctx, native.ProfileMessageRequest{OwnerKey: owner, Target: "dev/coder", Message: "one", Model: "openai/large"}); err != nil {
		t.Fatal(err)
	}
	if got := f.lastModel(); got != "model-large" {
		t.Fatalf("model = %q, want the one the message named", got)
	}
	if _, err := messenger.send(ctx, native.ProfileMessageRequest{OwnerKey: owner, Target: "dev/coder", Message: "two"}); err != nil {
		t.Fatal(err)
	}
	if got := f.lastModel(); got != "model-small" {
		t.Fatalf("model = %q, want the target's model default", got)
	}
	f.mu.Lock()
	runs := len(f.models)
	f.mu.Unlock()
	_, err := messenger.send(ctx, native.ProfileMessageRequest{OwnerKey: owner, Target: "dev/coder", Message: "three", Model: "openai/huge"})
	var modelErr *native.ProfileModelError
	if !errors.As(err, &modelErr) || modelErr.Model != "openai/huge" || !strings.Contains(modelErr.Message, "not available") {
		t.Fatalf("err = %v, want a model error", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.models) != runs {
		t.Fatal("a refused model still reached the provider")
	}
}

func callerContextOf(t *testing.T, f *layoutFixture, profile string) context.Context {
	t.Helper()
	return context.WithValue(context.Background(), callerMessagesPathKey{}, boundRunnerOf(t, f, profile).config.MessagesPath)
}
