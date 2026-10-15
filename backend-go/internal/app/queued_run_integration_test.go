package app

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A second run start on a session whose transcript is held by a running run must not block
// the session's other writers, and must run, with the settings changed meanwhile, once the
// first run ends. This goes through HTTP and the real transcript lock.
func TestQueuedRunStartLetsModelDefaultThroughAndRunsAfterTheFirstRunStops(t *testing.T) {
	f := newLayoutFixture(t)
	f.mu.Lock()
	f.hold, f.reached = true, make(chan struct{}, 4)
	f.mu.Unlock()

	firstDone := make(chan string, 1)
	go func() {
		_, body := f.postRun("first")
		firstDone <- body
	}()
	select {
	case <-f.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the first run never reached the provider")
	}

	secondDone := make(chan string, 1)
	go func() {
		_, body := f.postRun("second")
		secondDone <- body
	}()
	// The queued start has no observable state over HTTP; give it time to reach the lock.
	time.Sleep(300 * time.Millisecond)
	select {
	case <-f.reached:
		t.Fatal("the second run reached the provider while the first still held the transcript")
	default:
	}

	type reply struct {
		status int
		body   string
	}
	modelDefault := make(chan reply, 1)
	go func() {
		status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/model-default?profile="+url.QueryEscape("dev/coder"), `{"model_default":"openai/large"}`)
		modelDefault <- reply{status, body}
	}()
	select {
	case got := <-modelDefault:
		if got.status != http.StatusOK {
			t.Fatalf("model-default while a start is queued: %d %s", got.status, got.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("model-default was blocked behind the queued run start")
	}

	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop", ""); status != http.StatusOK {
		t.Fatalf("stop: %d %s", status, body)
	}
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the first run did not end after the stop")
	}
	select {
	case <-f.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the queued run never reached the provider after the first run ended")
	}

	f.mu.Lock()
	models := append([]string(nil), f.models...)
	f.hold = false
	f.mu.Unlock()
	if len(models) != 2 || models[0] != "model-small" || models[1] != "model-large" {
		t.Fatalf("provider models = %v, want [model-small model-large]: the queued run must use the model chosen while it waited", models)
	}

	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop", ""); status != http.StatusOK {
		t.Fatalf("stop of the queued run: %d %s", status, body)
	}
	select {
	case body := <-secondDone:
		if strings.Contains(body, "already") {
			t.Fatalf("the queued run was refused: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queued run did not end after the stop")
	}
}
