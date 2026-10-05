package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/session"
	"example.com/agent-harness/backend-go/internal/transcript"
)

// recordingRunner is a run session that, like the real bound runner, saves the
// user input (reporting it through OnInputAccepted) before it can fail, and
// keeps the failed runs it is asked to record.
type recordingRunner struct {
	run       func(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error)
	recorded  []transcript.RunErrorEntry
	recordErr error
}

func (r *recordingRunner) Run(ctx context.Context, input json.RawMessage, options session.RunOptions) (session.RunResult, error) {
	return r.run(ctx, input, options)
}

func (r *recordingRunner) RecordRunError(entry transcript.RunErrorEntry) error {
	r.recorded = append(r.recorded, entry)
	return r.recordErr
}

func acceptInput(options session.RunOptions, turn, offset int64, hasMessage bool) {
	options.OnInputAccepted(session.PersistedInput{SessionTurn: turn, MessageOffset: offset, HasMessage: hasMessage, Items: 1})
}

func executeWithRecorder(t *testing.T, runner RunSession) (*activeRun, *syncBuffer) {
	t.Helper()
	logger, output := capturedLogger(slog.LevelInfo)
	handler, err := NewHandler(Config{
		Logger: logger, APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096,
		RunStateRetention: time.Minute, ToolStreamInterval: time.Minute, MaxPageTurns: 50,
	}, &fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handler.Close)
	run, err := handler.runs.begin("session-1\x00dev", "turn-7")
	if err != nil {
		t.Fatal(err)
	}
	handler.executeRun(run, runner, json.RawMessage(`"input"`))
	return run, output
}

func TestFailedRunIsRecordedWithItsTurnAndMessageID(t *testing.T) {
	runner := &recordingRunner{run: func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		acceptInput(options, 4, 1234, true)
		return session.RunResult{}, errors.New("upstream said SECRET-DETAIL at /srv/path")
	}}
	run, _ := executeWithRecorder(t, runner)
	if run.state() != "failed" {
		t.Fatalf("run status = %q, want failed", run.state())
	}
	if len(runner.recorded) != 1 {
		t.Fatalf("recorded %d failures, want 1: %+v", len(runner.recorded), runner.recorded)
	}
	got := runner.recorded[0]
	if got.Turn != 4 || got.MessageID != "session-user-o1234" || got.TurnID != "turn-7" {
		t.Fatalf("entry = %+v, want turn 4, message session-user-o1234, turn id turn-7", got)
	}
	if got.Message != clientRunErrorMessage(errors.New("x")) {
		t.Fatalf("message = %q, want the client-safe text", got.Message)
	}
	if strings.Contains(got.Message, "SECRET-DETAIL") || strings.Contains(got.Message, "/srv/path") {
		t.Fatalf("the recorded message leaks the provider detail: %q", got.Message)
	}
	if _, err := time.Parse(time.RFC3339Nano, got.At); err != nil || !strings.HasSuffix(got.At, "Z") {
		t.Fatalf("at = %q, want a UTC ISO timestamp: %v", got.At, err)
	}
}

func TestFailedRunRecordsAProviderStatusMessage(t *testing.T) {
	runner := &recordingRunner{run: func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		acceptInput(options, 1, 0, true)
		return session.RunResult{}, providerStatus(401)
	}}
	executeWithRecorder(t, runner)
	if len(runner.recorded) != 1 || runner.recorded[0].Message != "The provider request failed (HTTP 401). Response details were withheld." {
		t.Fatalf("recorded = %+v", runner.recorded)
	}
}

type providerStatus int

func (p providerStatus) Error() string       { return "provider error" }
func (p providerStatus) ProviderStatus() int { return int(p) }

func TestFailedRunWithoutVisibleTextHasNoMessageID(t *testing.T) {
	runner := &recordingRunner{run: func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		acceptInput(options, 2, 99, false)
		return session.RunResult{}, errors.New("boom")
	}}
	executeWithRecorder(t, runner)
	if len(runner.recorded) != 1 || runner.recorded[0].MessageID != "" || runner.recorded[0].Turn != 2 {
		t.Fatalf("an image-only input must record a turn and no message id, got %+v", runner.recorded)
	}
}

func TestStoppedRunIsNotRecorded(t *testing.T) {
	runner := &recordingRunner{run: func(ctx context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		acceptInput(options, 1, 0, true)
		return session.RunResult{}, context.Canceled
	}}
	run, _ := executeWithRecorder(t, runner)
	if run.state() != "aborted" {
		t.Fatalf("run status = %q, want aborted", run.state())
	}
	if len(runner.recorded) != 0 {
		t.Fatalf("a user stop is not an error and must not be recorded: %+v", runner.recorded)
	}
}

func TestFailureBeforeTheInputIsSavedIsNotRecorded(t *testing.T) {
	runner := &recordingRunner{run: func(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error) {
		return session.RunResult{}, errors.New("input could not be normalized")
	}}
	run, _ := executeWithRecorder(t, runner)
	if run.state() != "failed" {
		t.Fatalf("run status = %q, want failed", run.state())
	}
	if len(runner.recorded) != 0 {
		t.Fatalf("with no saved turn there is nothing to anchor to: %+v", runner.recorded)
	}
}

func TestPanickingRunIsRecordedAsFailed(t *testing.T) {
	runner := &recordingRunner{run: func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		acceptInput(options, 3, 50, true)
		panic("tool exploded")
	}}
	run, _ := executeWithRecorder(t, runner)
	if run.state() != "failed" {
		t.Fatalf("run status = %q, want failed", run.state())
	}
	if len(runner.recorded) != 1 || runner.recorded[0].Turn != 3 || strings.Contains(runner.recorded[0].Message, "tool exploded") {
		t.Fatalf("recorded = %+v", runner.recorded)
	}
}

func TestRecordingFailureOnlyWarnsAndKeepsTheRunOutcome(t *testing.T) {
	runner := &recordingRunner{recordErr: errors.New("disk full"), run: func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		acceptInput(options, 1, 0, true)
		return session.RunResult{}, errors.New("boom")
	}}
	run, output := executeWithRecorder(t, runner)
	if run.state() != "failed" {
		t.Fatalf("run status = %q, want failed even when the cache write fails", run.state())
	}
	if !strings.Contains(output.String(), "failed to record run error") || !strings.Contains(output.String(), "level=WARN") {
		t.Fatalf("a failed cache write must log a warning:\n%s", output.String())
	}
}

func TestRunSessionWithoutARecorderStillFails(t *testing.T) {
	var runner RunSession = runnerFunc(func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		acceptInput(options, 1, 0, true)
		return session.RunResult{}, errors.New("boom")
	})
	run, _ := executeWithRecorder(t, runner)
	if run.state() != "failed" {
		t.Fatalf("run status = %q, want failed", run.state())
	}
}
