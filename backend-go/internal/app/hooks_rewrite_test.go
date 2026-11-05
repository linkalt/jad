package app

import (
	"context"
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
	"time"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
	"jad/backend-go/internal/tools"
)

// answerRecorder answers every request with one final assistant message and keeps the
// request bodies.
func answerRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		count := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		item := fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer %d"}]}`, count)
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
			`{"type":"response.completed","response":{"id":"resp-h","status":"completed","model":"gpt-test","output":[` + item + `]}}`,
			"[DONE]",
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	t.Cleanup(provider.Close)
	return provider, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// startEmptyService starts a service over a session with no transcript yet.
func startEmptyService(t *testing.T, settings map[string]any, provider *httptest.Server) (*httptest.Server, string) {
	t.Helper()
	cwd, _ := settings["cwd"].(string)
	spec := layouttest.Spec{
		SessionID: "one",
		CWD:       cwd,
		Settings:  settings,
		Server:    map[string]any{"max_body_bytes": 4096, "sse_max_buffered_bytes": 4096},
		Models:    map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}},
	}
	config, fixture := layoutConfig(t, spec, map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"})
	config.HTTP.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(service.Handler)
	t.Cleanup(func() {
		service.Close()
		api.Close()
	})
	return api, filepath.Join(profileState(fixture), "messages.jsonl")
}

func runAndRead(t *testing.T, api *httptest.Server, body string) string {
	t.Helper()
	response := postRunBody(t, api.URL+"/api/sessions/local:one/run", body)
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || strings.Contains(string(data), "event: error") {
		t.Fatalf("run status=%d body=%s", response.StatusCode, data)
	}
	return string(data)
}

func TestPromptRewriteSessionStartAndStopHooksShapeTheRun(t *testing.T) {
	provider, requests := answerRecorder(t)
	marker := filepath.Join(t.TempDir(), "stopped-once")
	api, messagesPath := startEmptyService(t, map[string]any{"hooks": map[string]any{
		"enabled":       true,
		"session_start": []any{map[string]any{"name": "intro", "argv": []any{"sh", "-c", "echo PROJECT-RULES"}, "context": true}},
		"prompt_submit": []any{map[string]any{"name": "polish", "argv": []any{"sh", "-c", `echo '{"prompt":"POLISHED PROMPT"}'`}, "rewrite": true}},
		// The first stop asks for another round; the marker lets the second one end the run.
		"stop": []any{map[string]any{"name": "verify", "argv": []any{"sh", "-c", `if [ -e ` + marker + ` ]; then exit 0; fi; touch ` + marker + `; echo '{"continue":"CHECK YOUR WORK"}'`}}},
	}}, provider)

	runAndRead(t, api, `{"input":"raw prompt"}`)
	sent := requests()
	if len(sent) != 2 {
		t.Fatalf("provider requests = %d, want the answer and the round the stop hook asked for", len(sent))
	}
	if strings.Contains(sent[0], "raw prompt") || !strings.Contains(sent[0], "POLISHED PROMPT") || !strings.Contains(sent[0], "PROJECT-RULES") {
		t.Fatalf("first request = %s", sent[0])
	}
	if !strings.Contains(sent[1], "[stop hook verify]") || !strings.Contains(sent[1], "CHECK YOUR WORK") {
		t.Fatalf("second request = %s", sent[1])
	}

	// A second run is not the start of the conversation: session_start stays quiet.
	runAndRead(t, api, `{"input":"again"}`)
	transcript, err := os.ReadFile(messagesPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(transcript), "PROJECT-RULES") != 1 || strings.Contains(string(transcript), "raw prompt") {
		t.Fatalf("transcript = %s", transcript)
	}
}

func TestNotificationHooksFireWhenAToolAsksTheUser(t *testing.T) {
	root := t.TempDir()
	payload := filepath.Join(root, "notified.json")
	config := sessionfs.SessionConfig{WorkingDir: root, Profile: "dev", Hooks: sessionfs.HooksSettings{
		Enabled: true, TimeoutMS: 5000, MaxOutputBytes: 2048, MaxPayloadBytes: 4096, KillGraceMs: 100, KillDeadlineMs: 1000, DrainAfterExitMs: 100, KillProcessGroup: "always", MaxLineChars: 2000,
		Notification: []sessionfs.RunEventHook{{Name: "ping", Argv: []string{"sh", "-c", "cat > " + payload + ".tmp && mv " + payload + ".tmp " + payload}}},
	}}
	runner := boundRunner{resolver: &Resolver{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, config: config, reference: "local:one"}
	asked := false
	options := session.RunOptions{TurnID: "turn-1"}
	options.ToolExecution.RequestInteraction = func(context.Context, tools.InteractionIdentity, tools.InteractionQuestion) (tools.InteractionAnswer, error) {
		asked = true
		return tools.InteractionAnswer{}, nil
	}
	runner.applyNotificationHooks(context.Background(), &options)
	if _, err := options.ToolExecution.RequestInteraction(context.Background(), tools.InteractionIdentity{Tool: "request_draft", ToolCallID: "call-9"}, tools.InteractionQuestion{Title: "Approve the plan"}); err != nil || !asked {
		t.Fatalf("the question did not reach the user: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(payload)
		if err == nil {
			for _, want := range []string{`"event":"notification"`, `"trigger":"interaction"`, `"tool":"request_draft"`, `"call_id":"call-9"`, `"title":"Approve the plan"`, `"turn_id":"turn-1"`} {
				if !strings.Contains(string(data), want) {
					t.Errorf("payload misses %s: %s", want, data)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the notification command did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProjectInstructionsReachTheModelPrompt(t *testing.T) {
	provider, requests := answerRecorder(t)
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("ALWAYS RUN THE LINTER"), 0o600); err != nil {
		t.Fatal(err)
	}
	// startEmptyService passes cwd to the fixture as the session's own working directory.
	api, messagesPath := startEmptyService(t, map[string]any{"cwd": cwd, "project_instructions": map[string]any{"enabled": true}}, provider)
	runAndRead(t, api, `{"input":"hello"}`)
	sent := requests()
	if len(sent) != 1 || !strings.Contains(sent[0], "ALWAYS RUN THE LINTER") || !strings.Contains(sent[0], "# Project instructions: ") {
		t.Fatalf("request = %v", sent)
	}
	transcript, err := os.ReadFile(messagesPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transcript), "ALWAYS RUN THE LINTER") {
		t.Fatalf("the prompt text reached messages.jsonl: %s", transcript)
	}
}
