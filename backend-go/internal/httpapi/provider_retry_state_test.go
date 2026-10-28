package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/session"
)

func newRetryTestHandler(t *testing.T) *Handler {
	t.Helper()
	handler, err := NewHandler(Config{
		ShutdownTimeout:     50 * time.Millisecond,
		APIToken:            testAPIToken,
		MaxBodyBytes:        4096,
		SSEMaxBufferedBytes: 4096,
		RunStateRetention:   time.Minute,
		ToolStreamInterval:  time.Minute,
		MaxPageTurns:        50,
	}, &fakeResolver{})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	t.Cleanup(handler.Close)
	return handler
}

// waitEvent decodes the data of one event frame.
func waitEvent(t *testing.T, frame []byte) map[string]any {
	t.Helper()
	text := string(frame)
	index := strings.Index(text, "data: ")
	if index < 0 {
		t.Fatalf("no data in frame %q", text)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text[index+len("data: "):])), &data); err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	return data
}

func TestProviderRetryWaitIsPublishedReplayedAndClearedOnTheRun(t *testing.T) {
	handler := newRetryTestHandler(t)
	run, err := handler.runs.begin("session\x00default", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	live := run.subscribe()
	defer run.detach(live)
	if event, ok := nextRunTestEvent(t, live, time.Second); !ok || event.name != "run" {
		t.Fatalf("first event = %+v", event)
	}

	run.setProviderRetry(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if event, ok := live.next(ctx); ok {
		t.Fatalf("clearing a wait that was never set sent %q", event.name)
	}
	cancel()

	before := time.Now()
	run.setProviderRetry(&responses.RetryWait{Attempt: 2, MaxRetries: 5, Status: 429, Delay: 4 * time.Second})
	event, ok := nextRunTestEvent(t, live, time.Second)
	if !ok || event.name != "provider_retry" {
		t.Fatalf("event = %+v, ok=%v; want provider_retry", event, ok)
	}
	data := waitEvent(t, event.frame)
	waiting, _ := data["waiting"].(map[string]any)
	if data["turn_id"] != "turn-1" || waiting["attempt"] != float64(2) || waiting["max_retries"] != float64(5) ||
		waiting["status"] != float64(429) || waiting["wait_ms"] != float64(4000) {
		t.Fatalf("data = %v", data)
	}
	resumeAt, err := time.Parse(time.RFC3339Nano, waiting["resume_at"].(string))
	if err != nil || resumeAt.Before(before.Add(3900*time.Millisecond)) || resumeAt.After(time.Now().Add(4100*time.Millisecond)) {
		t.Fatalf("resume_at = %v (%v)", waiting["resume_at"], err)
	}

	late := run.subscribe()
	defer run.detach(late)
	if first, _ := nextRunTestEvent(t, late, time.Second); first.name != "run" {
		t.Fatalf("late first event = %q", first.name)
	}
	if replay, ok := nextRunTestEvent(t, late, time.Second); !ok || replay.name != "provider_retry" {
		t.Fatalf("a client attaching mid-wait must be told about it, got %+v", replay)
	}

	run.setProviderRetry(nil)
	cleared, ok := nextRunTestEvent(t, live, time.Second)
	if !ok || cleared.name != "provider_retry" || waitEvent(t, cleared.frame)["waiting"] != nil {
		t.Fatalf("clear = %+v, ok=%v", cleared, ok)
	}
	after := run.subscribe()
	defer run.detach(after)
	nextRunTestEvent(t, after, time.Second) // the run summary
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if event, ok := after.next(ctx); ok {
		t.Fatalf("a cleared wait was replayed as %q", event.name)
	}
}

func TestProviderRetryWaitIsDroppedWhenTheRunFinishes(t *testing.T) {
	handler := newRetryTestHandler(t)
	run, err := handler.runs.begin("session\x00default", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	run.setProviderRetry(&responses.RetryWait{Attempt: 1, MaxRetries: 2, Status: 503, Delay: time.Second})
	run.finish("failed", failureEvents("boom"))
	run.setProviderRetry(&responses.RetryWait{Attempt: 2, MaxRetries: 2, Status: 503, Delay: time.Second})

	queue := run.subscribe()
	defer run.detach(queue)
	for {
		event, ok := nextRunTestEvent(t, queue, 100*time.Millisecond)
		if !ok {
			return
		}
		if event.name == "provider_retry" {
			t.Fatal("a finished run replayed a retry wait")
		}
	}
}

func TestExecuteRunGivesTheRunsContextAProviderRetryNotifier(t *testing.T) {
	handler := newRetryTestHandler(t)
	run, err := handler.runs.begin("session\x00default", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	queue := run.subscribe()
	defer run.detach(queue)
	nextRunTestEvent(t, queue, time.Second) // the run summary

	completed := json.RawMessage(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}`)
	runner := runnerFunc(func(ctx context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		notify := responses.RetryNotifierFrom(ctx)
		if notify == nil {
			t.Error("the run's context carries no retry notifier")
			return session.RunResult{TurnID: options.TurnID, Event: completed}, nil
		}
		notify(&responses.RetryWait{Attempt: 1, MaxRetries: 3, Status: 0, Delay: time.Second})
		notify(nil)
		return session.RunResult{TurnID: options.TurnID, Event: completed}, nil
	})
	handler.executeRun(run, runner, json.RawMessage(`"input"`))

	var seen []map[string]any
	for {
		event, ok := nextRunTestEvent(t, queue, 200*time.Millisecond)
		if !ok {
			break
		}
		if event.name == "provider_retry" {
			seen = append(seen, waitEvent(t, event.frame))
		}
	}
	if len(seen) != 2 {
		t.Fatalf("provider_retry events = %d, want a wait and a clear", len(seen))
	}
	waiting, _ := seen[0]["waiting"].(map[string]any)
	if waiting["status"] != nil {
		t.Fatalf("a connection failure must have status null, got %v", waiting["status"])
	}
	if seen[1]["waiting"] != nil {
		t.Fatalf("second event = %v, want a clear", seen[1])
	}
}
