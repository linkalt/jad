package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// retryFixture is one session whose provider answers each request from a script: a status
// (with a short Retry-After-Ms so the waits stay small) or a streamed final message.
type retryFixture struct {
	t           *testing.T
	sessionFile string
	// stateDir holds the profile's transcript.
	stateDir    string
	api         *httptest.Server
	providerHit atomic.Int32
	// failures is how many requests fail before the provider answers normally.
	failures atomic.Int32
	status   int
}

func newRetryFixture(t *testing.T, providerRetry string, status int) *retryFixture {
	t.Helper()
	fixture := &retryFixture{t: t, status: status}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fixture.providerHit.Add(1)
		if fixture.failures.Add(-1) >= 0 {
			w.Header().Set("Retry-After-Ms", "1")
			http.Error(w, "scripted failure", fixture.status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered"}]}}`,
			`{"type":"response.completed","response":{"id":"resp-1","status":"completed","model":"gpt-test","output":[]}}`,
			"[DONE]",
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	t.Cleanup(provider.Close)

	config, layout := layoutConfig(t, layouttest.Spec{
		Settings:    map[string]any{"max_tool_rounds": 2},
		RawSettings: retryRawSettings(providerRetry),
		Models:      map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}},
	}, map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"})
	fixture.sessionFile = layout.SessionFile
	fixture.stateDir = profileState(layout)
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	fixture.api = httptest.NewServer(service.Handler)
	t.Cleanup(func() {
		service.Close()
		fixture.api.Close()
	})
	return fixture
}

func (f *retryFixture) writeSetting(providerRetry string) {
	f.t.Helper()
	path := f.sessionFile
	var session map[string]any
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(data, &session); err != nil {
		f.t.Fatal(err)
	}
	settings := map[string]any{"max_tool_rounds": 2}
	if providerRetry != "" {
		settings["provider_retry"] = json.RawMessage(providerRetry)
	}
	session["settings"] = settings
	out, _ := json.Marshal(session)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// run starts one turn and returns the run stream's text and the provider requests it cost.
func (f *retryFixture) run(failures int32) (string, int32) {
	f.t.Helper()
	_, stream, requests := f.runWithStatus(failures)
	return stream, requests
}

// runWithStatus is run that also returns the HTTP status of the run request.
func (f *retryFixture) runWithStatus(failures int32) (int, string, int32) {
	f.t.Helper()
	f.providerHit.Store(0)
	f.failures.Store(failures)
	request, err := http.NewRequest(http.MethodPost, f.api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"go"}`))
	if err != nil {
		f.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body), f.providerHit.Load()
}

func TestProviderRetryRecoversFromRateLimitsThroughARealRun(t *testing.T) {
	f := newRetryFixture(t, `{"max_retries":3,"initial_delay_ms":1,"max_delay_ms":20}`, http.StatusTooManyRequests)
	stream, requests := f.run(2)
	if !strings.Contains(stream, "event: completed") || strings.Contains(stream, "event: error") {
		t.Fatalf("the run should have completed after two retries:\n%s", stream)
	}
	if requests != 3 {
		t.Fatalf("provider requests = %d, want 3 (the SDK's own retries must be off)", requests)
	}
	transcript, err := os.ReadFile(filepath.Join(f.stateDir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(string(transcript)), "\n") + 1; got != 2 {
		t.Fatalf("transcript has %d records, want 2 (user message and one response): failed attempts must leave nothing\n%s", got, transcript)
	}
}

func TestProviderRetryUsedUpReportsTheStatusAfterMaxRetriesPlusOneRequests(t *testing.T) {
	f := newRetryFixture(t, `{"max_retries":3,"initial_delay_ms":1,"max_delay_ms":20}`, http.StatusServiceUnavailable)
	stream, requests := f.run(100)
	if !strings.Contains(stream, "The provider request failed (HTTP 503)") {
		t.Fatalf("run stream lacks the provider status:\n%s", stream)
	}
	if requests != 4 {
		t.Fatalf("provider requests = %d, want 4", requests)
	}
}

func TestProviderRetryAbsentKeepsTheTwoRetriesTheSDKAlwaysMade(t *testing.T) {
	f := newRetryFixture(t, "", http.StatusTooManyRequests)
	_, requests := f.run(100)
	if requests != 3 {
		t.Fatalf("provider requests = %d, want 3 (1 + the default 2 retries)", requests)
	}
}

// The runner is cached per session by what it was built from. A changed provider_retry
// must reach the next run, not wait for a restart.
func TestProviderRetryChangedBetweenRunsTakesEffectOnTheNextRun(t *testing.T) {
	f := newRetryFixture(t, `{"max_retries":0}`, http.StatusTooManyRequests)
	if _, requests := f.run(100); requests != 1 {
		t.Fatalf("with max_retries 0: provider requests = %d, want 1", requests)
	}
	f.writeSetting(`{"max_retries":4,"initial_delay_ms":1,"max_delay_ms":20}`)
	if _, requests := f.run(100); requests != 5 {
		t.Fatalf("after raising max_retries to 4: provider requests = %d, want 5 (a cached runner would still make 1)", requests)
	}
}

func TestProviderRetryNullRefusesTheRunAndNamesTheKey(t *testing.T) {
	f := newRetryFixture(t, `{"max_retries":null}`, http.StatusTooManyRequests)
	status, body, requests := f.runWithStatus(0)
	if requests != 0 {
		t.Fatalf("a refused setting must not reach the provider, got %d requests", requests)
	}
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "provider_retry.max_retries must be a safe integer between 0 and 10") {
		t.Fatalf("status = %d, body = %s; want 422 naming provider_retry.max_retries", status, body)
	}
}

func TestProviderRetryWaitsAreAnnouncedOnTheRunStreamAndClearedBeforeTheResponse(t *testing.T) {
	f := newRetryFixture(t, `{"max_retries":3,"initial_delay_ms":1,"max_delay_ms":20}`, http.StatusTooManyRequests)
	stream, requests := f.run(2)
	if requests != 3 {
		t.Fatalf("provider requests = %d, want 3", requests)
	}
	waits := strings.Count(stream, "event: provider_retry")
	if waits != 3 {
		t.Fatalf("provider_retry events = %d, want two waits and one clear:\n%s", waits, stream)
	}
	for _, want := range []string{`"attempt":1`, `"attempt":2`, `"max_retries":3`, `"status":429`, `"waiting":null`, `"resume_at":"`} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream lacks %s:\n%s", want, stream)
		}
	}
	if last, completed := strings.LastIndex(stream, "event: provider_retry"), strings.Index(stream, "event: completed"); last > completed {
		t.Fatalf("the notice was cleared after the run completed:\n%s", stream)
	}
	transcript, err := os.ReadFile(filepath.Join(f.stateDir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transcript), "provider_retry") || strings.Contains(string(transcript), "resume_at") {
		t.Fatalf("the retry notice reached the transcript:\n%s", transcript)
	}
}

func TestProviderRetryUsedUpClearsTheNoticeBeforeTheError(t *testing.T) {
	f := newRetryFixture(t, `{"max_retries":2,"initial_delay_ms":1,"max_delay_ms":20}`, http.StatusServiceUnavailable)
	stream, _ := f.run(100)
	if got := strings.Count(stream, "event: provider_retry"); got != 3 {
		t.Fatalf("provider_retry events = %d, want two waits and one clear:\n%s", got, stream)
	}
	if last, failed := strings.LastIndex(stream, "event: provider_retry"), strings.Index(stream, "event: error"); failed < 0 || last > failed {
		t.Fatalf("the notice must be cleared before the error:\n%s", stream)
	}
}

func TestARequestThatWorksFirstTimeSendsNoProviderRetryEvent(t *testing.T) {
	f := newRetryFixture(t, `{"max_retries":3,"initial_delay_ms":1,"max_delay_ms":20}`, http.StatusTooManyRequests)
	stream, _ := f.run(0)
	if strings.Contains(stream, "provider_retry") {
		t.Fatalf("unexpected provider_retry event:\n%s", stream)
	}
}

// retryRawSettings is the session setting provider_retry, given as JSON text (null included).
func retryRawSettings(providerRetry string) map[string]json.RawMessage {
	if providerRetry == "" {
		return nil
	}
	return map[string]json.RawMessage{"provider_retry": json.RawMessage(providerRetry)}
}
