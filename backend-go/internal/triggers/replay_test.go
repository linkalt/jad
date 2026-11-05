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
	config.ReplayWindowMS, config.TimestampHeader, config.ReplayMaxRecords = windowMS, timestampHeader, 100
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
	manager := &Manager{settings: sessionfs.TriggerSettings{ReplayWindowMS: 1000, ReplayMaxRecords: 100}, now: clock.Now, delivered: map[string]time.Time{}}
	for index := range 5 {
		manager.firstDelivery("ci", []byte{byte(index)}, time.Time{})
	}
	// The window and the millisecond a record outlives it, and one more.
	clock.advance(1002 * time.Millisecond)
	manager.firstDelivery("ci", []byte{9}, time.Time{})
	if len(manager.delivered) != 1 || len(manager.expiries) != 1 {
		t.Fatalf("delivered holds %d signatures and %d expiries, want 1", len(manager.delivered), len(manager.expiries))
	}
	if _, first, _ := manager.firstDelivery("other", []byte{9}, time.Time{}); !first {
		t.Fatal("a signature accepted by one trigger was refused by another")
	}
}

// A full replay record refuses a new signature instead of dropping a live record, which
// would let that request run again; it takes new ones once records expire. A repeat of a
// recorded signature is still a repeat. Forgotten records do not grow the heap for ever.
func TestReplayRecordIsBounded(t *testing.T) {
	clock := &clock{now: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	manager := &Manager{settings: sessionfs.TriggerSettings{ReplayWindowMS: 1000, ReplayMaxRecords: 3}, now: clock.Now, delivered: map[string]time.Time{}}
	for index := range 3 {
		if _, first, full := manager.firstDelivery("ci", []byte{byte(index)}, time.Time{}); !first || full {
			t.Fatalf("signature %d: first %v full %v", index, first, full)
		}
	}
	if _, first, full := manager.firstDelivery("ci", []byte{9}, time.Time{}); first || !full {
		t.Fatalf("a new signature with the record full: first %v full %v, want refused as full", first, full)
	}
	if _, first, full := manager.firstDelivery("ci", []byte{0}, time.Time{}); first || full {
		t.Fatalf("a repeat with the record full: first %v full %v, want a repeat", first, full)
	}
	// The window and the millisecond a record outlives it, and one more.
	clock.advance(1002 * time.Millisecond)
	if _, first, full := manager.firstDelivery("ci", []byte{9}, time.Time{}); !first || full {
		t.Fatalf("after the records expired: first %v full %v", first, full)
	}
	for index := range 500 {
		key, _, _ := manager.firstDelivery("busy", []byte{byte(index), byte(index >> 8)}, time.Time{})
		manager.forgetDelivery(key)
	}
	if len(manager.delivered) != 1 || len(manager.expiries) > 2*len(manager.delivered)+65 {
		t.Fatalf("after 500 forgotten records: %d records, %d heap entries", len(manager.delivered), len(manager.expiries))
	}
}

// A signed request that started no run (the trigger was busy) does not use up its
// signature: the sender's retry after the run ended is accepted, and only once.
func TestWebhookRefusedFiringKeepsTheSignature(t *testing.T) {
	runner := newFakeRunner()
	clock := &clock{now: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	config := settings(webhookEntry())
	config.HistoryLimit, config.ReplayWindowMS, config.ReplayMaxRecords = 100, 60_000, 100
	manager, err := New(Options{Settings: config, Environment: map[string]string{"CI_SECRET": "s3cret"}, Runner: runner, APIToken: "api-token", Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	handler := manager.Wrap(http.NotFoundHandler())
	if response := post(handler, "/api/triggers/ci", "a", sign("s3cret", "a")); response.Code != http.StatusAccepted {
		t.Fatalf("first = %d %s", response.Code, response.Body)
	}
	<-runner.started
	busy := post(handler, "/api/triggers/ci", "b", sign("s3cret", "b"))
	if busy.Code != http.StatusConflict || !strings.Contains(busy.Body.String(), `"status":"skipped"`) {
		t.Fatalf("busy = %d %s", busy.Code, busy.Body)
	}
	close(runner.release)
	waitRuns(t, handler, 2)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if triggers, _ := list(t, handler); triggers[0]["running"] == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first run did not end")
		}
	}
	if code := deliver(t, handler, "b", map[string]string{"X-Hub-Signature-256": sign("s3cret", "b")}); code != http.StatusAccepted {
		t.Fatalf("retry after the run ended = %d, want 202", code)
	}
	if code := deliver(t, handler, "b", map[string]string{"X-Hub-Signature-256": sign("s3cret", "b")}); code != http.StatusConflict {
		t.Fatalf("repeat of the accepted retry = %d, want 409", code)
	}
}

// A sender whose clock runs ahead gets a timestamp the server accepts until window after
// that time, so the record of the request must last as long: a replay is refused at every
// moment the timestamp is still valid.
func TestWebhookTimestampAheadCannotBeReplayed(t *testing.T) {
	handler, clock := replayManager(t, 300_000, "X-Jad-Timestamp")
	stamp := strconv.FormatInt(clock.Now().Add(200*time.Second).Unix(), 10)
	captured := map[string]string{"X-Jad-Timestamp": stamp, "X-Hub-Signature-256": sign("s3cret", stamp+".{}")}
	if code := deliver(t, handler, "{}", captured); code != http.StatusAccepted {
		t.Fatalf("delivery ahead by 200 s = %d", code)
	}
	for _, after := range []time.Duration{299 * time.Second, 2 * time.Second, 198 * time.Second} {
		clock.advance(after)
		if code := deliver(t, handler, "{}", captured); code != http.StatusConflict {
			t.Fatalf("replay %v later = %d, want 409", after, code)
		}
	}
	clock.advance(2 * time.Second)
	if code := deliver(t, handler, "{}", captured); code != http.StatusUnauthorized {
		t.Fatalf("replay after the timestamp's window = %d, want 401", code)
	}
}

// A replay just past the window, inside the millisecond the timestamp's age still counts
// as the window, finds the record: it is kept one millisecond longer than the window.
func TestWebhookReplayInTheWindowsLastMillisecondIsRefused(t *testing.T) {
	handler, clock := replayManager(t, 300_000, "X-Jad-Timestamp")
	stamp := strconv.FormatInt(clock.Now().Add(200*time.Second).Unix(), 10)
	captured := map[string]string{"X-Jad-Timestamp": stamp, "X-Hub-Signature-256": sign("s3cret", stamp+".{}")}
	if code := deliver(t, handler, "{}", captured); code != http.StatusAccepted {
		t.Fatalf("first delivery = %d", code)
	}
	// 200 s to the timestamp, the 300 s window, and half a millisecond.
	clock.advance(500*time.Second + 500*time.Microsecond)
	if code := deliver(t, handler, "{}", captured); code != http.StatusConflict {
		t.Fatalf("replay half a millisecond past the window = %d, want 409", code)
	}
	clock.advance(time.Millisecond)
	if code := deliver(t, handler, "{}", captured); code != http.StatusUnauthorized {
		t.Fatalf("replay past the timestamp's window = %d, want 401", code)
	}
}

// The timestamp's age is compared to the window in milliseconds, not whole seconds.
func TestWebhookTimestampWindowCountsMilliseconds(t *testing.T) {
	handler, clock := replayManager(t, 1500, "X-Jad-Timestamp")
	at := func(seconds int64) map[string]string {
		stamp := strconv.FormatInt(seconds, 10)
		return map[string]string{"X-Jad-Timestamp": stamp, "X-Hub-Signature-256": sign("s3cret", stamp+".{}")}
	}
	start := clock.Now().Unix()
	clock.advance(1400 * time.Millisecond)
	if code := deliver(t, handler, "{}", at(start)); code != http.StatusAccepted {
		t.Fatalf("age 1.4 s in a 1.5 s window = %d, want 202", code)
	}
	clock.advance(200 * time.Millisecond)
	if code := deliver(t, handler, "{}", at(start)); code != http.StatusUnauthorized {
		t.Fatalf("age 1.6 s in a 1.5 s window = %d, want 401", code)
	}
}
