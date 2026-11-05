package triggers

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// clock is a settable time source for the replay tests.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(by time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(by)
	c.mu.Unlock()
}

// replayManager serves one webhook trigger whose runs end at once, with the given replay
// settings, on a clock the test moves.
func replayManager(t *testing.T, windowMS int64, timestampHeader string) (http.Handler, *clock) {
	t.Helper()
	runner := newFakeRunner()
	close(runner.release)
	clock := &clock{now: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	config := settings(webhookEntry())
	config.HistoryLimit = 100
	config.ReplayWindowMS, config.TimestampHeader = windowMS, timestampHeader
	manager, err := New(Options{Settings: config, Environment: map[string]string{"CI_SECRET": "s3cret"}, Runner: runner, APIToken: "api-token", Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return manager.Wrap(http.NotFoundHandler()), clock
}

// deliver posts body with the given headers, then waits for the run it may have started
// to end, so the next delivery never meets a busy trigger.
func deliver(t *testing.T, handler http.Handler, body string, headers map[string]string) int {
	t.Helper()
	_, before := list(t, handler)
	request := httptest.NewRequest(http.MethodPost, "/api/triggers/ci", strings.NewReader(body))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code == http.StatusAccepted {
		waitRuns(t, handler, len(before)+1)
	}
	return recorder.Code
}

// Without replay settings a captured request can be sent again and starts another run: the
// default is unchanged.
func TestWebhookRepeatIsAcceptedByDefault(t *testing.T) {
	handler, _ := replayManager(t, 0, "")
	headers := map[string]string{"X-Hub-Signature-256": sign("s3cret", "{}")}
	for attempt := range 2 {
		if code := deliver(t, handler, "{}", headers); code != http.StatusAccepted {
			t.Fatalf("delivery %d = %d", attempt, code)
		}
	}
}

// With replay_window_ms a signature accepted once is refused for the window, and only for
// the trigger that accepted it.
func TestWebhookRepeatInsideTheReplayWindowIsRefused(t *testing.T) {
	handler, clock := replayManager(t, 60_000, "")
	headers := map[string]string{"X-Hub-Signature-256": sign("s3cret", "{}")}
	if code := deliver(t, handler, "{}", headers); code != http.StatusAccepted {
		t.Fatalf("first delivery = %d", code)
	}
	clock.advance(59 * time.Second)
	if code := deliver(t, handler, "{}", headers); code != http.StatusConflict {
		t.Fatalf("repeat inside the window = %d, want 409", code)
	}
	if code := deliver(t, handler, "{ }", map[string]string{"X-Hub-Signature-256": sign("s3cret", "{ }")}); code != http.StatusAccepted {
		t.Fatalf("another body = %d", code)
	}
	clock.advance(2 * time.Second)
	if code := deliver(t, handler, "{}", headers); code != http.StatusAccepted {
		t.Fatalf("repeat after the window = %d, want 202", code)
	}
}

// With timestamp_header the signature covers the time, an old or future time is refused
// and a repeat inside the window is refused, so a captured request cannot run again at all.
func TestWebhookTimestampBindsTheSignatureToTheTime(t *testing.T) {
	handler, clock := replayManager(t, 300_000, "X-Jad-Timestamp")
	at := func(when time.Time, body string) map[string]string {
		stamp := strconv.FormatInt(when.Unix(), 10)
		return map[string]string{"X-Jad-Timestamp": stamp, "X-Hub-Signature-256": sign("s3cret", stamp+"."+body)}
	}
	start := clock.Now()
	captured := at(start, "{}")
	for name, test := range map[string]struct {
		headers map[string]string
		want    int
	}{
		"body alone signed": {map[string]string{"X-Jad-Timestamp": strconv.FormatInt(start.Unix(), 10), "X-Hub-Signature-256": sign("s3cret", "{}")}, http.StatusUnauthorized},
		"no timestamp":      {map[string]string{"X-Hub-Signature-256": sign("s3cret", "{}")}, http.StatusUnauthorized},
		"not a number":      {map[string]string{"X-Jad-Timestamp": "soon", "X-Hub-Signature-256": sign("s3cret", "soon.{}")}, http.StatusUnauthorized},
		"too old":           {at(start.Add(-301*time.Second), "{}"), http.StatusUnauthorized},
		"too far ahead":     {at(start.Add(301*time.Second), "{}"), http.StatusUnauthorized},
		"far off value":     {map[string]string{"X-Jad-Timestamp": "-9223372036854775808", "X-Hub-Signature-256": sign("s3cret", "-9223372036854775808.{}")}, http.StatusUnauthorized},
		"changed timestamp": {map[string]string{"X-Jad-Timestamp": strconv.FormatInt(start.Unix()+1, 10), "X-Hub-Signature-256": captured["X-Hub-Signature-256"]}, http.StatusUnauthorized},
	} {
		if code := deliver(t, handler, "{}", test.headers); code != test.want {
			t.Errorf("%s = %d, want %d", name, code, test.want)
		}
	}
	if code := deliver(t, handler, "{}", captured); code != http.StatusAccepted {
		t.Fatalf("signed delivery = %d", code)
	}
	clock.advance(299 * time.Second)
	if code := deliver(t, handler, "{}", captured); code != http.StatusConflict {
		t.Fatalf("replay inside the window = %d, want 409", code)
	}
	clock.advance(2 * time.Second)
	if code := deliver(t, handler, "{}", captured); code != http.StatusUnauthorized {
		t.Fatalf("replay after the window = %d, want 401", code)
	}
	if code := deliver(t, handler, "{}", at(clock.Now(), "{}")); code != http.StatusAccepted {
		t.Fatalf("same body with a new time = %d", code)
	}
}

// The record of accepted signatures drops what the window no longer covers.
func TestDeliveredSignaturesExpire(t *testing.T) {
	clock := &clock{now: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	manager := &Manager{settings: sessionfs.TriggerSettings{ReplayWindowMS: 1000}, now: clock.Now, delivered: map[string]time.Time{}}
	for index := range 5 {
		manager.firstDelivery("ci", []byte{byte(index)})
	}
	clock.advance(1001 * time.Millisecond)
	manager.firstDelivery("ci", []byte{9})
	if len(manager.delivered) != 1 {
		t.Fatalf("delivered holds %d signatures, want 1", len(manager.delivered))
	}
	if manager.firstDelivery("other", []byte{9}) != true {
		t.Fatal("a signature accepted by one trigger was refused by another")
	}
}
