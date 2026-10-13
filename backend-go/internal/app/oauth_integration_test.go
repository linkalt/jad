package app

import (
	"encoding/base64"
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
	"time"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// oauthFixture is one session in auth_mode openai_oauth whose "Codex endpoint" is a local
// server that records each request.
type oauthFixture struct {
	t          *testing.T
	sessionDir string
	// stateDir holds the profile's transcript, staging and HTTP debug log.
	stateDir string
	authFile string
	api      *httptest.Server
	codex    *httptest.Server
	// logs is the server log: a failed run's detail goes there, not to the client.
	logs *lockedBuffer

	mu       sync.Mutex
	requests []oauthRequest
}

type oauthRequest struct {
	path    string
	header  http.Header
	body    string
	requery string
}

func (f *oauthFixture) recorded() []oauthRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]oauthRequest(nil), f.requests...)
}

func oauthToken(expires time.Time) string {
	payload, _ := json.Marshal(map[string]any{"exp": expires.Unix()})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func (f *oauthFixture) writeAuth(accessToken, accountID string) {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{"access_token": accessToken, "account_id": accountID, "refresh_token": "refresh-secret"}})
	if err := os.WriteFile(f.authFile, body, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func newOAuthFixture(t *testing.T, modelExtra map[string]any) *oauthFixture {
	t.Helper()
	root := t.TempDir()
	f := &oauthFixture{t: t, authFile: filepath.Join(root, "codex-auth.json"), logs: &lockedBuffer{}}
	f.codex = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, oauthRequest{path: r.URL.Path, header: r.Header.Clone(), body: string(body)})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"codex says hi"}]}}`,
			`{"type":"response.completed","response":{"id":"resp-1","status":"completed","model":"gpt-test","output":[]}}`,
			"[DONE]",
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	t.Cleanup(f.codex.Close)

	modelFile := map[string]any{"provider": "openai", "model": "gpt-test", "auth_mode": "openai_oauth", "base_url": f.codex.URL + "/codex", "oauth_auth_file": f.authFile}
	for key, value := range modelExtra {
		modelFile[key] = value
	}
	// No provider key variable exists: OAuth mode must not ask for one.
	config, layout := layoutConfig(t, layouttest.Spec{
		CWD:      root,
		Settings: map[string]any{"max_tool_rounds": 2, "http_debug": map[string]any{"enabled": true, "include_request_headers": true, "include_request_body": true, "max_body_bytes": 65536}},
		Models:   map[string]map[string]any{"openai/default": modelFile},
	}, map[string]string{"JAD_API_TOKEN": integrationAPIToken})
	f.sessionDir = layout.SessionStateDir
	f.stateDir = profileState(layout)
	config.HTTP.Logger = slog.New(slog.NewTextHandler(f.logs, nil))
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	f.api = httptest.NewServer(service.Handler)
	t.Cleanup(func() {
		service.Close()
		f.api.Close()
	})
	return f
}

func (f *oauthFixture) run() (int, string) {
	f.t.Helper()
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
	return response.StatusCode, string(body)
}

func TestOAuthRunSendsTheCredentialToTheConfiguredEndpointAndNothingElseChanges(t *testing.T) {
	f := newOAuthFixture(t, nil)
	access := oauthToken(time.Now().Add(time.Hour))
	f.writeAuth(access, "acct-42")

	status, stream := f.run()
	if status != http.StatusOK || !strings.Contains(stream, "event: completed") || strings.Contains(stream, "event: error") {
		t.Fatalf("run status %d:\n%s", status, stream)
	}
	if !strings.Contains(stream, "codex says hi") {
		t.Errorf("the model text did not reach the stream:\n%s", stream)
	}
	requests := f.recorded()
	if len(requests) != 1 {
		t.Fatalf("codex requests = %d, want 1", len(requests))
	}
	got := requests[0]
	if got.path != "/codex/responses" {
		t.Errorf("path = %q, want /codex/responses", got.path)
	}
	if got.header.Get("Authorization") != "Bearer "+access || got.header.Get("Chatgpt-Account-Id") != "acct-42" {
		t.Errorf("credential headers missing or wrong (authorization ok: %v, account %q)", got.header.Get("Authorization") == "Bearer "+access, got.header.Get("Chatgpt-Account-Id"))
	}
	if got.header.Get("Openai-Beta") != "" {
		t.Errorf("OpenAI-Beta = %q, want none", got.header.Get("Openai-Beta"))
	}
	// The body is the one an API-key session sends: no store, include, instructions
	// default or max_output_tokens rewriting.
	var body map[string]any
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"store", "include", "max_output_tokens"} {
		if _, present := body[key]; present {
			t.Errorf("request body has %q; OAuth mode must not rewrite the body", key)
		}
	}
	if body["model"] != "gpt-test" || body["stream"] != true {
		t.Errorf("model/stream = %v/%v", body["model"], body["stream"])
	}
}

func TestOAuthRunNeverWritesTheTokenOrAccountIDToTheHTTPDebugLog(t *testing.T) {
	f := newOAuthFixture(t, nil)
	access := oauthToken(time.Now().Add(time.Hour))
	f.writeAuth(access, "acct-secret-id")
	if status, stream := f.run(); status != http.StatusOK || !strings.Contains(stream, "event: completed") {
		t.Fatalf("run status %d:\n%s", status, stream)
	}
	// The debug log must exist, or the check below proves nothing.
	if _, err := os.Stat(filepath.Join(f.stateDir, "debug", "http", "request1")); err != nil {
		t.Fatalf("the HTTP debug log was not written: %v", err)
	}
	err := filepath.WalkDir(f.stateDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range []string{access, "acct-secret-id", "refresh-secret"} {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s contains a credential value", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOAuthRunWithAnExpiredTokenIsRefusedBeforeAnyRequestAndNamesTheFile(t *testing.T) {
	f := newOAuthFixture(t, nil)
	f.writeAuth(oauthToken(time.Now().Add(-time.Hour)), "acct")
	_, stream := f.run()
	if !strings.Contains(stream, "event: error") || strings.Contains(stream, "event: completed") {
		t.Fatalf("the run should have failed:\n%s", stream)
	}
	// The client sees the generic failure; the file and the reason are in the server log.
	if logs := f.logs.String(); !strings.Contains(logs, f.authFile) || !strings.Contains(logs, "expired") {
		t.Errorf("the log should name the credential file and say expired:\n%s", logs)
	}
	if n := len(f.recorded()); n != 0 {
		t.Errorf("%d requests reached the endpoint", n)
	}
}

func TestOAuthRunWithAMissingCredentialFileIsRefused(t *testing.T) {
	f := newOAuthFixture(t, nil)
	_, stream := f.run()
	if !strings.Contains(stream, "event: error") || !strings.Contains(f.logs.String(), "file not found") {
		t.Fatalf("stream:\n%s\nlog:\n%s", stream, f.logs.String())
	}
	if n := len(f.recorded()); n != 0 {
		t.Errorf("%d requests reached the endpoint", n)
	}
}

func TestOAuthRunUsesATokenThatWasRefreshedBetweenRunsWithoutARestart(t *testing.T) {
	f := newOAuthFixture(t, nil)
	first := oauthToken(time.Now().Add(time.Hour))
	f.writeAuth(first, "acct")
	if status, stream := f.run(); status != http.StatusOK || !strings.Contains(stream, "event: completed") {
		t.Fatalf("first run %d:\n%s", status, stream)
	}
	second := oauthToken(time.Now().Add(2 * time.Hour))
	f.writeAuth(second, "acct")
	if status, stream := f.run(); status != http.StatusOK || !strings.Contains(stream, "event: completed") {
		t.Fatalf("second run %d:\n%s", status, stream)
	}
	requests := f.recorded()
	if len(requests) != 2 {
		t.Fatalf("codex requests = %d, want 2", len(requests))
	}
	if requests[0].header.Get("Authorization") != "Bearer "+first || requests[1].header.Get("Authorization") != "Bearer "+second {
		t.Error("the second run did not use the refreshed token")
	}
}

func TestOAuthRunFailsAtOnceWhenTheTokenExpiresBehindACachedRunner(t *testing.T) {
	f := newOAuthFixture(t, nil)
	f.writeAuth(oauthToken(time.Now().Add(time.Hour)), "acct")
	if status, stream := f.run(); status != http.StatusOK || !strings.Contains(stream, "event: completed") {
		t.Fatalf("first run %d:\n%s", status, stream)
	}
	// The runner is cached now. Expire the token behind it: the next run must fail at once
	// and never send the dead token.
	f.writeAuth(oauthToken(time.Now().Add(-time.Minute)), "acct")
	started := time.Now()
	_, stream := f.run()
	if !strings.Contains(stream, "event: error") {
		t.Fatalf("the run should have failed:\n%s", stream)
	}
	if n := len(f.recorded()); n != 1 {
		t.Errorf("codex requests = %d, want only the first run's", n)
	}
	if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
		t.Errorf("the failed run took %v; a credential error must not wait on retries", elapsed)
	}
}

func TestAPIKeySessionStillRequiresItsEnvironmentVariableAndIgnoresOAuthSettings(t *testing.T) {
	config, _ := layoutConfig(t, layouttest.Spec{
		Settings: map[string]any{"openai_oauth": map[string]any{"expiry_margin_ms": 1}},
		Models:   map[string]map[string]any{"openai/default": {"model": "gpt-test", "api_key_env": "FIXTURE_MISSING_KEY"}},
	}, map[string]string{"JAD_API_TOKEN": integrationAPIToken})
	logs := &lockedBuffer{}
	config.HTTP.Logger = slog.New(slog.NewTextHandler(logs, nil))
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(service.Handler)
	defer func() { service.Close(); api.Close() }()
	request, _ := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"go"}`))
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "event: error") || !strings.Contains(logs.String(), "environment variable FIXTURE_MISSING_KEY is not set") {
		t.Fatalf("stream:\n%s\nlog:\n%s", body, logs.String())
	}
}
