package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// A list built with one shared scan per catalog must equal the lists built by loading
// each session on its own.
func TestListSessionsMatchesPerSessionLoads(t *testing.T) {
	resolver := newManySessionsResolver(t, 6, 3)
	response, err := resolver.ListSessions(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 6; index++ {
		id := "session-000" + string(rune('0'+index))
		location, err := sessionfs.Resolve(resolver.roots, "local:"+id)
		if err != nil {
			t.Fatal(err)
		}
		scoped, err := sessionfs.LoadSessionSettingsScoped(location, "", "", sessionfs.NewCatalogScans())
		if err != nil {
			t.Fatal(err)
		}
		plain, err := sessionfs.LoadSessionSettings(location, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if scoped.Profile != plain.Profile || scoped.ModelPath != plain.ModelPath || scoped.SettingPath != plain.SettingPath || strings.Join(scoped.Profiles, ",") != strings.Join(plain.Profiles, ",") {
			t.Fatalf("%s: scoped load %+v differs from plain load %+v", id, scoped.Profile, plain.Profile)
		}
		if !bytes.Contains(response.Body, []byte(`"`+id+`"`)) {
			t.Fatalf("list body has no %s", id)
		}
	}
}

// The scans are shared inside one list call only: a profile added after a list shows
// up in the next one.
func TestListSessionsSeesACatalogChangeBetweenCalls(t *testing.T) {
	resolver := newManySessionsResolver(t, 2, 1)
	ctx := context.Background()
	first, err := resolver.ListSessions(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(first.Body, []byte("docs/extra")) {
		t.Fatalf("first list already shows the profile that does not exist yet")
	}
	profiles := resolver.roots[0].Layout.Profiles
	extra := filepath.Join(profiles, "docs", "extra.json")
	if err := os.WriteFile(extra, []byte(`{"models":["openai/small"],"model_default":"openai/small","toolset":"coding/none"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settingPath := filepath.Join(filepath.Dir(resolver.roots[0].Directory), "sessions", "session-0000.json")
	data, err := os.ReadFile(settingPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), `"profiles":["dev"]`, `"profiles":["dev","docs/extra"]`, 1)
	if edited == string(data) {
		t.Fatalf("session file has no scope to edit: %s", data)
	}
	if err := os.WriteFile(settingPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := resolver.ListSessions(ctx, first.ETag, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.NotModified {
		t.Fatal("second list was not-modified after the session's scope changed")
	}
	if !bytes.Contains(second.Body, []byte("docs/extra")) {
		t.Fatalf("second list does not show the added profile: %s", second.Body)
	}
}
