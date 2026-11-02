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

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// layoutFixture is a session in the catalog layout (setting.json with catalogs, a
// session.json, profile/model/toolset files) behind a fake Responses endpoint that
// records which model each request asked for.
type layoutFixture struct {
	root, cwd string
	api       *httptest.Server
	service   *Service
	mu        sync.Mutex
	models    []string
	// captureBodies records each request body in bodies, for a test that reads the prompt.
	captureBodies bool
	bodies        []string
	// hold, when set, makes the provider announce each request on reached and then wait until
	// the client gives up, so a test can have a run in flight.
	hold    bool
	reached chan struct{}
	// cancelled, when set, receives one value each time a held request's context ends.
	cancelled chan struct{}
	// script, when set, returns the JSON output item a request is answered with; "" keeps the plain "ok" message.
	script func(model, body string) string
	// usage, when set, is the JSON usage object every completed response carries.
	usage string
	// server is merged into the root setting's server object before the config is loaded.
	server map[string]any
}

func newLayoutFixture(t *testing.T) *layoutFixture {
	t.Helper()
	return newLayoutFixtureWithServer(t, nil)
}

// newLayoutFixtureWithServer is newLayoutFixture with extra keys in the root setting's server object.
func newLayoutFixtureWithServer(t *testing.T, server map[string]any) *layoutFixture {
	t.Helper()
	return newLayoutFixtureWithBudget(t, server, nil)
}

// newLayoutFixtureWithBudget is newLayoutFixtureWithServer with the budget of leaf tool calls
// the service's runs share. Nil keeps the process budget; a test passes a budget of one.
func newLayoutFixtureWithBudget(t *testing.T, server map[string]any, budget *tools.LeafBudget) *layoutFixture {
	t.Helper()
	f := &layoutFixture{root: t.TempDir(), cwd: t.TempDir(), server: server}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var request struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &request)
		f.mu.Lock()
		f.models = append(f.models, request.Model)
		if f.captureBodies {
			f.bodies = append(f.bodies, string(raw))
		}
		hold, reached, cancelled, script, usage := f.hold, f.reached, f.cancelled, f.script, f.usage
		f.mu.Unlock()
		if hold {
			reached <- struct{}{}
			<-r.Context().Done()
			if cancelled != nil {
				cancelled <- struct{}{}
			}
			return
		}
		item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`
		if script != nil {
			if scripted := script(request.Model, string(raw)); scripted != "" {
				item = scripted
			}
		}
		items := []json.RawMessage{json.RawMessage(item)}
		if strings.HasPrefix(item, "[") {
			items = nil
			_ = json.Unmarshal([]byte(item), &items)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for index, one := range items {
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", index, one)
		}
		for _, event := range []string{
			`{"type":"response.completed","response":{"id":"r` + fmt.Sprint(len(f.models)) + `","status":"completed","model":"` + request.Model + `",` + usage + `"output":[]}}`,
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
	rootSetting := map[string]any{
		"schema_version": 1,
		"session_roots":  []any{map[string]any{"id": "local", "path": "sessions"}},
		"catalogs":       map[string]any{"profiles": "profiles", "models": "models", "tools": "tools", "mcp": "mcp", "state": filepath.Join(f.cwd, ".jad", "state")},
	}
	toolDefinitions := make([]string, 0, 2)
	for _, name := range []string{"write", "profile_message", "async", "read"} {
		definition, err := filepath.Abs(filepath.Join("..", "..", "..", "sample", "tools", "native", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		toolDefinitions = append(toolDefinitions, definition)
	}
	rootSetting["libraries"] = map[string]any{"tool_definition_paths": toolDefinitions}
	if len(server) > 0 {
		rootSetting["server"] = server
	}
	write("setting.json", rootSetting)
	write("models/openai/small.json", model("model-small"))
	write("models/openai/large.json", model("model-large"))
	write("tools/coding/none.json", map[string]any{"native": map[string]any{}, "mcp": map[string]any{}})
	write("profiles/dev/coder.json", map[string]any{"models": []string{"openai/small", "openai/large"}, "model_default": "openai/small", "toolset": "coding/none"})
	write("profiles/review/reviewer.json", map[string]any{"models": []string{"openai/large"}, "toolset": "coding/none"})
	write("sessions/demo.json", map[string]any{
		"schema_version": 1, "cwd": f.cwd, "category": "work",
		"scope": map[string]any{"profiles": []string{"dev", "review"}},
	})

	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir: f.root, RootSettingPath: filepath.Join(f.root, "setting.json"),
		Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_KEY": "k"},
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	service, err := newServiceForLayout(config, budget)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	f.service = service
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

func TestLayoutSessionRunsAndKeepsStatePerProfile(t *testing.T) {
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

	// The other profile gets its own state.
	if status, body := f.do(t, http.MethodPost, session+"/run?profile="+url.QueryEscape("review/reviewer"), `{"input":"four"}`); status != 200 {
		t.Fatalf("run on the other profile: %d %s", status, body)
	}
	if got := f.lastModel(); got != "model-large" {
		t.Fatalf("reviewer model = %q", got)
	}
	reviewerMessages := filepath.Join(f.cwd, ".jad", "state", "demo", "review", "reviewer", "messages.jsonl")
	if data, err := os.ReadFile(reviewerMessages); err != nil || !strings.Contains(string(data), "four") || strings.Contains(string(data), "one") {
		t.Fatalf("transcript of review/reviewer: %v %s", err, data)
	}
	if status, body := f.do(t, http.MethodGet, "/api/sessions", ""); status != 200 || !strings.Contains(body, `"demo"`) {
		t.Fatalf("session list: %d %s", status, body)
	}
}

// The routes that chose a profile in the session file and read the working directory's
// README are gone: a call to either is not answered with success.
func TestLayoutRemovedProfileAndReadmeRoutesAreGone(t *testing.T) {
	f := newLayoutFixture(t)
	for name, call := range map[string]struct{ method, path, body string }{
		"profile route": {http.MethodPost, "/api/sessions/local:demo/profile", `{"active_profile":"review/reviewer"}`},
		"readme route":  {http.MethodGet, "/api/sessions/local:demo/readme", ""},
	} {
		if status, body := f.do(t, call.method, call.path, call.body); status < 400 || status >= 500 {
			t.Errorf("%s: status %d body %s, want a 4xx", name, status, body)
		}
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

// The model route writes the profile's own model_default, so a second session of the same
// profile sees the choice too; session.json is not touched.
func TestLayoutModelRouteWritesTheProfileFileForEverySession(t *testing.T) {
	f := newLayoutFixture(t)
	sessionFile := filepath.Join(f.root, "sessions", "demo.json")
	profileFile := filepath.Join(f.root, "profiles", "dev", "coder.json")
	sessionBefore, _ := os.ReadFile(sessionFile)
	other := filepath.Join(f.root, "sessions", "other.json")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, sessionBefore, 0o600); err != nil {
		t.Fatal(err)
	}

	status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/model-default?profile="+url.QueryEscape("dev/coder"), `{"model_default":"openai/large"}`)
	if status != 200 || !strings.Contains(body, `"model_default":"openai/large"`) {
		t.Fatalf("model-default: %d %s", status, body)
	}
	if after, _ := os.ReadFile(sessionFile); string(after) != string(sessionBefore) {
		t.Fatalf("session.json changed:\n%s", after)
	}
	if data, _ := os.ReadFile(profileFile); !strings.Contains(string(data), `"model_default": "openai/large"`) {
		t.Fatalf("profile file does not carry the choice:\n%s", data)
	}
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:other/run", `{"input":"x"}`); status != 200 {
		t.Fatalf("run in the other session: %d %s", status, body)
	}
	if got := f.lastModel(); got != "model-large" {
		t.Fatalf("the other session ran %q, want model-large: the profile is shared", got)
	}
	if status, _ := f.do(t, http.MethodPost, "/api/sessions/local:demo/model-default?profile="+url.QueryEscape("dev/coder"), `{"model_default":"openai/missing"}`); status != 400 {
		t.Fatalf("a model the profile does not offer: status %d, want 400", status)
	}
}

// A session.json that still has model_defaults answers 4xx on every route, naming the key.
func TestLayoutStaleModelDefaultsAreRefusedOverHTTP(t *testing.T) {
	f := newLayoutFixture(t)
	sessionFile := filepath.Join(f.root, "sessions", "demo.json")
	data, _ := os.ReadFile(sessionFile)
	data = []byte(strings.Replace(string(data), `"schema_version"`, `"model_defaults": {"dev/coder": "openai/large"}, "schema_version"`, 1))
	if err := os.WriteFile(sessionFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]struct{ method, path, body string }{
		"run":         {http.MethodPost, "/api/sessions/local:demo/run", `{"input":"x"}`},
		"model route": {http.MethodPost, "/api/sessions/local:demo/model-default", `{"model_default":"openai/large"}`},
	} {
		status, body := f.do(t, call.method, call.path, call.body)
		if status < 400 || status >= 500 || !strings.Contains(body, "model_defaults was removed") {
			t.Errorf("%s: status %d, body %s", name, status, body)
		}
	}
	if len(f.models) != 0 {
		t.Errorf("the provider was reached %d times with a refused session", len(f.models))
	}
}

// Profile instructions are sent to the provider as the prompt, in the profile's role.
func TestLayoutProfileInstructionsReachTheProvider(t *testing.T) {
	f := newLayoutFixture(t)
	profileFile := filepath.Join(f.root, "profiles", "dev", "coder.json")
	data, _ := os.ReadFile(profileFile)
	var profile map[string]any
	_ = json.Unmarshal(data, &profile)
	profile["instructions"] = "You are the coder."
	profile["prompt_role"] = "developer"
	data, _ = json.Marshal(profile)
	if err := os.WriteFile(profileFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f.captureBodies = true
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", `{"input":"hello"}`); status != 200 {
		t.Fatalf("run: %d %s", status, body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) != 1 || !strings.Contains(f.bodies[0], "You are the coder.") || !strings.Contains(f.bodies[0], `"role":"developer"`) {
		t.Fatalf("provider request bodies = %v", f.bodies)
	}
}

// A profile's name reaches the session list, and renaming it changes the list ETag, or a
// client holding the old list would get 304 and keep the old label.
func TestSessionListCarriesProfileNames(t *testing.T) {
	f := newLayoutFixture(t)
	file := filepath.Join(f.root, "profiles", "dev", "coder.json")
	editProfileName := func(name any) {
		var profile map[string]any
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &profile); err != nil {
			t.Fatal(err)
		}
		profile["name"] = name
		data, _ = json.Marshal(profile)
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	names := func() (map[string]string, string) {
		status, body := f.do(t, http.MethodGet, "/api/sessions", "")
		if status != 200 {
			t.Fatalf("session list: %d %s", status, body)
		}
		var list struct {
			Sessions []struct {
				ProfileNames map[string]string `json:"profile_names"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(body), &list); err != nil || len(list.Sessions) != 1 {
			t.Fatalf("decode %s: %v", body, err)
		}
		return list.Sessions[0].ProfileNames, body
	}
	if got, body := names(); len(got) != 0 || strings.Contains(body, "profile_names") {
		t.Fatalf("unnamed profiles: %v in %s", got, body)
	}
	editProfileName("Coder")
	if got, _ := names(); len(got) != 1 || got["dev/coder"] != "Coder" {
		t.Fatalf("profile_names = %v", got)
	}
}

func TestSessionVersionChangesWithAProfileName(t *testing.T) {
	base := sessionfs.SessionConfig{Profile: "a/b", DefaultProfile: "a/b", Profiles: []string{"a/b", "a/c"}}
	named := base
	named.ProfileNames = map[string]string{"a/b": "B"}
	renamed := base
	renamed.ProfileNames = map[string]string{"a/b": "B2"}
	moved := base
	moved.ProfileNames = map[string]string{"a/c": "B"}
	versions := map[string]string{
		"none": sessionVersion(base), "named": sessionVersion(named),
		"renamed": sessionVersion(renamed), "moved": sessionVersion(moved),
	}
	seen := map[string]string{}
	for label, version := range versions {
		if other, dup := seen[version]; dup {
			t.Errorf("sessionVersion is the same for %s and %s", label, other)
		}
		seen[version] = label
	}
}

// newServiceForLayout is NewService, or newServiceWithBudget when a test passes a budget.
func newServiceForLayout(config Config, budget *tools.LeafBudget) (*Service, error) {
	if budget == nil {
		return NewService(config)
	}
	return newServiceWithBudget(config, budget)
}
