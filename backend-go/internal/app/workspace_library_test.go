package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"jad/backend-go/internal/contentlibrary"
	"jad/backend-go/internal/sessionfs"
)

// Decider fix B with Q3: in the workspace layout the HTTP library reads global.json's and the
// workspace's own folders, read at start, never a setting.json. A workspace sees its own
// library and the global one; another workspace does not see it; a skill or command in both
// is left out and named in ambiguous; an unknown workspace is 404.
func TestWorkspaceLibraryFollowsTheWorkspaceLayout(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	global := filepath.Join(home, ".jad")
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	put := func(path, text string) { writeAppLibraryFile(t, path, text) }
	put(filepath.Join(global, "global.json"), `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}],"libraries":{"prompt_dirs":["prompts"],"skill_dirs":["skills"],"command_dirs":["commands"]}}`)
	put(filepath.Join(global, "prompts", "shared.md"), "# Shared prompt\n\nGlobal.\n")
	put(filepath.Join(global, "skills", "both", "SKILL.md"), "---\nname: both\ndescription: Global copy.\n---\nG.\n")
	put(filepath.Join(global, "skills", "gonly", "SKILL.md"), "---\nname: gonly\ndescription: Global only.\n---\nG.\n")
	put(filepath.Join(global, "commands", "review.md"), "Review globally.\n")
	// A v1 setting beside each must not be read.
	put(filepath.Join(global, "setting.json"), `{"libraries":{"prompt_dirs":["leak"]}}`)
	put(filepath.Join(global, "leak", "leak.md"), "# Leaked prompt\n")
	for _, dir := range []string{a, b} {
		put(filepath.Join(dir, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
		put(filepath.Join(dir, ".jad", "setting.json"), `{"libraries":{"prompt_dirs":["leak"]}}`)
		put(filepath.Join(dir, ".jad", "leak", "leak.md"), "# Leaked prompt\n")
	}
	put(filepath.Join(a, ".jad", "workspace.json"), `{"libraries":{"prompt_dirs":["prompts"],"skill_dirs":["skills"],"command_dirs":["commands"]}}`)
	put(filepath.Join(a, ".jad", "prompts", "own.md"), "# Own prompt\n\nWorkspace a.\n")
	put(filepath.Join(a, ".jad", "skills", "both", "SKILL.md"), "---\nname: both\ndescription: Workspace copy.\n---\nW.\n")
	put(filepath.Join(a, ".jad", "commands", "review.md"), "Review here.\n")
	put(filepath.Join(a, ".jad", "commands", "deploy.md"), "Deploy.\n")
	put(filepath.Join(b, ".jad", "workspace.json"), `{}`)

	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: a, Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken, "HOME": home}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	server := httptest.NewServer(service.Handler)
	defer server.Close()
	catalogOf := func(query string) (int, contentlibrary.Catalog) {
		t.Helper()
		response := getAppLibrary(t, server.URL+"/api/library"+query, integrationAPIToken)
		defer response.Body.Close()
		var catalog contentlibrary.Catalog
		if response.StatusCode == http.StatusOK {
			if err := json.NewDecoder(response.Body).Decode(&catalog); err != nil {
				t.Fatal(err)
			}
		}
		return response.StatusCode, catalog
	}
	names := func(items []contentlibrary.Summary, key func(contentlibrary.Summary) string) []string {
		var out []string
		for _, item := range items {
			out = append(out, key(item))
		}
		return out
	}
	byName := func(s contentlibrary.Summary) string { return s.Name }
	byWord := func(s contentlibrary.Summary) string { return s.Command }
	for _, query := range []string{"", "?workspace=a"} {
		status, catalog := catalogOf(query)
		if status != http.StatusOK {
			t.Fatalf("%q: status %d", query, status)
		}
		if got := names(catalog.Prompts, byName); !slices.Equal(got, []string{"Own prompt", "Shared prompt"}) {
			t.Errorf("%q prompts = %q", query, got)
		}
		if got := names(catalog.Skills, byName); !slices.Equal(got, []string{"gonly"}) {
			t.Errorf("%q skills = %q (both must be left out)", query, got)
		}
		if got := names(catalog.Commands, byWord); !slices.Equal(got, []string{"deploy"}) {
			t.Errorf("%q commands = %q (review must be left out)", query, got)
		}
		notes := strings.Join(catalog.Ambiguous, "\n")
		if !strings.Contains(notes, "skill both is in the global and the workspace library") || !strings.Contains(notes, "command review is in the global and the workspace library") {
			t.Errorf("%q ambiguous = %q", query, catalog.Ambiguous)
		}
	}
	status, other := catalogOf("?workspace=b")
	if status != http.StatusOK {
		t.Fatalf("b: status %d", status)
	}
	if got := names(other.Prompts, byName); !slices.Equal(got, []string{"Shared prompt"}) {
		t.Errorf("b prompts = %q (a's own library or a setting.json leaked)", got)
	}
	if got := names(other.Skills, byName); !slices.Equal(got, []string{"both", "gonly"}) || len(other.Ambiguous) != 0 {
		t.Errorf("b skills = %q, ambiguous %q", got, other.Ambiguous)
	}
	if status, _ := catalogOf("?workspace=nope"); status != http.StatusNotFound {
		t.Errorf("unknown workspace status = %d, want 404", status)
	}
}
