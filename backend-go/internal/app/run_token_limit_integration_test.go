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

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
	"jad/backend-go/internal/tools/native"
)

// tokenCapFixture is one session with a real read tool whose provider asks for another
// read in every round and reports 70 tokens each time, so a run's count is 70 * rounds.
type tokenCapFixture struct {
	t           *testing.T
	stagingDir  string
	api         *httptest.Server
	providerHit atomic.Int32
}

func newTokenCapFixture(t *testing.T, maxRunTokens string, finalAfter int32) *tokenCapFixture {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("sample content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readTool, err := native.NewReadTool(sessionfs.NativeReadSettings{
		Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}}, MaxBytes: 1024, MaxFileBytes: 4096, DefaultLimit: 250,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(struct {
		SchemaVersion int   `json:"schema_version"`
		Tools         []any `json:"tools"`
	}{SchemaVersion: 1, Tools: []any{readTool.Definition}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "read.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := &tokenCapFixture{t: t}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		call := fixture.providerHit.Add(1)
		const usage = `"usage":{"input_tokens":60,"output_tokens":10,"total_tokens":70}`
		var events []string
		if finalAfter > 0 && call > finalAfter {
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"finished"}]}}`,
				fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-final-%d","status":"completed","model":"gpt-test",%s,"output":[]}}`, call, usage),
			}
		} else {
			item := fmt.Sprintf(`{"type":"function_call","call_id":"call-%d","name":"read","arguments":"{\"path\":\"sample.txt\"}"}`, call)
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
				fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-%d","status":"completed","model":"gpt-test",%s,"output":[]}}`, call, usage),
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range append(events, "[DONE]") {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	t.Cleanup(provider.Close)

	settings := map[string]any{"max_tool_rounds": 10}
	if maxRunTokens != "" {
		settings["max_run_tokens"] = json.RawMessage(maxRunTokens)
	}
	config, layout := layoutConfig(t, layouttest.Spec{
		SessionID: "demo", CWD: workspace, ToolDefinitionPaths: []string{filepath.Join(workspace, "read.json")},
		Native:   map[string]any{"read": map[string]any{"max_bytes": 1024, "max_file_bytes": 4096, "default_limit": 250}},
		Settings: settings,
		Models:   map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}},
	}, map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"})
	location, err := sessionfs.Resolve(config.Roots, "local:demo")
	if err != nil {
		t.Fatal(err)
	}
	// A setting the loader refuses (max_run_tokens null) leaves no staging dir to read.
	if sessionSettings, err := sessionfs.LoadSessionSettings(location, "", ""); err == nil {
		fixture.stagingDir = sessionSettings.StagingDir
	}
	_ = layout
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

// run starts one turn and returns the HTTP status, the stream text and the provider
// requests it cost.
func (f *tokenCapFixture) run() (int, string, int32) {
	f.t.Helper()
	f.providerHit.Store(0)
	request, err := http.NewRequest(http.MethodPost, f.api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"read it"}`))
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

func TestMaxRunTokensStopsARunAndTheClientSeesTheCounts(t *testing.T) {
	f := newTokenCapFixture(t, "100", 0)
	status, stream, requests := f.run()
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, stream)
	}
	if requests != 2 {
		t.Errorf("provider requests = %d, want 2 (70 after round 1, 140 after round 2)", requests)
	}
	const want = "The run stopped before its next round: it used 140 tokens, which reached the max_run_tokens limit of 100."
	if !strings.Contains(stream, "event: error") || !strings.Contains(stream, want) {
		t.Errorf("stream does not carry the stop message %q:\n%s", want, stream)
	}
	if strings.Contains(stream, "event: completed") {
		t.Errorf("a stopped run must not complete:\n%s", stream)
	}
	if strings.Contains(stream, "Response details were withheld") {
		t.Errorf("the stop must not be replaced by the generic failure line:\n%s", stream)
	}
	// The refusal is kept beside the transcript, so it survives the run-state window.
	cache, err := os.ReadFile(filepath.Join(f.stagingDir, "run-error-cache.json"))
	if err != nil || !strings.Contains(string(cache), want) {
		t.Errorf("run-error cache missing the stop message: %v\n%s", err, cache)
	}
}

func TestMaxRunTokensAbsentLetsTheSameRunFinish(t *testing.T) {
	f := newTokenCapFixture(t, "", 3)
	status, stream, requests := f.run()
	if status != http.StatusOK || requests != 4 || !strings.Contains(stream, "event: completed") || strings.Contains(stream, "max_run_tokens") {
		t.Fatalf("status = %d, requests = %d, stream:\n%s\nwant 4 requests and a completed run", status, requests, stream)
	}
}

func TestMaxRunTokensNullRefusesTheRunAndNamesTheKey(t *testing.T) {
	f := newTokenCapFixture(t, "null", 0)
	status, body, requests := f.run()
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "max_run_tokens must be a safe integer between 1 and 1000000000000") || requests != 0 {
		t.Fatalf("status = %d, requests = %d, body = %s; want 422 naming max_run_tokens and no provider request", status, requests, body)
	}
}
