package app

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// A model file that still asks for the removed ChatGPT or Claude subscription sign-in must
// stop the run with the unsupported message, before any request: it is never read as an API
// key session, so no request leaves with some other credential.
func TestRemovedOAuthModelKeysStopTheRunBeforeAnyProviderRequest(t *testing.T) {
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
		t.Error("the provider was called")
	}))
	t.Cleanup(provider.Close)
	cases := map[string]struct {
		model map[string]any
		want  string
	}{
		"openai auth_mode": {map[string]any{"model": "m", "auth_mode": "openai_oauth", "base_url": provider.URL}, "auth_mode must be api_key (openai_oauth was removed)"},
		"openai file":      {map[string]any{"model": "m", "oauth_auth_file": "~/.jad/auth/openai-codex.json", "base_url": provider.URL}, "oauth_auth_file was removed"},
		"anthropic file": {map[string]any{"model": "m", "api": "anthropic_messages", "api_key_env": "FIXTURE_KEY", "base_url": provider.URL,
			"anthropic": map[string]any{"upstream": map[string]any{"credential_file": "~/.jad/auth/claude-oauth.json"}}}, "upstream.credential_file was removed"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			logs := &lockedBuffer{}
			f := newLayoutFixtureWithBudget(t, nil, nil, func(c *Config) { c.HTTP.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
			data, _ := json.Marshal(test.model)
			if err := os.WriteFile(filepath.Join(f.root, "models", "openai", "small.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", `{"input":"hi"}`)
			// The client gets the generic failure line (a run's detail stays in the server
			// log, as for every configuration error); the log says what to change.
			if status != http.StatusOK || !strings.Contains(body, "event: error") {
				t.Fatalf("status %d, body %s, want a failed run", status, body)
			}
			logged := logs.String()
			if !strings.Contains(logged, test.want) || !strings.Contains(logged, "no longer supported") || !strings.Contains(logged, "API key") {
				t.Fatalf("the log does not say the key is unsupported and to use an API key (want %q):\n%s", test.want, logged)
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("provider requests = %d, want none", got)
			}
		})
	}
}
