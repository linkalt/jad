package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

type checkpointMutation struct {
	*fakeMutation
	restored bool
}

func (m *checkpointMutation) ListCheckpoints(context.Context, bool) (CheckpointListing, error) {
	return CheckpointListing{}, nil
}

func (m *checkpointMutation) RestoreCheckpoint(context.Context, string, CheckpointRestoreRequest) (any, error) {
	m.restored = true
	return map[string]bool{"restored": true}, nil
}

// Each handler that checks runIsActive must still see the running run and refuse at once
// while a second start is queued for the transcript behind it.
func TestHandlersRefuseAtOnceWhileARunIsActiveAndAStartIsQueued(t *testing.T) {
	cases := []struct {
		name, path, body, message string
		ran                       func(f *queuedRunFixture, restore *checkpointMutation) bool
	}{
		{"front compact", "/api/sessions/s/front-compact?profile=dev", `{"message_id":"session-user-o1"}`, "cannot front compact a profile while a run is active",
			func(f *queuedRunFixture, _ *checkpointMutation) bool { return f.mutation.frontCompacted }},
		{"state context", "/api/sessions/s/statecontext?profile=dev", `{"prompt":"p"}`, "cannot write state context while a run is active",
			func(f *queuedRunFixture, _ *checkpointMutation) bool { return f.mutation.statePrompt != "" }},
		{"checkpoint restore", "/api/sessions/s/checkpoints/c1/restore?profile=dev", `{"mode":"safe"}`, "cannot restore a checkpoint while a run is active",
			func(_ *queuedRunFixture, restore *checkpointMutation) bool { return restore.restored }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newQueuedRunFixture(t)
			restore := &checkpointMutation{fakeMutation: f.mutation}
			f.resolver.mutation = restore
			response := f.answer(tc.path, tc.body)
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), tc.message) {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			if tc.ran(f, restore) {
				t.Fatal("the change ran during an active run")
			}
		})
	}
}

type panickingLogHandler struct{ slog.Handler }

func (panickingLogHandler) WithAttrs([]slog.Attr) slog.Handler { panic("log exploded") }

// A panic after the run is registered but before its goroutine starts must fail the run
// and give the transcript back, or the session could never run again.
func TestRunStartFailsTheRunAndReleasesTheTranscriptWhenSetupPanics(t *testing.T) {
	resolver, handler := newIdleTranscriptResolver(t)
	handler.logger = slog.New(panickingLogHandler{slog.NewTextHandler(discardWriter{}, nil)})
	if response := postRun(handler); response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want the request observer's 500", response.Code, response.Body.String())
	}
	if held := len(resolver.transcript); held != 0 {
		t.Fatalf("transcript lock still held after a setup panic (%d)", held)
	}
	if handler.runIsActive("canonical\x00dev") {
		t.Fatal("the run is still running after a setup panic")
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
