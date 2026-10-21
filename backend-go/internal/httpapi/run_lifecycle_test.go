package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/session"
)

func settledGoroutines(limit int) int {
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > limit && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}

// TestRunCancelCyclesLeaveNoGoroutines starts a run that blocks until it is cancelled,
// attaches a streaming client, stops the run over HTTP and waits for the stream to end;
// 100 cycles must not grow the goroutine count. Retention is zero so a retired run
// leaves no timer behind.
func TestRunCancelCyclesLeaveNoGoroutines(t *testing.T) {
	runner := runnerFunc(func(ctx context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		<-ctx.Done()
		return session.RunResult{}, ctx.Err()
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "s1", Runner: runner}}
	handler := testHandler(t, resolver, Config{
		APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 1 << 20,
		RunStateRetention: time.Nanosecond,
	})
	defer handler.Close()
	server := httptest.NewServer(handler)
	defer server.Close()
	client := server.Client()

	cycle := func() {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/s1/run", strings.NewReader(`{"input":"hello"}`))
		request.Header.Set("Authorization", "Bearer "+testAPIToken)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			buffer := make([]byte, 4096)
			for {
				if _, err := response.Body.Read(buffer); err != nil {
					return
				}
			}
		}()
		stop, _ := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/s1/run/stop", nil)
		stop.Header.Set("Authorization", "Bearer "+testAPIToken)
		for attempt := 0; attempt < 100; attempt++ {
			reply, err := client.Do(stop)
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Stop struct {
					Stopped bool `json:"stopped"`
				} `json:"stop"`
			}
			_ = json.NewDecoder(reply.Body).Decode(&body)
			reply.Body.Close()
			if body.Stop.Stopped {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not end after the run was stopped")
		}
		response.Body.Close()
	}
	cycle()
	client.CloseIdleConnections()
	time.Sleep(200 * time.Millisecond)
	before := runtime.NumGoroutine()
	for index := 0; index < 100; index++ {
		cycle()
	}
	client.CloseIdleConnections()
	if after := settledGoroutines(before); after > before {
		buffer := make([]byte, 1<<16)
		t.Fatalf("goroutines %d -> %d after 100 run/cancel cycles\n%s", before, after, buffer[:runtime.Stack(buffer, true)])
	}
}

// TestSlowSubscriberIsDroppedAndDoesNotHoldBackThePublisher: a subscriber that never reads
// overflows its byte budget and is removed from the run; a second, draining subscriber
// still gets every frame, and publishing never blocks.
func TestSlowSubscriberIsDroppedAndDoesNotHoldBackThePublisher(t *testing.T) {
	registry := newRunRegistry(time.Minute, 8<<10, 0)
	run, err := registry.begin("key", "turn")
	if err != nil {
		t.Fatal(err)
	}
	slow := run.subscribe()
	fast := run.subscribe()
	received := make(chan int, 1)
	go func() {
		count := 0
		for {
			if _, ok := fast.next(context.Background()); !ok {
				received <- count
				return
			}
			count++
		}
	}()
	started := time.Now()
	for revision := 0; revision < 200; revision++ {
		if err := run.publishMessage(benchmarkMessage(1, 1024, revision)); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("200 publishes took %v with a stalled subscriber", elapsed)
	}
	run.mu.Lock()
	_, slowAttached := run.subscribers[slow]
	_, fastAttached := run.subscribers[fast]
	run.mu.Unlock()
	if slowAttached {
		t.Fatal("the stalled subscriber is still attached after overflowing its budget")
	}
	if !fastAttached {
		t.Fatal("the draining subscriber was dropped")
	}
	slow.mu.Lock()
	closed, queued := slow.closed, len(slow.queue)
	slow.mu.Unlock()
	if !closed || queued != 0 {
		t.Fatalf("stalled queue closed=%v queued=%d, want closed and emptied", closed, queued)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		fast.mu.Lock()
		drained := len(fast.queue) == 0
		fast.mu.Unlock()
		if drained {
			break
		}
	}
	run.finish("completed", nil)
	fast.close()
	if got := <-received; got < 200 {
		t.Fatalf("draining subscriber received %d frames, want at least 200", got)
	}
}
