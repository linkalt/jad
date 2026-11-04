package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// heldResetFixture is a session with a run whose provider answers only once release is
// closed, so a reset sent meanwhile finds the run active. It drives the real service.
type heldResetFixture struct {
	t       *testing.T
	api     *httptest.Server
	service *Service
	release chan struct{}
	// reached is closed by the provider once the run has started its request.
	reached chan struct{}
	// fixture is the session layout the service was loaded from.
	fixture layouttest.Fixture
}

func newHeldResetFixture(t *testing.T) *heldResetFixture {
	t.Helper()
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reached <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
			`{"type":"response.completed","response":{"id":"resp-1","status":"completed","model":"gpt-test","output":[]}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	spec := maintenanceSpec(map[string]any{}, "")
	spec.Models = map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}}
	config, fixture := layoutConfig(t, spec, map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"})
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	api := httptest.NewServer(service.Handler)
	t.Cleanup(func() {
		service.Close()
		api.Close()
	})
	return &heldResetFixture{t: t, api: api, service: service, release: release, reached: reached, fixture: fixture}
}

// startHeldRun starts a run and returns once the provider has been reached, so the run is
// active with its transcript held. The response body is read to the end by the caller.
func (f *heldResetFixture) startHeldRun() io.ReadCloser {
	f.t.Helper()
	request, _ := http.NewRequest(http.MethodPost, f.api.URL+"/api/sessions/local:one/run?profile=dev/coder", strings.NewReader(`{"input":"go"}`))
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { response.Body.Close() })
	return response.Body
}

func (f *heldResetFixture) reset() (int, string) {
	f.t.Helper()
	request, _ := http.NewRequest(http.MethodPost, f.api.URL+"/api/sessions/local:one/reset?profile=dev/coder", nil)
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

// awaitRunActive waits until the provider has been reached, which only happens once the run
// is registered and holds the transcript, so the reset is sent while the run is active.
func (f *heldResetFixture) awaitRunActive() {
	f.t.Helper()
	select {
	case <-f.reached:
	case <-time.After(10 * time.Second):
		f.t.Fatal("the run never reached the provider")
	}
}

// The reset is refused with 409 while the profile's run is active, and the transcript is left
// as the run wrote it.
func TestHTTPResetIsRefusedWhileARunIsActive(t *testing.T) {
	f := newHeldResetFixture(t)
	run := f.startHeldRun()
	f.awaitRunActive()
	status, body := f.reset()
	if status != http.StatusConflict || !strings.Contains(body, "cannot reset a profile while a run is active") {
		t.Fatalf("reset during a run: status = %d body = %s, want 409 with the active-run text", status, body)
	}
	close(f.release)
	if _, err := io.Copy(io.Discard, run); err != nil {
		t.Fatal(err)
	}
	var answer map[string]any
	if status, body := f.reset(); status != http.StatusOK || json.Unmarshal([]byte(body), &answer) != nil || answer["reset"] != true {
		t.Fatalf("reset after the run ended: status = %d body = %s", status, body)
	}
}

// A reset sent while the profile's run is active is refused as the run holds the transcript:
// the held run is left running and the reset does not wait for it.
func TestResetIsRefusedAtOnceAndDoesNotWaitForTheRun(t *testing.T) {
	f := newHeldResetFixture(t)
	run := f.startHeldRun()
	f.awaitRunActive()
	finished := make(chan struct{})
	var status int
	var body string
	go func() {
		status, body = f.reset()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the reset waited for the run instead of being refused")
	}
	if status != http.StatusConflict || !strings.Contains(body, "cannot reset a profile while a run is active") {
		t.Fatalf("reset status = %d body = %s", status, body)
	}
	close(f.release)
	if _, err := io.Copy(io.Discard, run); err != nil {
		t.Fatal(err)
	}
}

// The active-run check is made on the resolved reference and profile, so a reset spelled
// differently from the run is still refused while the run is going, and not only when both
// spell the session alike.
func TestResetRejectsAnActiveRunForResolvedProfile(t *testing.T) {
	f := newHeldResetFixture(t)
	run := f.startHeldRun()
	f.awaitRunActive()
	err := f.service.resolver.ResetProfile(context.Background(), "one", "dev/coder")
	if err != ErrRunActive {
		t.Fatalf("reset by the bare id during a run = %v, want ErrRunActive", err)
	}
	close(f.release)
	if _, err := io.Copy(io.Discard, run); err != nil {
		t.Fatal(err)
	}
	if err := f.service.resolver.ResetProfile(context.Background(), "local:one", "dev/coder"); err != nil {
		t.Fatalf("reset after the run ended: %v", err)
	}
}

// A resolver whose maintenance has no session answers the specific 500 the HTTP routes answer,
// not a generic error, so the message is not lost.
func TestResetWithoutAMaintenanceSessionSaysSo(t *testing.T) {
	var statusErr *httpapi.StatusError
	err := maintenanceReady(httpapi.ResolvedMutation{Reference: "session-1", Profile: "dev/coder"})
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusInternalServerError || statusErr.Message != "session maintenance provider is not initialized" {
		t.Fatalf("nil session = %v, want the 500 with the provider message", err)
	}
}
