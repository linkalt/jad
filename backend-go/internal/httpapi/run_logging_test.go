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
)

func runWithLogger(t *testing.T, level slog.Level, runner runnerFunc) (*activeRun, *syncBuffer) {
	t.Helper()
	logger, output := capturedLogger(level)
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

func TestRunLifecycleLogsStartAndFinishWithIdentifiers(t *testing.T) {
	_, output := runWithLogger(t, slog.LevelInfo, func(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error) {
		return session.RunResult{}, errors.New("provider returned 502 for https://api.example.test/v1/responses")
	})
	logged := output.String()
	for _, want := range []string{
		`msg="run started"`, "session=session-1", "profile=dev", "turn=turn-7",
		`msg="run failed"`, "level=ERROR", "502",
		`msg="run finished"`, "status=failed", "duration_ms=",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("run log is missing %q:\n%s", want, logged)
		}
	}
}

func TestRunAbortLogsTheCancellationReason(t *testing.T) {
	logger, output := capturedLogger(slog.LevelInfo)
	handler, err := NewHandler(Config{Logger: logger, APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute, ToolStreamInterval: time.Minute, MaxPageTurns: 50}, &fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	run, err := handler.runs.begin("session-1\x00", "turn-8")
	if err != nil {
		t.Fatal(err)
	}
	handler.executeRun(run, runnerFunc(func(ctx context.Context, _ json.RawMessage, _ session.RunOptions) (session.RunResult, error) {
		run.cancel()
		return session.RunResult{}, ctx.Err()
	}), json.RawMessage(`"input"`))

	logged := output.String()
	for _, want := range []string{`msg="run aborted"`, "reason=", "context canceled", "status=aborted", "profile=-"} {
		if !strings.Contains(logged, want) {
			t.Errorf("abort log is missing %q:\n%s", want, logged)
		}
	}
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("a stopped run is not an error:\n%s", logged)
	}
}

func TestPanickingRunFailsTheRunInsteadOfTheProcess(t *testing.T) {
	run, output := runWithLogger(t, slog.LevelInfo, func(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error) {
		panic("tool exploded")
	})
	if state := run.state(); state != "failed" {
		t.Fatalf("run state = %q, want failed", state)
	}
	logged := output.String()
	for _, want := range []string{`msg="run panicked"`, "tool exploded", "stack=", "status=failed"} {
		if !strings.Contains(logged, want) {
			t.Errorf("panic log is missing %q:\n%s", want, logged)
		}
	}
	select {
	case <-run.done:
	default:
		t.Error("the run must be finished so subscribers get their terminal events")
	}
}

func TestRunLogsDoNotContainUserInput(t *testing.T) {
	logger, output := capturedLogger(slog.LevelDebug)
	handler, err := NewHandler(Config{Logger: logger, APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute, ToolStreamInterval: time.Minute, MaxPageTurns: 50}, &fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	run, err := handler.runs.begin("session-1\x00", "turn-9")
	if err != nil {
		t.Fatal(err)
	}
	handler.executeRun(run, runnerFunc(func(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error) {
		return session.RunResult{}, errors.New("failed")
	}), json.RawMessage(`"my secret prompt SECRETPROMPT"`))
	if strings.Contains(output.String(), "SECRETPROMPT") {
		t.Errorf("the prompt leaked into the log:\n%s", output.String())
	}
}

func TestNilLoggerKeepsRunsQuiet(t *testing.T) {
	handler, err := NewHandler(Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute, ToolStreamInterval: time.Minute, MaxPageTurns: 50}, &fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	run, err := handler.runs.begin("session-1\x00", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	handler.executeRun(run, runnerFunc(func(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error) {
		panic("no logger configured")
	}), json.RawMessage(`"x"`))
	if run.state() != "failed" {
		t.Errorf("state = %q, want failed even with logging disabled", run.state())
	}
}
