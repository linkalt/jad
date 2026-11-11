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
	model := filepath.Join(fixture.Root, "models", "m", "b.json")
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
