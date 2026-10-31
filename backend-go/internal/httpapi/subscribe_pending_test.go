package httpapi

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/serverconfig"
)

func pendingTestRun(t *testing.T, maxQueue, textBytes, memoryLimit int) *activeRun {
	t.Helper()
	registry := newRunRegistry(time.Minute, maxQueue, 0)
	run, err := registry.begin("key", "turn")
	if err != nil {
		t.Fatal(err)
	}
	cache := newRunContentCache(memoryLimit, serverconfig.DefaultRunContentWriteBatchBytes, time.Duration(serverconfig.DefaultRunContentFlushIntervalMS)*time.Millisecond)
	cache.SetPartialPath(filepath.Join(t.TempDir(), "responses.partial"))
	run.setContentCache(cache)
	t.Cleanup(run.disposeContent)
	if err := run.publishMessage(webUIMessage{ID: "turn", Sender: "assistant", Status: "streaming"}); err != nil {
		t.Fatal(err)
	}
	run.cacheContent(strings.Repeat("a", textBytes))
	return run
}

// drainQueue reads what is queued, stopping when the queue is quiet or closed.
func drainQueue(queue *eventQueue) []streamEvent {
	var events []streamEvent
	for {
		event, ok, quiet := queue.nextOrIdle(context.Background(), 20*time.Millisecond)
		if !ok || quiet {
			return events
		}
		events = append(events, event)
	}
}

func pendingEventNames(events []streamEvent) string {
	names := make([]string, len(events))
	for index, event := range events {
		names[index] = event.name
	}
	return strings.Join(names, ",")
}

func hasEvent(events []streamEvent, name string) bool {
	for _, event := range events {
		if event.name == name {
			return true
		}
	}
	return false
}

func usageValue(t *testing.T, event streamEvent) int {
	t.Helper()
	payload := strings.TrimSuffix(strings.SplitN(string(event.frame), "data: ", 2)[1], "\n\n")
	var body struct {
		Usage int `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		t.Fatal(err)
	}
	return body.Usage
}

func TestEventsPublishedDuringASlowSnapshotArriveAfterItInOrder(t *testing.T) {
	run := pendingTestRun(t, 1<<20, 64<<10, 1<<10)
	if err := run.publish("usage", map[string]any{"usage": 0}); err != nil {
		t.Fatal(err)
	}
	pending := run.beginSubscribe()
	for value := 1; value <= 5; value++ {
		if err := run.publish("usage", map[string]any{"usage": value}); err != nil {
			t.Fatal(err)
		}
	}
	run.cacheContent("late text the snapshot must not contain")
	if got := drainQueue(pending.queue); pendingEventNames(got) != "run" {
		t.Fatalf("before the snapshot the queue holds %q, want only run", pendingEventNames(got))
	}
	pending.finish()
	events := drainQueue(pending.queue)
	if pendingEventNames(events) != "ui.message,usage,usage,usage,usage,usage,usage" {
		t.Fatalf("events = %s", pendingEventNames(events))
	}
	if strings.Contains(string(events[0].frame), "late text") {
		t.Fatal("the snapshot contains text appended after its point")
	}
	for index, want := range []int{0, 1, 2, 3, 4, 5} {
		if got := usageValue(t, events[1+index]); got != want {
			t.Fatalf("usage frame %d = %d, want %d", index, got, want)
		}
	}
}

func TestSnapshotKeepsTheContentOfItsPointWhenTheCacheIsClosedMeanwhile(t *testing.T) {
	run := pendingTestRun(t, 1<<20, 64<<10, 1<<10)
	pending := run.beginSubscribe()
	run.setCommittedMessage(webUIMessage{ID: "turn", Sender: "assistant", Status: "streaming"})
	run.disposeContent()
	pending.finish()
	events := drainQueue(pending.queue)
	if len(events) < 2 || events[1].name != "ui.message" {
		t.Fatalf("events = %s", pendingEventNames(events))
	}
	if got := strings.Count(string(events[1].frame), "a"); got < 64<<10 {
		t.Fatalf("snapshot lost its content: %d bytes of text", got)
	}
}

func TestTerminalFramesDuringAPendingSnapshotFollowIt(t *testing.T) {
	run := pendingTestRun(t, 1<<20, 4<<10, 1<<10)
	pending := run.beginSubscribe()
	done, _ := makeSSEEvent("done", map[string]any{})
	completed, _ := makeSSEEvent("completed", map[string]any{"ok": true})
	run.finish("completed", []streamEvent{completed, done})
	pending.finish()
	if got := pendingEventNames(drainQueue(pending.queue)); got != "run,ui.message,completed,done" {
		t.Fatalf("events = %s", got)
	}
}

func TestUnsubscribeDuringAPendingSnapshotDropsEverythingAndReleasesTheCache(t *testing.T) {
	run := pendingTestRun(t, 1<<20, 4<<10, 1<<10)
	cache := run.content
	pending := run.beginSubscribe()
	if err := run.publish("usage", map[string]any{"usage": 1}); err != nil {
		t.Fatal(err)
	}
	run.detach(pending.queue)
	run.disposeContent()
	pending.finish()
	if events := drainQueue(pending.queue); len(events) != 0 {
		t.Fatalf("a detached queue delivered %s", pendingEventNames(events))
	}
	run.mu.Lock()
	_, registered := run.subscribers[pending.queue]
	run.mu.Unlock()
	if registered {
		t.Fatal("the detached queue is still registered")
	}
	if cache.pins.Load() != 0 || !cache.closed || cache.file != nil {
		t.Fatalf("cache not released: pins=%d closed=%v file=%v", cache.pins.Load(), cache.closed, cache.file)
	}
}

func TestSlowPendingSubscriberIsDroppedWhenHeldEventsOutgrowTheBudget(t *testing.T) {
	run := pendingTestRun(t, 4<<10, 100, 1<<10)
	pending := run.beginSubscribe()
	for value := 0; value < 400; value++ {
		_ = run.publish("usage", map[string]any{"usage": value})
	}
	run.mu.Lock()
	_, registered := run.subscribers[pending.queue]
	run.mu.Unlock()
	if registered {
		t.Fatal("an overflowing pending subscriber stays registered")
	}
	pending.finish()
	if _, ok := pending.queue.next(context.Background()); ok {
		t.Fatal("the dropped queue still delivers")
	}
}

func TestFullStateFrameOutgrowingTheHoldReplacesTheOlderSnapshot(t *testing.T) {
	run := pendingTestRun(t, 4<<10, 100, 1<<10)
	pending := run.beginSubscribe()
	big := webUIMessage{ID: "turn", Sender: "assistant", Status: "complete", Parts: textParts(strings.Repeat("z", 8<<10)), Text: strings.Repeat("z", 8<<10)}
	if err := run.publishMessage(big); err != nil {
		t.Fatal(err)
	}
	pending.finish()
	events := drainQueue(pending.queue)
	if pendingEventNames(events) != "ui.message" || !strings.Contains(string(events[0].frame), "zzzz") {
		t.Fatalf("events = %s", pendingEventNames(events))
	}
}

func TestManyConcurrentSubscribersSeeAnUninterruptedStream(t *testing.T) {
	run := pendingTestRun(t, 1<<22, 256<<10, 1<<10)
	const publishes, subscribers = 400, 24
	before := runtime.NumGoroutine()
	var group sync.WaitGroup
	results := make([][]streamEvent, subscribers)
	ready := make(chan struct{})
	for index := 0; index < subscribers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-ready
			queue := run.subscribe()
			results[index] = drainUntilDone(queue)
			run.detach(queue)
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		close(ready)
		for value := 1; value <= publishes; value++ {
			_ = run.publish("usage", map[string]any{"usage": value})
			if value%50 == 0 {
				run.cacheContent("more")
			}
		}
		uiEvent, _ := makeSSEEvent("ui.message", map[string]any{"message": webUIMessage{ID: "turn", Sender: "assistant", Status: "complete"}})
		done, _ := makeSSEEvent("done", map[string]any{})
		run.finish("completed", []streamEvent{uiEvent, done})
	}()
	group.Wait()
	for index, events := range results {
		if len(events) < 3 || events[0].name != "run" || events[len(events)-1].name != "done" || !hasEvent(events[1:len(events)-1], "ui.message") {
			t.Fatalf("subscriber %d: events = %s", index, pendingEventNames(events))
		}
		last := -1
		for _, event := range events[1 : len(events)-1] {
			if event.name != "usage" {
				continue
			}
			value := usageValue(t, event)
			if last >= 0 && value != last+1 {
				t.Fatalf("subscriber %d: usage %d follows %d", index, value, last)
			}
			last = value
		}
		if last != publishes {
			t.Fatalf("subscriber %d ended at usage %d, want %d", index, last, publishes)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
}

func drainUntilDone(queue *eventQueue) []streamEvent {
	var events []streamEvent
	for {
		event, ok, quiet := queue.nextOrIdle(context.Background(), 5*time.Second)
		if !ok || quiet {
			return events
		}
		events = append(events, event)
		if event.name == "done" {
			return events
		}
	}
}
