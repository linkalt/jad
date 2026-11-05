package app

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// recordingProvider is a Responses endpoint that answers every request with one text
// message and keeps each request body.
type recordingProvider struct {
	mu     sync.Mutex
	bodies []map[string]json.RawMessage
}

func (p *recordingProvider) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.bodies = append(p.bodies, body)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"provider answer"}]}}`,
			`{"type":"response.completed","response":{"id":"resp-hook","status":"completed","model":"gpt-test","usage":{"input_tokens":100},"output":[]}}`,
			"[DONE]",
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	})
}

func (p *recordingProvider) requests() []map[string]json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]json.RawMessage(nil), p.bodies...)
}

// startHookRunService is startPreCompactService with extra model file keys, against a
// recording provider.
func startHookRunService(t *testing.T, settings, modelExtra map[string]any, provider *recordingProvider, logs *lockedBuffer) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewServer(provider.handler())
	t.Cleanup(server.Close)
	model := map[string]any{"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": server.URL + "/v1"}
	for key, value := range modelExtra {
		model[key] = value
	}
	spec := layouttest.Spec{
		SessionID: "one",
		Settings:  settings,
		Server:    map[string]any{"max_body_bytes": 4096, "sse_max_buffered_bytes": 4096},
		Models:    map[string]map[string]any{"openai/default": model},
	}
	config, fixture := layoutConfig(t, spec, map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"})
	config.HTTP.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	messagesPath := filepath.Join(profileState(fixture), "messages.jsonl")
	if err := os.MkdirAll(filepath.Dir(messagesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(messagesPath, []byte(preCompactTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(service.Handler)
	t.Cleanup(func() {
		service.Close()
		api.Close()
	})
	return api, messagesPath
}

func postHookRun(t *testing.T, apiURL, input string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"input": input})
	request, err := http.NewRequest(http.MethodPost, apiURL+"/api/sessions/local:one/run", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("run status=%d body=%s", response.StatusCode, body)
	}
	return string(body)
}

func readTranscript(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

// A context command's output is stored right after the input as a developer message, so
// the model reads it in this run and the later ones; the command reads the prompt.
func TestPromptSubmitContextIsStoredAfterTheInputAndReachesTheModel(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events.jsonl")
	context := runHookEntry("ctx", "cat >> "+events+"; echo >> "+events+"; echo CONTEXT-MARKER")
	context["context"] = true
	silent := runHookEntry("quiet", "true")
	silent["context"] = true
	provider := &recordingProvider{}
	logs := &lockedBuffer{}
	api, messagesPath := startHookRunService(t, map[string]any{"hooks": map[string]any{"enabled": true, "prompt_submit": []any{context, silent}}}, nil, provider, logs)

	if body := postHookRun(t, api.URL, "hello there"); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	lines := readHookLines(t, events)
	if len(lines) < 1 {
		t.Fatal("prompt_submit did not run")
	}
	for _, want := range []string{`"event":"prompt_submit"`, `"prompt":"hello there"`, `"session":"one"`, `"turn_id":"`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("payload lacks %s: %s", want, lines[0])
		}
	}
	transcript := readTranscript(t, messagesPath)
	user := strings.Index(transcript, "hello there")
	stored := strings.Index(transcript, `"role":"developer"`)
	if user < 0 || stored < user || !strings.Contains(transcript[stored:], "[prompt_submit hook ctx]\\nCONTEXT-MARKER") {
		t.Fatalf("the context is not stored after the input:\n%s", transcript)
	}
	if strings.Contains(transcript, "prompt_submit hook quiet") {
		t.Errorf("a context command that printed nothing added a section:\n%s", transcript)
	}
	requests := provider.requests()
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(requests))
	}
	input := string(requests[0]["input"])
	if at := strings.Index(input, "CONTEXT-MARKER"); at < 0 || at < strings.Index(input, "hello there") {
		t.Fatalf("the model did not get the context after the prompt: %s", input)
	}
}

// A blocking command that fails refuses the run before anything is stored or sent, and the
// commands after it do not start; a failing command that is not blocking changes nothing.
func TestBlockingPromptSubmitRefusesTheRunAndStoresNothing(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events.jsonl")
	failing := runHookEntry("advisory", "exit 4")
	gate := runHookEntry("gate", "echo GATE-REPORT; exit 3")
	gate["blocking"] = true
	after := runHookEntry("after", "cat >> "+events)
	provider := &recordingProvider{}
	logs := &lockedBuffer{}
	api, messagesPath := startHookRunService(t, map[string]any{"hooks": map[string]any{"enabled": true, "prompt_submit": []any{failing, gate, after}}}, nil, provider, logs)

	body := postHookRun(t, api.URL, "do the forbidden thing")
	if !strings.Contains(body, "The prompt_submit hook gate blocked the prompt; nothing was stored.") {
		t.Fatalf("the run did not fail with the hook's name:\n%s", body)
	}
	// The report reaches the client as a hook_report event, like every run hook's; the
	// error message itself names only the hook.
	if errorEvent := body[strings.Index(body, "event: error"):]; strings.Contains(errorEvent, "GATE-REPORT") {
		t.Errorf("the error message carries the hook's output:\n%s", errorEvent)
	}
	if transcript := readTranscript(t, messagesPath); transcript != preCompactTranscript {
		t.Fatalf("a blocked prompt changed messages.jsonl:\n%s", transcript)
	}
	if got := len(provider.requests()); got != 0 {
		t.Errorf("provider requests = %d, want 0", got)
	}
	if _, err := os.Stat(events); err == nil {
		t.Error("a command after the blocking one ran")
	}
	if !strings.Contains(logs.String(), "GATE-REPORT") {
		t.Errorf("the server log lacks the blocking report:\n%s", logs.String())
	}
}

// A blocking pre_compact that fails skips a manual compaction with 409 and leaves the
// transcript as it was.
func TestBlockingPreCompactSkipsAManualCompaction(t *testing.T) {
	gate := runHookEntry("keep", "exit 1")
	gate["blocking"] = true
	logs := &lockedBuffer{}
	_, api, messagesPath := startPreCompactService(t, map[string]any{
		"compaction": map[string]any{"keepbatch": map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}}},
		"hooks":      map[string]any{"enabled": true, "pre_compact": []any{gate}},
	}, nil, logs)

	response := postEmptyMutation(t, api.URL+"/api/sessions/local:one/compact", integrationAPIToken)
	body := readMutationResponse(t, response)
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "pre_compact hook keep blocked the compaction") {
		t.Fatalf("compact status=%d body=%s", response.StatusCode, body)
	}
	if transcript := readTranscript(t, messagesPath); transcript != preCompactTranscript {
		t.Fatalf("a blocked compaction changed messages.jsonl:\n%s", transcript)
	}
}

// A blocking pre_compact that fails skips an automatic compaction: the run completes and
// the old turn stays. Without the block the same run compacts it away.
func TestBlockingPreCompactSkipsAnAutomaticCompaction(t *testing.T) {
	for _, blocking := range []bool{true, false} {
		t.Run(fmt.Sprintf("blocking=%v", blocking), func(t *testing.T) {
			gate := runHookEntry("keep", "exit 1")
			gate["blocking"] = blocking
			provider := &recordingProvider{}
			api, messagesPath := startHookRunService(t, map[string]any{
				"max_tool_rounds": 1,
				"compaction":      map[string]any{"auto_enabled": true, "trigger_tokens": 1, "keepbatch": map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}}},
				"hooks":           map[string]any{"enabled": true, "pre_compact": []any{gate}},
			}, nil, provider, &lockedBuffer{})
			if body := postHookRun(t, api.URL, "continue"); !strings.Contains(body, "event: completed") {
				t.Fatalf("run did not complete:\n%s", body)
			}
			kept := strings.Contains(readTranscript(t, messagesPath), "old context")
			if kept != blocking {
				t.Fatalf("old turn kept = %v, want %v", kept, blocking)
			}
		})
	}
}

// prompt_cache_key session sends one key for every request of the session, a digest that
// holds no path; off (the default) sends none.
func TestPromptCacheKeyIsTheSameForEveryRunOfTheSession(t *testing.T) {
	provider := &recordingProvider{}
	api, messagesPath := startHookRunService(t, map[string]any{}, map[string]any{"prompt_cache_key": "session"}, provider, &lockedBuffer{})
	postHookRun(t, api.URL, "first")
	postHookRun(t, api.URL, "second")
	requests := provider.requests()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(requests))
	}
	var first, second string
	_ = json.Unmarshal(requests[0]["prompt_cache_key"], &first)
	_ = json.Unmarshal(requests[1]["prompt_cache_key"], &second)
	if !strings.HasPrefix(first, "jad-") || len(first) != len("jad-")+32 || first != second {
		t.Fatalf("keys = %q and %q, want one jad- digest", first, second)
	}
	if strings.Contains(first, filepath.Base(filepath.Dir(messagesPath))) {
		t.Errorf("the key %q holds a path element", first)
	}

	off := &recordingProvider{}
	api, _ = startHookRunService(t, map[string]any{}, nil, off, &lockedBuffer{})
	postHookRun(t, api.URL, "first")
	if _, exists := off.requests()[0]["prompt_cache_key"]; exists {
		t.Fatal("a model without prompt_cache_key sent one")
	}
}

// subagent_stop reports each finished child to the parent session's commands, with its
// agent, answer and counts; the children of an agent whose model sets prompt_cache_key share
// a key the parent does not send.
func TestSubagentStopHookReportsTheChildRun(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events.jsonl")
	f := newSubagentFixture(t, map[string]any{
		"model":         "child.json",
		"__child_model": map[string]any{"prompt_cache_key": "session"},
		"__session":     map[string]any{"hooks": map[string]any{"enabled": true, "subagent_stop": []any{runHookEntry("done", "cat >> "+events+"; echo >> "+events)}}},
	})
	body := f.run(t)
	if !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	// The command's report reaches the parent run's stream.
	reported := false
	for _, frame := range strings.Split(body, "\n\n") {
		if strings.Contains(frame, "event: hook_report") && strings.Contains(frame, `"event":"subagent_stop"`) && strings.Contains(frame, `[done]`) {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("no subagent_stop hook_report in the parent stream:\n%s", body)
	}
	lines := readHookLines(t, events)
	var stops []string
	for _, line := range lines {
		if line != "" {
			stops = append(stops, line)
		}
	}
	if len(stops) != 1 {
		t.Fatalf("subagent_stop fired %d times, want 1: %q", len(stops), stops)
	}
	for _, want := range []string{`"event":"subagent_stop"`, `"agent":"explore"`, `"outcome":"ok"`, `"answer":"the answer is 42"`, `"rounds":2`, `"tokens":32`, `"session":"demo"`, `"call_id":"`} {
		if !strings.Contains(stops[0], want) {
			t.Errorf("payload lacks %s: %s", want, stops[0])
		}
	}
	children := f.provider.forModel("child-model")
	if len(children) != 2 || children[0].PromptCacheKey == "" || children[0].PromptCacheKey != children[1].PromptCacheKey {
		t.Fatalf("child keys = %+v, want one shared key", children)
	}
	for _, parent := range f.provider.forModel("parent-model") {
		if parent.PromptCacheKey != "" {
			t.Fatalf("the parent, whose model leaves it off, sent prompt_cache_key %q", parent.PromptCacheKey)
		}
	}
}
