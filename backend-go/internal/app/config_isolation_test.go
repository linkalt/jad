package app

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs/layouttest"
)

type isolationListed struct {
	ID       string   `json:"id"`
	Profiles []string `json:"profiles"`
	Models   []string `json:"models"`
	Active   string   `json:"active_profile"`
}

func listedFor(t *testing.T, body []byte) isolationListed {
	t.Helper()
	var listed struct {
		Sessions []isolationListed `json:"sessions"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode the list: %v\n%s", err, body)
	}
	if len(listed.Sessions) != 1 {
		t.Fatalf("listed %d sessions, want 1: %s", len(listed.Sessions), body)
	}
	return listed.Sessions[0]
}

// A broken profile and a broken model of a session do not stop it: it is listed with what is
// fine, runs with the model the profile chose, and the files are named in the log. Asking for
// the broken profile by name is refused, and the files stay in the catalog listing so they can
// be fixed.
func TestBrokenProfileAndModelFilesDoNotFailTheSession(t *testing.T) {
	logs := &lockedBuffer{}
	f := newLayoutFixtureWithBudget(t, nil, nil, func(c *Config) { c.HTTP.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"colour":"red"}`)
	f.overwrite(t, "models/openai/large.json", "{")

	status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", `{"input":"hi"}`)
	if status != http.StatusOK || strings.Contains(body, "event: error") {
		t.Fatalf("run: %d %s", status, body)
	}
	if got := f.lastModel(); got != "model-small" {
		t.Fatalf("the run used %q, want the profile's model_default model-small", got)
	}

	status, body = f.do(t, http.MethodGet, "/api/sessions", "")
	if status != http.StatusOK {
		t.Fatalf("list: %d %s", status, body)
	}
	listed := listedFor(t, []byte(body))
	if listed.ID != "demo" || strings.Join(listed.Profiles, ",") != "dev/coder" || strings.Join(listed.Models, ",") != "openai/small" {
		t.Fatalf("listed = %+v, want demo with dev/coder and openai/small only", listed)
	}
	for _, want := range []string{"profile review/reviewer left out", "reviewer.json", "colour", "model openai/large left out", "large.json"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, logs.String())
		}
	}

	// The files can still be listed, opened and fixed.
	for kind, id := range map[string]string{"models": "openai/large", "profiles": "review/reviewer"} {
		reply := f.catalogCall(t, "GET", "/api/catalog/"+kind, "", nil)
		if reply.status != http.StatusOK || !strings.Contains(reply.body, `"id":"`+id+`"`) {
			t.Errorf("catalog %s = %d %s, want %s listed", kind, reply.status, reply.body, id)
		}
	}

	// A broken profile named in the request is an error: no other profile runs in its place.
	before := len(f.models)
	status, body = f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("review/reviewer"), `{"input":"x"}`)
	if status == http.StatusOK && !strings.Contains(body, "event: error") {
		t.Fatalf("run on the broken profile: %d %s", status, body)
	}
	if len(f.models) != before {
		t.Fatalf("the provider was called for the broken profile")
	}
}

// A model that fails the model rules is hidden from every view, including the profile's own
// model_default. The session stays listed and its history readable. A run that would use the
// hidden model, because it names none or names that one, stops with that file's error and never
// reaches a provider; a run that names another model of the profile works, and the picker's
// model-default update names the file for the hidden model and repairs by choosing another.
func TestAHiddenModelDefaultKeepsTheSessionReadableAndIsNeverSubstituted(t *testing.T) {
	logs := &lockedBuffer{}
	f := newLayoutFixtureWithBudget(t, nil, nil, func(c *Config) { c.HTTP.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", `{"input":"first message","model":"openai/large"}`); status != http.StatusOK || strings.Contains(body, "event: error") {
		t.Fatalf("setup run: %d %s", status, body)
	}
	f.overwrite(t, "models/openai/small.json", "{")
	small := filepath.Join(f.global, "models", "openai", "small.json")
	calls := len(f.models)

	// Listed, with the model nowhere in it.
	status, body := f.do(t, http.MethodGet, "/api/sessions", "")
	if status != http.StatusOK {
		t.Fatalf("list: %d %s", status, body)
	}
	var listed struct {
		Sessions []struct {
			ID           string `json:"id"`
			ModelDefault string `json:"model_default"`
			ModelFault   string `json:"model_fault"`
			Models       []string
			ModelOptions []struct {
				ID string `json:"id"`
			} `json:"model_options"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil || len(listed.Sessions) != 1 {
		t.Fatalf("decode: %v %s", err, body)
	}
	session := listed.Sessions[0]
	if strings.Join(session.Models, ",") != "openai/large" || session.ModelDefault != "" || len(session.ModelOptions) != 1 || session.ModelOptions[0].ID != "openai/large" {
		t.Errorf("the list offers the hidden model: models %v default %q options %+v", session.Models, session.ModelDefault, session.ModelOptions)
	}
	// The record says why there is no default, naming the file, so a client asks for a choice
	// and does not preselect another model; the snapshot carries the same.
	if !strings.Contains(session.ModelFault, small) {
		t.Errorf("model_fault = %q, want it to name %s", session.ModelFault, small)
	}
	if _, snapshot := f.do(t, http.MethodGet, "/api/sessions/local:demo", ""); !strings.Contains(snapshot, `"model_fault":"`) || !strings.Contains(snapshot, "small.json") {
		t.Errorf("the snapshot lacks model_fault:\n%s", snapshot)
	}
	if !strings.Contains(logs.String(), "model openai/small left out") || !strings.Contains(logs.String(), small) {
		t.Errorf("the log lacks the warning naming the file:\n%s", logs.String())
	}

	// The session and its history stay readable.
	for _, path := range []string{"/api/sessions/local:demo", "/api/sessions/local:demo/history", "/api/sessions/local:demo/tools"} {
		if status, body := f.do(t, http.MethodGet, path, ""); status != http.StatusOK {
			t.Errorf("GET %s = %d %s, want it readable", path, status, body)
		}
	}
	if _, body := f.do(t, http.MethodGet, "/api/sessions/local:demo", ""); !strings.Contains(body, "first message") {
		t.Errorf("the snapshot lost the first message:\n%s", body)
	}

	// A run that names no model, or the hidden one, stores its input and fails; no provider call.
	for name, request := range map[string]string{
		"no model named":     `{"input":"second message"}`,
		"the hidden model":   `{"input":"third message","model":"openai/small"}`,
		"hidden with effort": `{"input":"fourth message","model":"openai/small@high"}`,
	} {
		status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", request)
		if status != http.StatusOK || !strings.Contains(body, "event: error") {
			t.Errorf("%s: %d %s, want an error event", name, status, body)
		}
	}
	if len(f.models) != calls {
		t.Fatalf("a provider was called %d times for a run whose model is hidden", len(f.models)-calls)
	}
	if _, body := f.do(t, http.MethodGet, "/api/sessions/local:demo", ""); !strings.Contains(body, "second message") || !strings.Contains(body, "fourth message") {
		t.Errorf("a run that failed on the model did not keep its input:\n%s", body)
	}
	if !strings.Contains(logs.String(), small) {
		t.Errorf("the log does not name the file that failed the run:\n%s", logs.String())
	}

	// Another model of the profile runs.
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", `{"input":"fifth","model":"openai/large"}`); status != http.StatusOK || strings.Contains(body, "event: error") || f.lastModel() != "model-large" {
		t.Fatalf("run with the other model: %d %s (provider model %q)", status, body, f.lastModel())
	}

	// Choosing the hidden model as the default names its file; choosing another repairs.
	status, body = f.do(t, http.MethodPost, "/api/sessions/local:demo/model-default", `{"model_default":"openai/small"}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "small.json") {
		t.Errorf("model-default on the hidden model: %d %s, want 400 naming the file", status, body)
	}
	if status, body = f.do(t, http.MethodPost, "/api/sessions/local:demo/model-default", `{"model_default":"openai/large"}`); status != http.StatusOK {
		t.Fatalf("model-default on the other model: %d %s", status, body)
	}
	if _, body = f.do(t, http.MethodGet, "/api/sessions", ""); strings.Contains(body, "model_fault") || !strings.Contains(body, `"model_default":"openai/large"`) {
		t.Errorf("a repaired session still reports a model fault or lacks its default:\n%s", body)
	}
	if status, body = f.do(t, http.MethodPost, "/api/sessions/local:demo/run", `{"input":"sixth"}`); status != http.StatusOK || strings.Contains(body, "event: error") || f.lastModel() != "model-large" {
		t.Fatalf("run after the repair: %d %s (provider model %q)", status, body, f.lastModel())
	}
}

// A profile whose toolset parses but cannot be applied to the session is left out as a whole:
// the session lists with the others, and jad check names it with the session.
func TestAProfileWhoseToolsetCannotBeAppliedIsLeftOutOfTheSession(t *testing.T) {
	logs := &lockedBuffer{}
	f := newLayoutFixtureWithBudget(t, nil, nil, func(c *Config) { c.HTTP.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	f.overwrite(t, "tools/coding/none.json", `{"native":{},"mcp":{"nope/missing":{}}}`)
	// dev/coder (the default by name) and review/reviewer both use it; give the reviewer a good one.
	f.overwrite(t, "tools/coding/fine.json", `{"native":{},"mcp":{}}`)
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"coding/fine"}`)

	status, body := f.do(t, http.MethodGet, "/api/sessions", "")
	if status != http.StatusOK {
		t.Fatalf("list: %d %s", status, body)
	}
	listed := listedFor(t, []byte(body))
	if listed.ID != "demo" || strings.Join(listed.Profiles, ",") != "review/reviewer" || listed.Active != "review/reviewer" {
		t.Fatalf("listed = %+v, want demo with review/reviewer only", listed)
	}
	if !strings.Contains(logs.String(), "profile dev/coder left out") || !strings.Contains(logs.String(), "none.json") {
		t.Errorf("the log lacks the warning naming the toolset:\n%s", logs.String())
	}
	// jad check names the toolset once, on its own kind; a toolset that is fine alone but does
	// not apply to a session is named with the session (TestCheckFilesLoadsEveryProfileOfASession).
	if valid, out := f.checkFiles(t); valid || !strings.Contains(out, "nope/missing") {
		t.Errorf("jad check: valid=%v\n%s", valid, out)
	}
	// Named, the profile is an error and the reviewer does not run in its place.
	before := len(f.models)
	status, body = f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("dev/coder"), `{"input":"x"}`)
	if status == http.StatusOK && !strings.Contains(body, "event: error") {
		t.Fatalf("run on the profile: %d %s", status, body)
	}
	if len(f.models) != before {
		t.Fatalf("the provider was called for the left-out profile")
	}
}

// A native tool setting of a toolset that fails its rules leaves the default profile out of the
// session, and the profile that takes its place is the one the list, the profile info and the
// model-default route all use; a run on the profile named explicitly is an error.
func TestADefaultProfileWithABadNativeToolsetSettingMovesTheDefaultEverywhere(t *testing.T) {
	logs := &lockedBuffer{}
	f := newLayoutFixtureWithBudget(t, nil, nil, func(c *Config) { c.HTTP.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	f.overwrite(t, "tools/coding/none.json", `{"native":{"exec":{"no_such_setting":1}},"mcp":{}}`)
	f.overwrite(t, "tools/coding/fine.json", `{"native":{},"mcp":{}}`)
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"coding/fine"}`)

	status, body := f.do(t, http.MethodGet, "/api/sessions", "")
	if status != http.StatusOK {
		t.Fatalf("list: %d %s", status, body)
	}
	listed := listedFor(t, []byte(body))
	if listed.ID != "demo" || listed.Active != "review/reviewer" || strings.Join(listed.Profiles, ",") != "review/reviewer" {
		t.Fatalf("listed = %+v, want demo on review/reviewer only", listed)
	}
	if !strings.Contains(logs.String(), "profile dev/coder left out") || !strings.Contains(logs.String(), "no_such_setting") {
		t.Errorf("the log lacks the warning naming the setting:\n%s", logs.String())
	}
	// The picker's write goes to the profile the session runs, not to the one left out.
	if status, body = f.do(t, http.MethodPost, "/api/sessions/local:demo/model-default", `{"model_default":"openai/large"}`); status != http.StatusOK || !strings.Contains(body, `"active_profile":"review/reviewer"`) {
		t.Fatalf("model-default: %d %s", status, body)
	}
	if status, body = f.do(t, http.MethodPost, "/api/sessions/local:demo/run", `{"input":"hello"}`); status != http.StatusOK || strings.Contains(body, "event: error") {
		t.Fatalf("run: %d %s", status, body)
	}
	// Named, the profile is an error and the reviewer does not run in its place.
	before := len(f.models)
	status, body = f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("dev/coder"), `{"input":"x"}`)
	if status == http.StatusOK && !strings.Contains(body, "event: error") {
		t.Fatalf("run on the profile: %d %s", status, body)
	}
	if len(f.models) != before {
		t.Fatalf("the provider was called for the left-out profile")
	}
}

// The session list reloads a session when a profile or model file it left out is repaired or
// broken in place, logs each warning once however often the list is polled, and its ETag
// changes with what it offers.
func TestListSettingsFollowRepairedAndBrokenProfileAndModelFiles(t *testing.T) {
	loaded, fixture := layoutConfig(t, layouttest.Spec{
		SessionID: "demo",
		Models: map[string]map[string]any{
			"m/a": {"provider": "alpha", "model": "alpha-model", "api_key_env": "FIXTURE_KEY"},
			"m/b": {"provider": "beta", "model": "beta-model", "api_key_env": "FIXTURE_KEY"},
		},
		Profiles: map[string]map[string]any{
			"dev/aaa": {"models": []string{"m/a", "m/b"}, "model_default": "m/a"},
			"dev/bbb": {"models": []string{"m/a"}},
		},
		Session: map[string]any{"scope": map[string]any{"profiles": []string{"dev"}}},
	}, map[string]string{"FIXTURE_KEY": "key"})
	logs := &lockedBuffer{}
	resolver, err := NewResolver(loaded.Roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 1024}, WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()

	profile := filepath.Join(fixture.Root, "profiles", "dev", "bbb.json")
	model := filepath.Join(fixture.GlobalDir, "models", "m", "b.json")
	goodProfile := `{"models":["m/a"]}`
	goodModel := `{"provider":"beta","model":"beta-model","api_key_env":"FIXTURE_KEY"}`
	// Every file is dated well before the list, so a list can keep what it read; a change then
	// has to be seen by its stamp, not because the file looks new.
	settle := func(minutesAgo int) {
		old := time.Now().Add(-time.Duration(minutesAgo) * time.Minute)
		err := filepath.WalkDir(fixture.Root, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(path, old, old)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	list := func(etag string) (httpResponse, isolationListed) {
		t.Helper()
		response, err := resolver.ListSessions(context.Background(), etag, nil)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if response.NotModified {
			return httpResponse{etag: response.ETag, notModified: true}, isolationListed{}
		}
		return httpResponse{etag: response.ETag}, listedFor(t, response.Body)
	}

	write(profile, `{"colour":"red"}`)
	write(model, "{")
	settle(60)
	first, listed := list("")
	for poll := 0; poll < 3; poll++ {
		list("")
	}
	if strings.Join(listed.Profiles, ",") != "dev/aaa" || strings.Join(listed.Models, ",") != "m/a" {
		t.Fatalf("broken: listed = %+v, want dev/aaa and m/a only", listed)
	}
	if resolver.listSettingsCache.len() != 1 {
		t.Fatalf("the settings were not cached (%d entries), so a repair is not tested against a stamp", resolver.listSettingsCache.len())
	}
	if n := strings.Count(logs.String(), "profile dev/bbb left out"); n != 1 {
		t.Errorf("the profile warning was logged %d times over 4 polls, want 1:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "model m/b left out"); n != 1 {
		t.Errorf("the model warning was logged %d times over 4 polls, want 1:\n%s", n, logs.String())
	}

	// Repaired in place: both come back, and the list is not answered 304.
	write(profile, goodProfile)
	write(model, goodModel)
	settle(30)
	second, listed := list(first.etag)
	if second.notModified || second.etag == first.etag {
		t.Fatalf("a repaired profile and model did not change the list's ETag")
	}
	if strings.Join(listed.Profiles, ",") != "dev/aaa,dev/bbb" || strings.Join(listed.Models, ",") != "m/a,m/b" {
		t.Fatalf("repaired: listed = %+v, want both back", listed)
	}

	// Broken again: left out again and warned again.
	write(profile, `{"colour":"red"}`)
	write(model, "{")
	settle(20)
	third, listed := list(second.etag)
	if third.notModified || strings.Join(listed.Profiles, ",") != "dev/aaa" || strings.Join(listed.Models, ",") != "m/a" {
		t.Fatalf("broken again: listed = %+v", listed)
	}
	if n := strings.Count(logs.String(), "profile dev/bbb left out"); n != 2 {
		t.Errorf("the profile warning was logged %d times after it broke again, want 2", n)
	}
}

type httpResponse struct {
	etag        string
	notModified bool
}
