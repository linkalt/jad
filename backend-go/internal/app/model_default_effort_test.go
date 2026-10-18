package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func newDefaultEffortFixture(t *testing.T) (*layoutFixture, string) {
	t.Helper()
	config, fixture := layoutConfig(t, layouttest.Spec{
		Models: map[string]map[string]any{
			"openai/gpt":   {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_KEY", "efforts": []string{"low", "high"}},
			"openai/plain": {"provider": "openai", "model": "plain-test", "api_key_env": "FIXTURE_KEY"},
		},
		Profiles: map[string]map[string]any{"dev/coder": {"models": []string{"openai/gpt", "openai/plain"}, "model_default": "openai/plain"}},
	}, map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_KEY": "k"})
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	api := httptest.NewServer(service.Handler)
	t.Cleanup(func() { service.Close(); api.Close() })
	return &layoutFixture{api: api}, filepath.Join(fixture.Root, "profiles", "dev", "coder.json")
}

func TestHTTPModelDefaultTakesAnEffort(t *testing.T) {
	f, profileFile := newDefaultEffortFixture(t)
	path := "/api/sessions/local:demo/model-default?profile=dev/coder"

	status, body := f.do(t, http.MethodPost, path, `{"model_default":"openai/gpt@high"}`)
	if status != http.StatusOK || !strings.Contains(body, `"model_default":"openai/gpt@high"`) {
		t.Fatalf("model-default = %d %s", status, body)
	}
	if !strings.Contains(body, `"models":["openai/gpt","openai/plain"]`) {
		t.Fatalf("models must stay plain: %s", body)
	}
	stored, err := os.ReadFile(profileFile)
	if err != nil || !strings.Contains(string(stored), `"model_default": "openai/gpt@high"`) {
		t.Fatalf("profile file: err=%v\n%s", err, stored)
	}

	for _, model := range []string{"openai/gpt@max", "openai/plain@high", "openai/gpt@"} {
		if status, body := f.do(t, http.MethodPost, path, `{"model_default":"`+model+`"}`); status != http.StatusBadRequest {
			t.Errorf("%s: status %d %s, want 400", model, status, body)
		}
	}
	if after, _ := os.ReadFile(profileFile); string(after) != string(stored) {
		t.Fatalf("a refused update changed the file:\n%s", after)
	}
}

func TestCatalogProfileAcceptsAnEffortInModelDefault(t *testing.T) {
	f, profileFile := newDefaultEffortFixture(t)
	path := "/api/catalog/profiles/dev/coder"
	before := f.catalogCall(t, "GET", path, "", nil)
	headers := map[string]string{"If-Match": before.etag}

	bad := f.catalogCall(t, "PUT", path, `{"models":["openai/gpt","openai/plain"],"model_default":"openai/gpt@max"}`, headers)
	if bad.status != http.StatusUnprocessableEntity || !strings.Contains(bad.body, "model_default") {
		t.Fatalf("bad effort = %d %s, want 422 naming model_default", bad.status, bad.body)
	}
	if onDisk, _ := os.ReadFile(profileFile); string(onDisk) != before.body {
		t.Fatalf("a refused write changed the file: %q", onDisk)
	}

	next := `{"models":["openai/gpt","openai/plain"],"model_default":"openai/gpt@high"}`
	reply := f.catalogCall(t, "PUT", path, next, headers)
	if reply.status != http.StatusOK {
		t.Fatalf("PUT = %d %s", reply.status, reply.body)
	}
	if onDisk, _ := os.ReadFile(profileFile); string(onDisk) != next {
		t.Fatalf("stored %q", onDisk)
	}
}
