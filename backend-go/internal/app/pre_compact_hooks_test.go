package app

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

const preCompactTranscript = `{"type":"message","role":"user","content":[{"type":"input_text","text":"old context"}]}
{"type":"response.completed","response":{"id":"resp_old","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]}]}}
{"type":"message","role":"user","content":[{"type":"input_text","text":"latest context"}]}
{"type":"response.completed","response":{"id":"resp_latest","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"latest answer"}]}]}}
`

func preCompactHooks(events string) map[string]any {
	return map[string]any{"enabled": true, "pre_compact": []any{runHookEntry("before", "cat >> "+events+"; echo >> "+events+"; echo PRECOMPACT-MARKER")}}
}

func startPreCompactService(t *testing.T, settings map[string]any, provider *httptest.Server, logs *lockedBuffer) (*Service, *httptest.Server, string) {
	t.Helper()
	spec := layouttest.Spec{
		SessionID: "one",
		Settings:  settings,
		Server:    map[string]any{"max_body_bytes": 4096, "sse_max_buffered_bytes": 4096},
		Models:    map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY"}},
	}
	if provider != nil {
		spec.Models["openai/default"]["base_url"] = provider.URL + "/v1"
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
	return service, api, messagesPath
}

func TestPreCompactHookFiresBeforeAManualCompaction(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events.jsonl")
	logs := &lockedBuffer{}
	_, api, messagesPath := startPreCompactService(t, map[string]any{
		"compaction": map[string]any{"keepbatch": map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}}},
		"hooks":      preCompactHooks(events),
	}, nil, logs)

	response := postEmptyMutation(t, api.URL+"/api/sessions/local:one/compact", integrationAPIToken)
	body := readMutationResponse(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `"compacted":true`) {
		t.Fatalf("compact status=%d body=%s", response.StatusCode, body)
	}
	var lines []string
	for _, line := range readHookLines(t, events) {
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("pre_compact fired %d times, want 1: %q", len(lines), lines)
	}
	for _, want := range []string{`"event":"pre_compact"`, `"trigger":"manual"`, `"session":"one"`, `"profile":"`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("payload lacks %s: %s", want, lines[0])
		}
	}
	if strings.Contains(lines[0], `"turn_id"`) {
		t.Errorf("a manual compaction belongs to no turn: %s", lines[0])
	}
	if output := logs.String(); !strings.Contains(output, "hook pre_compact") || !strings.Contains(output, "PRECOMPACT-MARKER") {
		t.Errorf("the server log lacks the pre_compact report:\n%s", output)
	}
	contents, err := os.ReadFile(messagesPath)
	if err != nil || strings.Contains(string(contents), "PRECOMPACT") {
		t.Fatalf("hook output reached messages.jsonl: %q, %v", contents, err)
	}
}

func TestPreCompactHookStartsNothingWhenDisabledOrWhenNothingIsConfigured(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events.jsonl")
	logs := &lockedBuffer{}
	hooks := preCompactHooks(events)
	hooks["enabled"] = false
	_, api, _ := startPreCompactService(t, map[string]any{
		"compaction": map[string]any{"keepbatch": map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}}},
		"hooks":      hooks,
	}, nil, logs)
	response := postEmptyMutation(t, api.URL+"/api/sessions/local:one/compact", integrationAPIToken)
	if body := readMutationResponse(t, response); response.StatusCode != http.StatusOK || !strings.Contains(body, `"compacted":true`) {
		t.Fatalf("compact status=%d body=%s", response.StatusCode, body)
	}
	if _, err := os.Stat(events); err == nil {
		t.Fatal("pre_compact ran although hooks.enabled is false")
	}
	if strings.Contains(logs.String(), "hook pre_compact") {
		t.Errorf("the log mentions pre_compact:\n%s", logs.String())
	}
}

func TestPreCompactHookFiresBeforeAnAutomaticCompactionInsideARun(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events.jsonl")
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after automatic compaction"}]}}`,
			`{"type":"response.completed","response":{"id":"resp-auto","status":"completed","model":"gpt-test","usage":{"input_tokens":100},"output":[]}}`,
			"[DONE]",
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	defer provider.Close()
	logs := &lockedBuffer{}
	_, api, _ := startPreCompactService(t, map[string]any{
		"max_tool_rounds": 1,
		"compaction":      map[string]any{"auto_enabled": true, "trigger_tokens": 1, "keepbatch": map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}}},
		"hooks":           preCompactHooks(events),
	}, provider, logs)

	request, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:one/run", strings.NewReader(`{"input":"continue"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "after automatic compaction") {
		t.Fatalf("run status=%d body=%s", response.StatusCode, body)
	}
	lines := readHookLines(t, events)
	if len(lines) < 1 || !strings.Contains(lines[0], `"event":"pre_compact"`) || !strings.Contains(lines[0], `"trigger":"automatic"`) || !strings.Contains(lines[0], `"turn_id":"`) || strings.Contains(lines[0], `"turn_id":""`) {
		t.Fatalf("payload = %q; want an automatic pre_compact event carrying the run's turn id", lines)
	}
	if got := providerCalls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want 1", got)
	}
}
