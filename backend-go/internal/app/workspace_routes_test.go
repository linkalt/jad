package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
)

// /api/workspaces lists the workspaces of the workspace layout (a skipped one with why), and
// the catalog routes take ?scope=: a profile PUT with scope workspace:b lands in b's .jad, an
// unknown workspace is 404 (decider fix D).
func TestWorkspaceRoutesListWorkspacesAndScopeCatalogWrites(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	global := filepath.Join(home, ".jad")
	a, b, gone := filepath.Join(base, "a"), filepath.Join(base, "b"), filepath.Join(base, "gone")
	put := func(path, text string) { writeAppLibraryFile(t, path, text) }
	put(filepath.Join(global, "global.json"), `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`","label":"Bee"},{"id":"gone","path":"`+gone+`"}]}`)
	put(filepath.Join(global, "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY"}`)
	for _, dir := range []string{a, b} {
		put(filepath.Join(dir, ".jad", "workspace.json"), `{}`)
		put(filepath.Join(dir, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
	}
	put(filepath.Join(b, ".jad", "profiles", "dev", "base.json"), `{"models":["openai/default"],"model_default":"openai/default"}`)
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
	do := func(method, path string, body []byte, headers map[string]string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	response := do(http.MethodGet, "/api/workspaces", nil, nil)
	var listed struct {
		Workspaces []httpapi.WorkspaceInfo `json:"workspaces"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(listed.Workspaces) != 3 || listed.Workspaces[0].ID != "a" || !listed.Workspaces[0].Start || listed.Workspaces[1].Label != "Bee" || listed.Workspaces[2].Unavailable == "" {
		t.Fatalf("workspaces = %+v", listed.Workspaces)
	}

	profile := []byte(`{"models":["openai/default"]}`)
	response = do(http.MethodPut, "/api/catalog/profiles/dev/coder?scope=workspace:b", profile, map[string]string{"If-None-Match": "*"})
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("scoped PUT status = %d", response.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(b, ".jad", "profiles", "dev", "coder.json")); err != nil {
		t.Fatalf("not written to b: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a, ".jad", "profiles", "dev", "coder.json")); err == nil {
		t.Fatal("written to the start workspace")
	}
	for path, want := range map[string]int{
		"/api/catalog/profiles?scope=workspace:nope": http.StatusNotFound, "/api/catalog/profiles?scope=global": http.StatusNotFound,
		"/api/catalog/mcp?scope=workspace:a": http.StatusNotFound, "/api/catalog/profiles?scope=workspace:b": http.StatusOK,
		"/api/catalog/profiles": http.StatusOK, "/api/catalog/models?scope=global": http.StatusOK,
	} {
		response := do(http.MethodGet, path, nil, nil)
		response.Body.Close()
		if response.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, response.StatusCode, want)
		}
	}
	// The unscoped profile list is the start workspace's, which has none.
	response = do(http.MethodGet, "/api/catalog/profiles", nil, nil)
	var files struct {
		Files []httpapi.CatalogFileInfo `json:"files"`
	}
	_ = json.NewDecoder(response.Body).Decode(&files)
	response.Body.Close()
	if len(files.Files) != 0 {
		t.Fatalf("unscoped profiles = %+v", files.Files)
	}

	// A profile fork of a session in b copies b's profile into b (the session's workspace).
	response = do(http.MethodPost, "/api/sessions/b:s1/fork?profile=dev/base", []byte(`{"profile":"try","state":"none"}`), nil)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("fork in b = %d", response.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(b, ".jad", "profiles", "dev", "try.json")); err != nil {
		t.Fatalf("the forked profile is not in b: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a, ".jad", "profiles", "dev", "try.json")); err == nil {
		t.Fatal("the forked profile landed in the start workspace")
	}
}
