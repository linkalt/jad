package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// layoutFixture is a session in the catalog layout (setting.json with catalogs, a
// session.json, profile/model/toolset files) behind a fake Responses endpoint that
// records which model each request asked for.
type layoutFixture struct {
	root, cwd string
	api       *httptest.Server
	mu        sync.Mutex
	models    []string
}

func newLayoutFixture(t *testing.T) *layoutFixture {
	t.Helper()
	f := &layoutFixture{root: t.TempDir(), cwd: t.TempDir()}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var request struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &request)
		f.mu.Lock()
		f.models = append(f.models, request.Model)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","model":"` + request.Model + `","output":[]}}`,
			"[DONE]",
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	t.Cleanup(provider.Close)

	write := func(rel string, value any) {
		path := filepath.Join(f.root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	model := func(name string) map[string]any {
		return map[string]any{"provider": "openai", "model": name, "api_key_env": "FIXTURE_KEY", "base_url": provider.URL + "/v1"}
	}
	write("setting.json", map[string]any{
		"schema_version": 1,
		"session_roots":  []any{map[string]any{"id": "local", "path": "sessions"}},
		"catalogs":       map[string]any{"profiles": "profiles", "models": "models", "tools": "tools", "mcp": "mcp", "state": filepath.Join(f.cwd, ".jad", "state")},
	})
	write("models/openai/small.json", model("model-small"))
	write("models/openai/large.json", model("model-large"))
	write("tools/coding/none.json", map[string]any{"native": map[string]any{}, "mcp": map[string]any{}})
	write("profiles/dev/coder.json", map[string]any{"models": []string{"openai/small", "openai/large"}, "model_default": "openai/small", "toolset": "coding/none"})
	write("profiles/review/reviewer.json", map[string]any{"models": []string{"openai/large"}, "toolset": "coding/none"})
	write("sessions/demo/session.json", map[string]any{
		"schema_version": 1, "cwd": f.cwd, "active_profile": "dev/coder", "category": "work",
		"scope": map[string]any{"profiles": []string{"dev", "review"}},
	})

	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir: f.root, RootSettingPath: filepath.Join(f.root, "setting.json"),
		Environment: map[string]string{"AGENT_API_TOKEN": integrationAPIToken, "FIXTURE_KEY": "k"},
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	f.api = httptest.NewServer(service.Handler)
	t.Cleanup(func() { service.Close(); f.api.Close() })
	return f
}

func (f *layoutFixture) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, f.api.URL+path, strings.NewReader(body))
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
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

func (f *layoutFixture) lastModel() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.models[len(f.models)-1]
}

func TestLayoutSessionRunsSwitchesProfileAndKeepsStatePerProfile(t *testing.T) {
	f := newLayoutFixture(t)
	const session = "/api/sessions/local:demo"

	if status, body := f.do(t, http.MethodPost, session+"/run", `{"input":"one"}`); status != 200 || !strings.Contains(body, "event: completed") {
		t.Fatalf("run on the active profile: %d %s", status, body)
	}
	if got := f.lastModel(); got != "model-small" {
		t.Fatalf("model = %q, want the profile's default", got)
	}
	coderMessages := filepath.Join(f.cwd, ".jad", "state", "demo", "dev", "coder", "messages.jsonl")
	if data, err := os.ReadFile(coderMessages); err != nil || !strings.Contains(string(data), "one") {
		t.Fatalf("transcript of dev/coder: %v %s", err, data)
	}

	// A model chosen for this session, by name; the profile file is not edited.
	status, body := f.do(t, http.MethodPost, session+"/model-default?profile="+url.QueryEscape("dev/coder"), `{"model_default":"openai/large"}`)
	if status != 200 {
		t.Fatalf("model-default: %d %s", status, body)
	}
	if status, body := f.do(t, http.MethodPost, session+"/run?profile="+url.QueryEscape("dev/coder"), `{"input":"two"}`); status != 200 {
		t.Fatalf("run after model-default: %d %s", status, body)
	}
	if got := f.lastModel(); got != "model-large" {
		t.Fatalf("model = %q, want the session's choice", got)
	}

	// The run body names a model too.
	if status, body := f.do(t, http.MethodPost, session+"/run", `{"input":"three","model":"openai/small"}`); status != 200 {
		t.Fatalf("run with a model: %d %s", status, body)
	}
	if got := f.lastModel(); got != "model-small" {
		t.Fatalf("model = %q, want the requested one", got)
	}
	if status, _ := f.do(t, http.MethodPost, session+"/run?profile="+url.QueryEscape("review/reviewer"), `{"input":"x","model":"openai/small"}`); status != 400 {
		t.Fatalf("a model outside the profile: status %d, want 400", status)
	}

	// Switching the profile writes session.json and the other profile gets its own state.
	if status, body := f.do(t, http.MethodPost, session+"/profile", `{"active_profile":"review/reviewer"}`); status != 200 || !strings.Contains(body, "review/reviewer") {
		t.Fatalf("profile switch: %d %s", status, body)
	}
	if status, body := f.do(t, http.MethodPost, session+"/run", `{"input":"four"}`); status != 200 {
		t.Fatalf("run on the new profile: %d %s", status, body)
	}
	if got := f.lastModel(); got != "model-large" {
		t.Fatalf("reviewer model = %q", got)
	}
	reviewerMessages := filepath.Join(f.cwd, ".jad", "state", "demo", "review", "reviewer", "messages.jsonl")
	if data, err := os.ReadFile(reviewerMessages); err != nil || !strings.Contains(string(data), "four") || strings.Contains(string(data), "one") {
		t.Fatalf("transcript of review/reviewer: %v %s", err, data)
	}
	if status, _ := f.do(t, http.MethodPost, session+"/profile", `{"active_profile":"coder"}`); status != 400 {
		t.Fatalf("a bare profile name: status %d, want 400", status)
	}
	if status, body := f.do(t, http.MethodGet, "/api/sessions", ""); status != 200 || !strings.Contains(body, `"demo"`) {
		t.Fatalf("session list: %d %s", status, body)
	}
}

// The session list carries the category of session.json, so a client can group
// sessions without reading the folder layout.
func TestLayoutSessionListCarriesTheSessionCategory(t *testing.T) {
	f := newLayoutFixture(t)
	status, body := f.do(t, http.MethodGet, "/api/sessions", "")
	if status != 200 {
		t.Fatalf("session list: %d %s", status, body)
	}
	var list struct {
		Sessions []struct {
			ID       string `json:"id"`
			Category string `json:"category"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].Category != "work" {
		t.Fatalf("sessions = %+v, want one session in category work", list.Sessions)
	}
}

// A category edit must change the list ETag, or a client holding the old list
// would get 304 and keep showing the session under the old group.
func TestSessionVersionChangesWithTheCategory(t *testing.T) {
	base := sessionfs.SessionConfig{Profile: "a/b", DefaultProfile: "a/b", Category: "work"}
	moved := base
	moved.Category = "test"
	if sessionVersion(base) == sessionVersion(moved) {
		t.Fatal("sessionVersion ignores the category")
	}
}
