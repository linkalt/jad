package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/session"
)

func stalledReaderFixture(t *testing.T, writeTimeout time.Duration) (*httptest.Server, *Handler) {
	t.Helper()
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
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 1 << 20, RunStateRetention: 50 * time.Millisecond, SSEWriteTimeout: writeTimeout})
	server := httptest.NewServer(handler)
	t.Cleanup(func() { server.Close(); handler.Close() })
	return server, handler
}

// TestStalledSSEReaderIsDroppedAtTheWriteDeadline: a client that sends the run request
// and never reads must lose its handler and queue once the per-write deadline passes,
// without its connection being closed first.
func TestStalledSSEReaderIsDroppedAtTheWriteDeadline(t *testing.T) {
	const deadline = 300 * time.Millisecond
	server, handler := stalledReaderFixture(t, deadline)
	before := settledGoroutines(0)
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"input":"hello"}`
	fmt.Fprintf(conn, "POST /api/sessions/s1/run HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", testAPIToken, len(body), body)

	started := false
	for limit := time.Now().Add(30 * time.Second); time.Now().Before(limit); time.Sleep(5 * time.Millisecond) {
		running := handler.runs.current("s1\x00") != nil
		started = started || running
		if started && !running {
			break
		}
	}
	if !started {
		t.Fatal("the run never started")
	}
	begin := time.Now()
	after := settledGoroutines(before)
	for limit := time.Now().Add(10 * time.Second); after > before && time.Now().Before(limit); after = settledGoroutines(before) {
	}
	if after > before {
		buffer := make([]byte, 1<<16)
		t.Fatalf("goroutines %d -> %d: the stalled reader was not dropped\n%s", before, after, buffer[:runtime.Stack(buffer, true)])
	}
	t.Logf("stalled reader released %v after the run ended (deadline %v)", time.Since(begin).Round(10*time.Millisecond), deadline)
}

// TestWriteDeadlineDoesNotCutAHealthyStream: a reader that keeps reading gets the whole
// answer even though the stream outlasts several deadlines.
func TestWriteDeadlineDoesNotCutAHealthyStream(t *testing.T) {
	server, _ := stalledReaderFixture(t, 200*time.Millisecond)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/s1/run", strings.NewReader(`{"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	buffer := make([]byte, 64<<10)
	total := 0
	for {
		n, err := response.Body.Read(buffer)
		total += n
		time.Sleep(time.Millisecond)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the stream was cut after %d bytes: %v", total, err)
		}
	}
	if total < 16<<20 {
		t.Fatalf("read %d bytes, want the whole 16 MiB answer", total)
	}
}

// TestWriteDeadlineIsClearedAfterTheStream: the same keep-alive connection must serve a
// later request after a stream ended, so an armed deadline must not outlive serveSSE.
func TestWriteDeadlineIsClearedAfterTheStream(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		return session.RunResult{TurnID: options.TurnID, Event: json.RawMessage(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}`)}, nil
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "s1", Runner: runner}}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 1 << 20, RunStateRetention: 50 * time.Millisecond, SSEWriteTimeout: 150 * time.Millisecond})
	server := httptest.NewServer(handler)
	defer server.Close()
	defer handler.Close()
	client := &http.Client{Transport: &http.Transport{MaxIdleConns: 1}}
	for round := 0; round < 2; round++ {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/s1/run", strings.NewReader(`{"input":"hello"}`))
		request.Header.Set("Authorization", "Bearer "+testAPIToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		time.Sleep(400 * time.Millisecond)
	}
	health, err := http.NewRequest(http.MethodGet, server.URL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(health)
	if err != nil {
		t.Fatalf("request on the reused connection after the deadline time: %v", err)
	}
	response.Body.Close()
}
