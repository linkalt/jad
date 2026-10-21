package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/session"
)

// TestStalledSSEReaderHoldsHandlerUntilItsConnectionCloses: a client sends a run request
// and never reads. The run's terminal frames are always queued whatever their size, so
// the response write blocks in the kernel socket buffer. The server has no write deadline
// (WriteTimeout stays zero so SSE is not cut), so the handler goroutine, its queue and the
// frames stay alive after the run is over, until the peer closes. The test records that
// and fails only if the goroutine survives the peer closing.
func TestStalledSSEReaderHoldsHandlerUntilItsConnectionCloses(t *testing.T) {
	text := strings.Repeat("x", 16<<20)
	completed, err := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "resp_1", "status": "completed",
		"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	runner := runnerFunc(func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		return session.RunResult{TurnID: options.TurnID, Event: completed}, nil
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "s1", Runner: runner}}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 1 << 20, RunStateRetention: 50 * time.Millisecond})
	defer handler.Close()
	server := httptest.NewServer(handler)
	defer server.Close()

	before := settledGoroutines(0)
	var baseHeap runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&baseHeap)
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"input":"hello"}`
	fmt.Fprintf(conn, "POST /api/sessions/s1/run HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", testAPIToken, len(body), body)

	started := false
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		running := handler.runs.current("s1\x00") != nil
		if running {
			started = true
		}
		if started && !running {
			break
		}
	}
	if !started {
		t.Fatal("the run never started")
	}
	time.Sleep(300 * time.Millisecond)
	stalled := runtime.NumGoroutine()
	var heap runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&heap)
	t.Logf("goroutines before=%d while the peer stalls=%d, heap above baseline=%d KiB for a %d KiB message", before, stalled, (int64(heap.HeapAlloc)-int64(baseHeap.HeapAlloc))>>10, len(text)>>10)
	if stalled <= before {
		t.Skip("the handler goroutine already ended; the write did not block on this machine")
	}

	_ = conn.Close()
	after := settledGoroutines(before)
	for deadline := time.Now().Add(30 * time.Second); after > before && time.Now().Before(deadline); after = settledGoroutines(before) {
	}
	if after > before {
		buffer := make([]byte, 1<<16)
		t.Fatalf("goroutines %d -> %d after the stalled peer closed\n%s", before, after, buffer[:runtime.Stack(buffer, true)])
	}
}
