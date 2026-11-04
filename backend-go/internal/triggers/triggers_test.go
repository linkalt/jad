package triggers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/cronexpr"
	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/sessionfs"
)

// fakeRunner records requests; a run blocks until release is closed or its context ends.
type fakeRunner struct {
	mu       sync.Mutex
	requests []headless.Request
	started  chan headless.Request
	release  chan struct{}
	result   headless.Result
	err      error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{started: make(chan headless.Request, 16), release: make(chan struct{}), result: headless.Result{Status: headless.StatusCompleted, TurnID: "t1"}}
}

func (f *fakeRunner) Run(ctx context.Context, request headless.Request) (headless.Result, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	f.started <- request
	select {
	case <-f.release:
		return f.result, f.err
	case <-ctx.Done():
		return headless.Result{Status: headless.StatusAborted, Error: "stopped"}, nil
	}
}

func settings(entries ...sessionfs.TriggerEntry) sessionfs.TriggerSettings {
	return sessionfs.TriggerSettings{Enabled: true, MaxPayloadBytes: 64, HistoryLimit: 3, SignatureHeader: "X-Hub-Signature-256", Entries: entries}
}

func webhookEntry() sessionfs.TriggerEntry {
	return sessionfs.TriggerEntry{Name: "ci", Session: "demo", Profile: "dev/coder", Model: "openai/small", Prompt: "[{trigger} at {time}] CI said: {payload}", WebhookSecretEnv: "CI_SECRET", TimeoutMS: 1500, Location: time.UTC}
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newManager(t *testing.T, runner Runner, entries ...sessionfs.TriggerEntry) *Manager {
	t.Helper()
	manager, err := New(Options{Settings: settings(entries...), Environment: map[string]string{"CI_SECRET": "s3cret"}, Runner: runner, APIToken: "api-token",
		Now: func() time.Time { return time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return manager
}

func post(handler http.Handler, path, body, signature string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if signature != "" {
		request.Header.Set("X-Hub-Signature-256", signature)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func list(t *testing.T, handler http.Handler) (triggers []map[string]any, runs []Record) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/triggers", nil)
	request.Header.Set("Authorization", "Bearer api-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list: %d %s", recorder.Code, recorder.Body)
	}
	var body struct {
		Triggers []map[string]any `json:"triggers"`
		Runs     []Record         `json:"runs"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Triggers, body.Runs
}

// waitRuns waits until the history holds n runs.
func waitRuns(t *testing.T, handler http.Handler, n int) []Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, runs := list(t, handler)
		if len(runs) >= n {
			return runs
		}
		if time.Now().After(deadline) {
			t.Fatalf("history has %d runs, want %d", len(runs), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSignedWebhookStartsTheRunWithThePayload(t *testing.T) {
	runner := newFakeRunner()
	manager := newManager(t, runner, webhookEntry())
	handler := manager.Wrap(http.NotFoundHandler())
	body := `{"conclusion":"failure"}`
	response := post(handler, "/api/triggers/ci", body, sign("s3cret", body))
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"status":"running"`) {
		t.Fatalf("webhook: %d %s", response.Code, response.Body)
	}
	request := <-runner.started
	want := headless.Request{Session: "demo", Profile: "dev/coder", Model: "openai/small", Input: `[ci at 2026-11-04T10:00:00Z] CI said: {"conclusion":"failure"}`, Timeout: 1500 * time.Millisecond}
	if request.Session != want.Session || request.Profile != want.Profile || request.Model != want.Model || request.Input != want.Input || request.Timeout != want.Timeout {
		t.Fatalf("request = %+v, want %+v", request, want)
	}
	triggers, runs := list(t, handler)
	if len(runs) != 0 || triggers[0]["running"] == nil || triggers[0]["webhook"] != true {
		t.Fatalf("while running: triggers %v runs %v", triggers, runs)
	}
	close(runner.release)
	runs = waitRuns(t, handler, 1)
	if runs[0].Status != headless.StatusCompleted || runs[0].TurnID != "t1" || runs[0].Source != SourceWebhook || runs[0].Trigger != "ci" {
		t.Fatalf("run = %+v", runs[0])
	}
}

// A payload is filled in once: placeholders inside it stay as they are.
func TestPayloadIsNotExpandedAgain(t *testing.T) {
	runner := newFakeRunner()
	close(runner.release)
	manager := newManager(t, runner, webhookEntry())
	body := `{trigger}`
	post(manager.Wrap(http.NotFoundHandler()), "/api/triggers/ci", body, sign("s3cret", body))
	if request := <-runner.started; !strings.HasSuffix(request.Input, "CI said: {trigger}") {
		t.Fatalf("input = %q", request.Input)
	}
}

func TestWebhookRefusals(t *testing.T) {
	runner := newFakeRunner()
	scheduled := sessionfs.TriggerEntry{Name: "nightly", Session: "demo", Prompt: "p", Schedule: "0 3 * * *", Location: time.UTC}
	scheduled.Cron, _ = cronexpr.Parse(scheduled.Schedule)
	manager := newManager(t, runner, webhookEntry(), scheduled)
	handler := manager.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	for name, test := range map[string]struct {
		path, body, signature string
		want                  int
	}{
		"no signature":       {"/api/triggers/ci", "{}", "", http.StatusUnauthorized},
		"wrong secret":       {"/api/triggers/ci", "{}", sign("other", "{}"), http.StatusUnauthorized},
		"signed other body":  {"/api/triggers/ci", "{}", sign("s3cret", "{ }"), http.StatusUnauthorized},
		"not hex":            {"/api/triggers/ci", "{}", "sha256=zz", http.StatusUnauthorized},
		"unknown trigger":    {"/api/triggers/nope", "{}", sign("s3cret", "{}"), http.StatusNotFound},
		"schedule only":      {"/api/triggers/nightly", "{}", sign("s3cret", "{}"), http.StatusNotFound},
		"too large":          {"/api/triggers/ci", strings.Repeat("x", 65), sign("s3cret", strings.Repeat("x", 65)), http.StatusRequestEntityTooLarge},
		"other path is next": {"/api/sessions", "{}", "", http.StatusTeapot},
	} {
		if response := post(handler, test.path, test.body, test.signature); response.Code != test.want {
			t.Errorf("%s: %d %s, want %d", name, response.Code, response.Body, test.want)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/triggers/ci", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET webhook = %d", recorder.Code)
	}
	for _, header := range []string{"", "Bearer wrong", "api-token"} {
		request := httptest.NewRequest(http.MethodGet, "/api/triggers", nil)
		request.Header.Set("Authorization", header)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("list with %q = %d", header, recorder.Code)
		}
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.requests) != 0 {
		t.Fatalf("a refused webhook started a run: %+v", runner.requests)
	}
}

// One run per trigger at a time; a second fire is recorded as skipped and answered 409.
func TestBusyTriggerSkipsAndHistoryIsBounded(t *testing.T) {
	runner := newFakeRunner()
	manager := newManager(t, runner, webhookEntry())
	handler := manager.Wrap(http.NotFoundHandler())
	post(handler, "/api/triggers/ci", "a", sign("s3cret", "a"))
	<-runner.started
	for range 3 {
		if response := post(handler, "/api/triggers/ci", "b", sign("s3cret", "b")); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"status":"skipped"`) {
			t.Fatalf("busy: %d %s", response.Code, response.Body)
		}
	}
	close(runner.release)
	var runs []Record
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, runs = list(t, handler); len(runs) > 0 && runs[0].Status != StatusSkipped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run did not end: %+v", runs)
		}
	}
	// History limit 3: the first skipped run was dropped; newest first.
	if len(runs) != 3 || runs[0].Status != headless.StatusCompleted || runs[1].Status != StatusSkipped || runs[2].Status != StatusSkipped {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestRefusedRunIsRecorded(t *testing.T) {
	runner := newFakeRunner()
	runner.err = &headless.RequestError{Status: http.StatusNotFound, Message: "session not found"}
	close(runner.release)
	manager := newManager(t, runner, webhookEntry())
	handler := manager.Wrap(http.NotFoundHandler())
	post(handler, "/api/triggers/ci", "a", sign("s3cret", "a"))
	runs := waitRuns(t, handler, 1)
	if runs[0].Status != StatusRefused || runs[0].Error != "session not found" {
		t.Fatalf("run = %+v", runs[0])
	}
}

func TestScheduleFiresAtTheNextMatchAndCloseStopsTheRun(t *testing.T) {
	runner := newFakeRunner()
	entry := sessionfs.TriggerEntry{Name: "nightly", Session: "demo", Prompt: "nightly {trigger}", Schedule: "30 3 * * *", Location: time.FixedZone("plus2", 2*3600)}
	entry.Cron, _ = cronexpr.Parse(entry.Schedule)
	waits := make(chan time.Duration, 4)
	fire := make(chan time.Time)
	manager, err := New(Options{Settings: settings(entry), Runner: runner,
		Now:   func() time.Time { return time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC) },
		After: func(d time.Duration) <-chan time.Time { waits <- d; return fire }})
	if err != nil {
		t.Fatal(err)
	}
	manager.Start()
	// 00:00 UTC is 02:00 at +2, so 03:30 there is 90 minutes away.
	if wait := <-waits; wait != 90*time.Minute {
		t.Fatalf("first wait = %v", wait)
	}
	fire <- time.Time{}
	if request := <-runner.started; request.Input != "nightly nightly" || request.Session != "demo" {
		t.Fatalf("request = %+v", request)
	}
	<-waits // the scheduler waits for the next match while the run goes on
	manager.Close()
	manager.mu.Lock()
	history := append([]Record(nil), manager.history...)
	manager.mu.Unlock()
	if len(history) != 1 || history[0].Status != headless.StatusAborted || history[0].Source != SourceSchedule {
		t.Fatalf("history = %+v", history)
	}
	if _, err := manager.Fire("nightly", SourceSchedule, nil); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("fire after close: %v", err)
	}
}

func TestNewNeedsEveryWebhookSecret(t *testing.T) {
	_, err := New(Options{Settings: settings(webhookEntry()), Environment: map[string]string{"CI_SECRET": ""}, Runner: newFakeRunner()})
	if err == nil || !strings.Contains(err.Error(), "CI_SECRET is not set or empty") {
		t.Fatalf("err = %v", err)
	}
}
