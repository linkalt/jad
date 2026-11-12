package app

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A catalog file whose name is not an identifier ("file (1).json", the copy a file manager
// makes) used to fail every session that read its kind: the session list dropped them all. It is
// now left out with a warning that names it, the sessions keep listing and running, jad check
// names it, and renaming it brings it back without a restart.
func TestBadCatalogFileNamesDoNotFailSessionsAndComeBackWhenRenamed(t *testing.T) {
	logs := &lockedBuffer{}
	f := newLayoutFixtureWithBudget(t, nil, nil, func(c *Config) { c.HTTP.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	reviewer := `{"models":["openai/small"]}`
	copies := map[string]string{
		"profiles/review/reviewer (1).json": reviewer,
		"profiles/loose.json":               reviewer,
		"profiles/bad cat/x.json":           reviewer,
		"models/openai/small (copy).json":   `{"provider":"openai","model":"copy-model","api_key_env":"FIXTURE_KEY"}`,
		"models/loose.json":                 `{}`,
	}
	for rel, text := range copies {
		path := filepath.Join(f.root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		f.overwrite(t, filepath.FromSlash(rel), text)
	}

	// The session is listed (it used to be dropped), with the files that can be named.
	status, etag, sessions := f.listSessions(t, "")
	if status != http.StatusOK || len(sessions) != 1 || sessions[0].ID != "demo" {
		t.Fatalf("list: %d %+v", status, sessions)
	}
	if got := strings.Join(sessions[0].Profiles, ","); got != "dev/coder,review/reviewer" {
		t.Fatalf("profiles = %q, want only the files that can be named", got)
	}
	for poll := 0; poll < 3; poll++ {
		f.listSessions(t, "")
	}
	for rel := range copies {
		skipped := filepath.Join(f.root, filepath.FromSlash(rel))
		if strings.HasPrefix(rel, "profiles/bad cat") {
			skipped = filepath.Join(f.root, "profiles", "bad cat")
		}
		if n := strings.Count(logs.String(), skipped+" left out"); n != 1 {
			t.Errorf("%s was warned about %d times over 4 polls, want once:\n%s", rel, n, logs.String())
		}
	}

	// A run works, with the profile's own model.
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", `{"input":"hi"}`); status != http.StatusOK || strings.Contains(body, "event: error") {
		t.Fatalf("run: %d %s", status, body)
	}
	if got := f.lastModel(); got != "model-small" {
		t.Fatalf("the run used %q", got)
	}

	// The catalog listings answer, with rows for the files that can be opened only.
	for kind, banned := range map[string]string{"profiles": "reviewer (1)", "models": "small (copy)"} {
		reply := f.catalogCall(t, "GET", "/api/catalog/"+kind, "", nil)
		if reply.status != http.StatusOK || strings.Contains(reply.body, banned) || strings.Contains(reply.body, "loose") {
			t.Errorf("catalog %s = %d %s", kind, reply.status, reply.body)
		}
	}
	// Naming a bad one is refused and nothing else answers in its place.
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile=review%2Freviewer%20%281%29", `{"input":"x"}`); status == http.StatusOK && !strings.Contains(body, "event: error") {
		t.Fatalf("a run on a skipped profile: %d %s", status, body)
	}

	// jad check names every one of them.
	valid, report := f.checkFiles(t)
	if valid {
		t.Fatalf("check passed with bad names:\n%s", report)
	}
	for _, want := range []string{"reviewer (1).json", "loose.json", "bad cat", "small (copy).json"} {
		if !strings.Contains(report, want) {
			t.Errorf("check does not name %q:\n%s", want, report)
		}
	}

	// Renamed, the files are back at the next read: the list is not answered 304 and offers them.
	rename := func(from, to string) {
		t.Helper()
		if err := os.Rename(filepath.Join(f.root, from), filepath.Join(f.root, to)); err != nil {
			t.Fatal(err)
		}
	}
	rename("profiles/review/reviewer (1).json", "profiles/review/second.json")
	rename("models/openai/small (copy).json", "models/openai/copy.json")
	status, second, sessions := f.listSessions(t, etag)
	if status != http.StatusOK || second == etag || len(sessions) != 1 {
		t.Fatalf("after the rename: %d etag %q (was %q)", status, second, etag)
	}
	if got := strings.Join(sessions[0].Profiles, ","); got != "dev/coder,review/reviewer,review/second" {
		t.Fatalf("profiles after the rename = %q", got)
	}
}
