package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/sessionfs"
)

// headlessFor builds a headless service on the files of a layout fixture. The environment
// has no JAD_API_TOKEN: a headless service makes its own.
func headlessFor(t *testing.T, f *layoutFixture) *headless.Runner {
	t.Helper()
	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir: f.cwd, GlobalDir: f.global,
		Environment: map[string]string{"FIXTURE_KEY": "k"},
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	service, runner, err := NewHeadless(config)
	if err != nil {
		t.Fatalf("NewHeadless: %v", err)
	}
	t.Cleanup(service.Close)
	return runner
}

func TestHeadlessRunCompletesAndWritesTheSessionTranscript(t *testing.T) {
	f := newLayoutFixture(t)
	f.usage = `"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18},`
	runner := headlessFor(t, f)
	var names []string
	result, err := runner.Run(context.Background(), headless.Request{
		Session: "local:demo", Profile: "dev/coder", Input: "hello headless",
		OnEvent: func(event headless.Event) { names = append(names, event.Name) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != headless.StatusCompleted || result.Text != "ok" || result.Error != "" {
		t.Fatalf("result = %+v, want completed with the answer", result)
	}
	if result.TurnID == "" || result.Session != "local:demo" || result.Profile != "dev/coder" {
		t.Fatalf("result identity = %+v", result)
	}
	var usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal(result.Usage, &usage); err != nil || usage.InputTokens != 11 || usage.OutputTokens != 7 {
		t.Fatalf("usage = %s (%v), want the provider's counts", result.Usage, err)
	}
	if len(names) < 3 || names[0] != "run" || names[len(names)-2] != "completed" || names[len(names)-1] != "done" {
		t.Fatalf("events = %v, want run first and completed, done last", names)
	}
	transcript := filepath.Join(f.cwd, ".jad", "state", "demo", "dev", "coder", "messages.jsonl")
	if data, err := os.ReadFile(transcript); err != nil || !strings.Contains(string(data), "hello headless") {
		t.Fatalf("transcript: %v %s", err, data)
	}
	if got := f.lastModel(); got != "model-small" {
		t.Fatalf("model = %q, want the profile default", got)
	}
}

func TestHeadlessRunUsesTheRequestedModel(t *testing.T) {
	f := newLayoutFixture(t)
	runner := headlessFor(t, f)
	result, err := runner.Run(context.Background(), headless.Request{Session: "local:demo", Model: "openai/large", Input: "x"})
	if err != nil || result.Status != headless.StatusCompleted {
		t.Fatalf("Run: %+v %v", result, err)
	}
	if got := f.lastModel(); got != "model-large" {
		t.Fatalf("model = %q, want the requested one", got)
	}
}

func TestHeadlessRunRefusalsAreRequestErrors(t *testing.T) {
	f := newLayoutFixture(t)
	runner := headlessFor(t, f)
	for name, request := range map[string]headless.Request{
		"unknown session":         {Session: "local:nope", Input: "x"},
		"model outside a profile": {Session: "local:demo", Profile: "review/reviewer", Model: "openai/small", Input: "x"},
	} {
		_, err := runner.Run(context.Background(), request)
		var refused *headless.RequestError
		if !errors.As(err, &refused) || refused.Status < 400 || refused.Message == "" {
			t.Errorf("%s: error = %v, want a request error with the server's message", name, err)
		}
	}
}

func TestHeadlessRunTimeoutStopsTheRun(t *testing.T) {
	f := newLayoutFixture(t)
	f.hold, f.reached, f.cancelled = true, make(chan struct{}, 1), make(chan struct{}, 1)
	runner := headlessFor(t, f)
	result, err := runner.Run(context.Background(), headless.Request{Session: "local:demo", Input: "slow", Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != headless.StatusTimedOut || result.Error == "" {
		t.Fatalf("result = %+v, want timed_out with the stop message", result)
	}
	select {
	case <-f.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the provider request was not cancelled")
	}
}

func TestHeadlessRunCancelledContextAbortsTheRun(t *testing.T) {
	f := newLayoutFixture(t)
	f.hold, f.reached = true, make(chan struct{}, 1)
	runner := headlessFor(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-f.reached
		cancel()
	}()
	result, err := runner.Run(ctx, headless.Request{Session: "local:demo", Input: "slow"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != headless.StatusAborted {
		t.Fatalf("result = %+v, want aborted", result)
	}
	// The session is free again: a second run starts and completes.
	f.mu.Lock()
	f.hold = false
	f.mu.Unlock()
	if again, err := runner.Run(context.Background(), headless.Request{Session: "local:demo", Input: "again"}); err != nil || again.Status != headless.StatusCompleted {
		t.Fatalf("second run: %+v %v", again, err)
	}
}
