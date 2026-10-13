package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/session"
)

// slowRunner stays quiet for the given duration, like a long tool call, then
// finishes the run.
func slowRunner(quiet time.Duration) runnerFunc {
	completed := json.RawMessage(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`)
	return func(ctx context.Context, _ json.RawMessage, _ session.RunOptions) (session.RunResult, error) {
		select {
		case <-time.After(quiet):
		case <-ctx.Done():
			return session.RunResult{}, ctx.Err()
		}
		return session.RunResult{Event: completed, TurnID: "turn-1"}, nil
	}
}

// runStreamBody starts a run against a real server and returns the raw event
// stream text once the run has ended.
func runStreamBody(t *testing.T, heartbeat time.Duration, quiet time.Duration) string {
	t.Helper()
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "s1", Runner: slowRunner(quiet)}}
	handler := testHandler(t, resolver, Config{
		APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096,
		RunStateRetention: time.Minute, SSEHeartbeatInterval: heartbeat,
	})
	defer handler.Close()
	server := httptest.NewServer(handler)
	defer server.Close()

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
	var body strings.Builder
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		body.WriteString(scanner.Text())
		body.WriteString("\n")
	}
	return body.String()
}

func TestQuietRunStreamGetsKeepAliveCommentFramesThatCarryNoEvent(t *testing.T) {
	body := runStreamBody(t, 40*time.Millisecond, 400*time.Millisecond)
	frames := 0
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == ": keep-alive" {
			frames++
		}
	}
	if frames < 2 {
		t.Fatalf("keep-alive frames = %d, want at least 2 during the 0.4s quiet period\n%s", frames, body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, ": ") && line != ": keep-alive" {
			t.Fatalf("unexpected comment line %q", line)
		}
	}
	// Real events still arrive and the run still completes normally.
	if got := strings.Count(body, "event: completed"); got != 1 {
		t.Fatalf("completed events = %d, want 1\n%s", got, body)
	}
	if got := strings.Count(body, "event: done"); got != 1 {
		t.Fatalf("done events = %d, want 1\n%s", got, body)
	}
}

func TestZeroHeartbeatIntervalSendsNoKeepAliveFrames(t *testing.T) {
	body := runStreamBody(t, 0, 200*time.Millisecond)
	if strings.Contains(body, "keep-alive") {
		t.Fatalf("a zero interval must disable the heartbeat\n%s", body)
	}
	if !strings.Contains(body, "event: done") {
		t.Fatalf("run did not finish\n%s", body)
	}
}

func TestNewHandlerRejectsAnOutOfRangeHeartbeatInterval(t *testing.T) {
	for _, interval := range []time.Duration{-time.Millisecond, 5*time.Minute + time.Millisecond} {
		_, err := NewHandler(Config{
			APIToken: testAPIToken, MaxBodyBytes: 1024, SSEMaxBufferedBytes: 1024,
			RunStateRetention: time.Minute, MaxPageTurns: 50, SSEHeartbeatInterval: interval,
		}, &fakeResolver{})
		if err == nil {
			t.Errorf("NewHandler accepted heartbeat interval %v", interval)
		}
	}
}

func TestEventQueueNextOrIdleReportsQuietAndRestartsAfterEachEvent(t *testing.T) {
	queue := newEventQueue(4096)
	ctx := context.Background()

	started := time.Now()
	if _, ok, quiet := queue.nextOrIdle(ctx, 30*time.Millisecond); !ok || !quiet || time.Since(started) < 25*time.Millisecond {
		t.Fatalf("nextOrIdle on an empty queue: ok=%v quiet=%v after %v, want a quiet signal after the interval", ok, quiet, time.Since(started))
	}
	event, _ := makeSSEEvent("ui.message", map[string]string{"k": "v"})
	queue.push(event, false)
	got, ok, quiet := queue.nextOrIdle(ctx, 30*time.Millisecond)
	if !ok || quiet || got.name != "ui.message" {
		t.Fatalf("nextOrIdle with a queued event = %q ok=%v quiet=%v", got.name, ok, quiet)
	}
	queue.close()
	if _, ok, quiet := queue.nextOrIdle(ctx, 30*time.Millisecond); ok || quiet {
		t.Fatalf("nextOrIdle on a closed queue: ok=%v quiet=%v, want the end of the stream", ok, quiet)
	}
}
