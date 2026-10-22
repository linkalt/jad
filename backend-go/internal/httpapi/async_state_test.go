package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/session"
)

type asyncResolver struct {
	fakeResolver
	mu        sync.Mutex
	state     AsyncState
	notify    func()
	stopped   bool
	reference string
	profile   string
}

func (r *asyncResolver) WatchSessionAsync(reference, profile string, notify func()) (func() AsyncState, func()) {
	r.mu.Lock()
	r.notify, r.reference, r.profile = notify, reference, profile
	r.mu.Unlock()
	read := func() AsyncState {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.state
	}
	return read, func() {
		r.mu.Lock()
		r.stopped = true
		r.mu.Unlock()
	}
}

func (r *asyncResolver) set(state AsyncState) {
	r.mu.Lock()
	r.state = state
	notify := r.notify
	r.mu.Unlock()
	if notify != nil {
		notify()
	}
}

func newAsyncTestHandler(t *testing.T, resolver SessionResolver, interval time.Duration) *Handler {
	t.Helper()
	handler, err := NewHandler(Config{
		APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096,
		RunStateRetention: time.Minute, ToolStreamInterval: interval, MaxPageTurns: 50,
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handler.Close)
	return handler
}

func asyncEvents(t *testing.T, queue *eventQueue) []map[string]any {
	t.Helper()
	var out []map[string]any
	for {
		event, ok := nextRunTestEvent(t, queue, 150*time.Millisecond)
		if !ok {
			return out
		}
		if event.name == "async_state" {
			out = append(out, waitEvent(t, event.frame))
		}
	}
}

func runWith(t *testing.T, handler *Handler, body func(options session.RunOptions)) *eventQueue {
	t.Helper()
	run, err := handler.runs.begin("session\x00dev/coder", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	queue := run.subscribe()
	t.Cleanup(func() { run.detach(queue) })
	completed := json.RawMessage(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}`)
	handler.executeRun(run, runnerFunc(func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		body(options)
		return session.RunResult{TurnID: options.TurnID, Event: completed}, nil
	}), json.RawMessage(`"input"`))
	return queue
}

func TestRunStreamCarriesCountsAndBoundedIDsOnChange(t *testing.T) {
	resolver := &asyncResolver{state: AsyncState{Active: 1, IDs: []string{"a"}}}
	handler := newAsyncTestHandler(t, resolver, 0)
	queue := runWith(t, handler, func(session.RunOptions) {
		resolver.set(AsyncState{Active: 2, Unread: 1, IDs: []string{"b", "a"}})
		time.Sleep(60 * time.Millisecond)
	})
	events := asyncEvents(t, queue)
	if len(events) == 0 {
		t.Fatal("no async_state event for a change")
	}
	last := events[len(events)-1]
	if last["active"] != float64(2) || last["unread"] != float64(1) || last["turn_id"] != "turn-1" {
		t.Fatalf("last event = %v", last)
	}
	ids, _ := last["ids"].([]any)
	if len(ids) != 2 || ids[0] != "b" {
		t.Fatalf("ids = %v", last["ids"])
	}
	for key := range last {
		if key != "turn_id" && key != "active" && key != "unread" && key != "ids" {
			t.Fatalf("event carries %q: only counts and ids belong in it", key)
		}
	}
	if resolver.reference != "session" || resolver.profile != "dev/coder" || !resolver.stopped {
		t.Fatalf("watcher: reference %q profile %q stopped %v", resolver.reference, resolver.profile, resolver.stopped)
	}
}

func TestRunStreamPublishesTheInitialAsyncStateOnceWhenTheSessionHasHandles(t *testing.T) {
	resolver := &asyncResolver{state: AsyncState{Active: 2, Unread: 0, IDs: []string{"p", "q"}}}
	handler := newAsyncTestHandler(t, resolver, 0)
	queue := runWith(t, handler, func(session.RunOptions) { time.Sleep(60 * time.Millisecond) })
	events := asyncEvents(t, queue)
	if len(events) != 1 || events[0]["active"] != float64(2) {
		t.Fatalf("events = %v, want exactly the initial state", events)
	}
}

func TestRunStreamSendsNoAsyncStateForASessionWithoutHandles(t *testing.T) {
	resolver := &asyncResolver{}
	handler := newAsyncTestHandler(t, resolver, 0)
	queue := runWith(t, handler, func(session.RunOptions) {
		resolver.set(AsyncState{})
		time.Sleep(30 * time.Millisecond)
	})
	if events := asyncEvents(t, queue); len(events) != 0 {
		t.Fatalf("async_state events = %v, want none", events)
	}
	plain := newAsyncTestHandler(t, &fakeResolver{}, 0)
	if events := asyncEvents(t, runWith(t, plain, func(session.RunOptions) {})); len(events) != 0 {
		t.Fatalf("a resolver without async state sent %v", events)
	}
}

func TestRunStreamCoalescesAsyncStateChangesWithinTheInterval(t *testing.T) {
	resolver := &asyncResolver{state: AsyncState{Active: 1, IDs: []string{"a"}}}
	handler := newAsyncTestHandler(t, resolver, 200*time.Millisecond)
	queue := runWith(t, handler, func(session.RunOptions) {
		time.Sleep(40 * time.Millisecond)
		for i := 2; i <= 40; i++ {
			resolver.set(AsyncState{Active: i, IDs: []string{"a"}})
		}
		time.Sleep(450 * time.Millisecond)
	})
	events := asyncEvents(t, queue)
	if len(events) < 2 || len(events) > 4 {
		t.Fatalf("39 changes produced %d events, want a few coalesced ones", len(events))
	}
	if events[len(events)-1]["active"] != float64(40) {
		t.Fatalf("the last event must carry the final state, got %v", events[len(events)-1])
	}
}

func TestAsyncStateIsReplayedToAClientThatAttachesLate(t *testing.T) {
	handler := newAsyncTestHandler(t, &fakeResolver{}, 0)
	run, err := handler.runs.begin("session\x00default", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	run.setAsyncState(AsyncState{Active: 3, Unread: 1, IDs: []string{"x", "y", "z"}})
	late := run.subscribe()
	defer run.detach(late)
	replayed := asyncEvents(t, late)
	if len(replayed) != 1 || replayed[0]["active"] != float64(3) {
		t.Fatalf("late client saw %v, want the current async_state", replayed)
	}
	run.setAsyncState(AsyncState{Active: 3, Unread: 1, IDs: []string{"x", "y", "z"}})
	if more := asyncEvents(t, late); len(more) != 0 {
		t.Fatalf("an unchanged state was published again: %v", more)
	}
	run.finish("completed", failureEvents("done"))
	run.setAsyncState(AsyncState{Active: 9})
	if strings.Contains(string(mustJSON(t, run.asyncState)), `"active":9`) {
		t.Fatal("a finished run accepted a new async state")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
