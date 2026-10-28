package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/transcript"
)

func TestExecuteRunCoalescesSSEAppendDeltasUntilCommit(t *testing.T) {
	pending := make(chan struct{})
	flushCommit := make(chan struct{})
	committed := make(chan struct{})
	finished := make(chan struct{})
	var flushOnce sync.Once
	flush := func() { flushOnce.Do(func() { close(flushCommit) }) }
	defer flush()

	completed := json.RawMessage(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ab"}]}]}}`)
	runner := runnerFunc(func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		for _, raw := range []json.RawMessage{
			json.RawMessage(`{"type":"response.output_text.delta","delta":"a"}`),
			json.RawMessage(`{"type":"response.output_text.delta","delta":"b"}`),
		} {
			if err := options.OnEvent(responses.Event{Type: "response.output_text.delta", Raw: raw}); err != nil {
				return session.RunResult{}, err
			}
		}
		close(pending)
		<-flushCommit
		if options.OnResponseCommitted != nil {
			options.OnResponseCommitted(completed, transcript.CommitResult{})
		}
		close(committed)
		return session.RunResult{TurnID: options.TurnID, Event: completed}, nil
	})

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
	defer handler.Close()

	run, err := handler.runs.begin("session\x00default", "turn-1")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	queue := run.subscribe()
	defer run.detach(queue)
	if event, ok := nextRunTestEvent(t, queue, time.Second); !ok || event.name != "run" {
		t.Fatalf("initial event = %+v, ok=%v; want run", event, ok)
	}

	go func() {
		handler.executeRun(run, runner, json.RawMessage(`"input"`))
		close(finished)
	}()
	defer func() {
		flush()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("executeRun did not finish during cleanup")
		}
	}()

	// The first visible change is published as a whole message, with no empty
	// placeholder before it.
	if event, ok := nextRunTestEvent(t, queue, time.Second); !ok || event.name != "ui.message" {
		t.Fatalf("first message event = %+v, ok=%v", event, ok)
	}
	select {
	case <-pending:
	case <-time.After(time.Second):
		t.Fatal("runner did not reach pending append")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if event, ok := queue.next(ctx); ok {
		t.Fatalf("append event %q escaped before the coalescing window was flushed", event.name)
	}

	flush()
	select {
	case <-committed:
	case <-time.After(time.Second):
		t.Fatal("commit callback did not finish")
	}
	event, ok := nextRunTestEvent(t, queue, time.Second)
	if !ok || event.name != "ui.delta" {
		t.Fatalf("coalesced append event = %+v, ok=%v; want ui.delta", event, ok)
	}
	parsed := parseEvents(t, string(event.frame))
	if len(parsed) != 1 || parsed[0].name != "ui.delta" {
		t.Fatalf("parsed append event = %+v", parsed)
	}
	var payload struct {
		Append string `json:"append"`
	}
	if err := json.Unmarshal(parsed[0].data, &payload); err != nil {
		t.Fatalf("decode append event: %v", err)
	}
	if payload.Append != "b" || !strings.Contains(string(event.frame), `"index":0`) {
		t.Fatalf("coalesced delta payload = %s", parsed[0].data)
	}

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("executeRun did not complete")
	}
}

// A run that fails with text still waiting in the coalescing window must not
// publish that text: it was never committed, and TypeScript drops it too.
func TestExecuteRunDropsCoalescedSnapshotWhenTheRunFails(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		for _, raw := range []json.RawMessage{
			json.RawMessage(`{"type":"response.output_text.delta","delta":"a"}`),
			json.RawMessage(`{"type":"response.output_text.delta","delta":"b"}`),
		} {
			if err := options.OnEvent(responses.Event{Type: "response.output_text.delta", Raw: raw}); err != nil {
				return session.RunResult{}, err
			}
		}
		return session.RunResult{}, errors.New("provider stream ended early")
	})

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
	defer handler.Close()

	run, err := handler.runs.begin("session\x00default", "turn-1")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	queue := run.subscribe()
	defer run.detach(queue)

	handler.executeRun(run, runner, json.RawMessage(`"input"`))

	var names []string
	for {
		event, ok := nextRunTestEvent(t, queue, time.Second)
		if !ok {
			t.Fatalf("stream ended early after %v", names)
		}
		names = append(names, event.name)
		if event.name == "done" {
			break
		}
	}
	// run, the first visible message ("a"), the terminal error; no ui.delta for "b".
	want := []string{"run", "ui.message", "error", "done"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", names, want)
	}
}

func nextRunTestEvent(t *testing.T, queue *eventQueue, timeout time.Duration) (streamEvent, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return queue.next(ctx)
}
