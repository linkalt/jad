package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func enterAsync(q *turnQueue, ctx context.Context, key string) <-chan func() {
	got := make(chan func(), 1)
	go func() {
		leave, err := q.enter(ctx, key)
		if err != nil {
			got <- nil
			return
		}
		got <- leave
	}()
	return got
}

func waitQueueLen(t *testing.T, q *turnQueue, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for q.len(key) != want {
		if time.Now().After(deadline) {
			t.Fatalf("queue length = %d, want %d", q.len(key), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTurnQueueGivesTurnsInArrivalOrder(t *testing.T) {
	var q turnQueue
	first, err := q.enter(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	second := enterAsync(&q, context.Background(), "k")
	waitQueueLen(t, &q, "k", 2)
	third := enterAsync(&q, context.Background(), "k")
	waitQueueLen(t, &q, "k", 3)
	select {
	case <-second:
		t.Fatal("second arrival ran before the first left")
	case <-third:
		t.Fatal("third arrival ran before the first left")
	case <-time.After(20 * time.Millisecond):
	}

	first()
	var leaveSecond func()
	select {
	case leaveSecond = <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("second arrival did not get the turn")
	}
	select {
	case <-third:
		t.Fatal("third arrival overtook the second")
	case <-time.After(20 * time.Millisecond):
	}
	leaveSecond()
	select {
	case leaveThird := <-third:
		leaveThird()
	case <-time.After(2 * time.Second):
		t.Fatal("third arrival did not get the turn")
	}
	if got := q.len("k"); got != 0 {
		t.Fatalf("queue length after all left = %d, want 0", got)
	}
	if len(q.queues) != 0 {
		t.Fatalf("queue map still holds %d keys", len(q.queues))
	}
}

func TestTurnQueueCancelledWaiterLeavesWithoutBlockingTheOnesBehindIt(t *testing.T) {
	var q turnQueue
	first, _ := q.enter(context.Background(), "k")
	ctx, cancel := context.WithCancel(context.Background())
	second := enterAsync(&q, ctx, "k")
	waitQueueLen(t, &q, "k", 2)
	third := enterAsync(&q, context.Background(), "k")
	waitQueueLen(t, &q, "k", 3)

	cancel()
	if leave := <-second; leave != nil {
		t.Fatal("cancelled waiter got a turn")
	}
	waitQueueLen(t, &q, "k", 2)
	first()
	select {
	case leave := <-third:
		leave()
	case <-time.After(2 * time.Second):
		t.Fatal("waiter behind a cancelled one never got the turn")
	}
}

func TestTurnQueueCancelledWaiterAtTheFrontPassesTheTurnOn(t *testing.T) {
	var q turnQueue
	first, _ := q.enter(context.Background(), "k")
	ctx, cancel := context.WithCancel(context.Background())
	second := enterAsync(&q, ctx, "k")
	waitQueueLen(t, &q, "k", 2)
	third := enterAsync(&q, context.Background(), "k")
	waitQueueLen(t, &q, "k", 3)

	// The first one leaves, which hands the turn to the second; the second is
	// cancelled at the same time and must not keep the third waiting.
	first()
	cancel()
	if leave := <-second; leave != nil {
		leave()
	}
	select {
	case leave := <-third:
		leave()
	case <-time.After(2 * time.Second):
		t.Fatal("waiter behind a cancelled front never got the turn")
	}
}

func TestTurnQueueLeaveIsIdempotentAndKeysAreIndependent(t *testing.T) {
	var q turnQueue
	a, _ := q.enter(context.Background(), "a")
	b, err := q.enter(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	next := enterAsync(&q, context.Background(), "a")
	waitQueueLen(t, &q, "a", 2)
	a()
	a()
	leaveNext := <-next
	if leaveNext == nil {
		t.Fatal("waiter did not get the turn")
	}
	if got := q.len("a"); got != 1 {
		t.Fatalf("queue length after a double leave = %d, want 1 (the waiter only)", got)
	}
	if got := q.len("b"); got != 1 {
		t.Fatalf("key b length = %d, want 1", got)
	}
	leaveNext()
	b()
}

func TestTurnQueueDoesNotLoseTurnsUnderContention(t *testing.T) {
	var q turnQueue
	var wg sync.WaitGroup
	inside := 0
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			if i%5 == 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Duration(i)*time.Microsecond)
				defer cancel()
			}
			leave, err := q.enter(ctx, "k")
			if err != nil {
				return
			}
			mu.Lock()
			inside++
			if inside != 1 {
				t.Errorf("%d holders at once", inside)
			}
			mu.Unlock()
			time.Sleep(50 * time.Microsecond)
			mu.Lock()
			inside--
			mu.Unlock()
			leave()
		}(i)
	}
	wg.Wait()
	if len(q.queues) != 0 {
		t.Fatalf("queue map still holds %d keys", len(q.queues))
	}
}

type panickingResolver struct {
	*transcriptLockedResolver
}

func (panickingResolver) Resolve(context.Context, string, string, string) (ResolvedSession, error) {
	panic("resolve exploded")
}

// A panic between taking the transcript lock and registering the run must give the lock back.
func TestRunStartReleasesTheTranscriptWhenResolvePanics(t *testing.T) {
	inner, _ := newIdleTranscriptResolver(t)
	resolver := panickingResolver{inner}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic was swallowed")
			}
		}()
		_, _, _ = handler.tryStartRun(context.Background(), "s", "dev", "")
	}()
	if held := len(inner.transcript); held != 0 {
		t.Fatalf("transcript lock still held after a panic (%d)", held)
	}
}

// A start that arrives while a compaction is queued waits for it and then runs. The
// compaction's gate is cleared before the session lock is given back, so the start
// does not meet it and is not refused with 409.
func TestStartQueuedBehindACompactionRunsAfterIt(t *testing.T) {
	f := newQueuedRunFixture(t)
	f.endQueuedStart()
	<-f.second
	select {
	case <-f.resolver.waiting:
	default:
	}
	compact := f.serve(context.Background(), "/api/sessions/s/compact?profile=dev", "")
	select {
	case <-f.resolver.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("compact did not wait for the transcript")
	}
	start := f.serve(context.Background(), "/api/sessions/s/run?profile=dev", `{"input":"three"}`)
	waitQueueLen(t, &f.handler.startTurns, f.handler.turnKey("s", "dev"), 2)
	close(f.release)
	for name, done := range map[string]<-chan *httptest.ResponseRecorder{"compact": compact, "start": start} {
		select {
		case response := <-done:
			if response.Code != http.StatusOK {
				t.Fatalf("%s status = %d body=%s", name, response.Code, response.Body.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s never finished", name)
		}
	}
	if !f.mutation.compacted {
		t.Fatal("compact did not run")
	}
}

// Compact waits for the transcript outside the session lock, like a run start.
func TestQueuedCompactDoesNotBlockSessionSettingsAndRunsAfterTheRun(t *testing.T) {
	f := newQueuedRunFixture(t)
	f.endQueuedStart()
	select {
	case <-f.second:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled start did not end")
	}
	select {
	case <-f.resolver.waiting:
	default:
	}
	compact := f.serve(context.Background(), "/api/sessions/s/compact?profile=dev", "")
	select {
	case <-f.resolver.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("compact did not wait for the transcript")
	}
	response := f.answer("/api/sessions/s/model-default?profile=dev", `{"model_default":"models/new.json"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("model-default status = %d body=%s", response.Code, response.Body.String())
	}
	if f.mutation.compacted {
		t.Fatal("compact ran while the run still held the transcript")
	}
	close(f.release)
	select {
	case response := <-compact:
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"compacted":true`) {
			t.Fatalf("compact status = %d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued compact never ran after the run ended")
	}
}
