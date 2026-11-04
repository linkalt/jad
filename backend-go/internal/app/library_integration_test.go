package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/contentlibrary"
	"jad/backend-go/internal/sessionfs"
)

func TestRootLibraryHTTPRoutesMatchFrontendContract(t *testing.T) {
	workspace := t.TempDir()
	writeAppLibraryFile(t, filepath.Join(workspace, "setting.json"), `{"session_roots":[{"id":"work","path":"sessions"}],"libraries":{"prompt_dirs":["shared/prompts"],"template_dirs":["shared/templates"],"skill_dirs":["shared/skills"]}}`)
	writeAppLibraryFile(t, filepath.Join(workspace, "shared/prompts/hello.md"), "---\ntitle: Hello prompt\ndescription: Say hello.\n---\nWrite a friendly greeting.\n")
	writeAppLibraryFile(t, filepath.Join(workspace, "shared/templates/basic.md"), "# Basic template\n\nTemplate instructions.\n")
	writeAppLibraryFile(t, filepath.Join(workspace, "shared/skills/greet/SKILL.md"), "---\nname: greet\ndescription: Say hello from this skill.\n---\nRespond kindly.\n")

	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir:  workspace,
		Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken},
	})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	defer service.Close()
	server := httptest.NewServer(service.Handler)
	defer server.Close()

	unauthorized, err := http.Get(server.URL + "/api/library")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized catalog status = %d, want 401", unauthorized.StatusCode)
	}

	catalogResponse := getAppLibrary(t, server.URL+"/api/library", integrationAPIToken)
	if catalogResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(catalogResponse.Body)
		catalogResponse.Body.Close()
		t.Fatalf("catalog status = %d, body=%s", catalogResponse.StatusCode, body)
	}
	if got := catalogResponse.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("catalog Cache-Control = %q, want no-store", got)
	}
	var catalog contentlibrary.Catalog
	if err := json.NewDecoder(catalogResponse.Body).Decode(&catalog); err != nil {
		catalogResponse.Body.Close()
		t.Fatal(err)
	}
	catalogResponse.Body.Close()
	if len(catalog.Prompts) != 1 || len(catalog.Templates) != 1 || len(catalog.Skills) != 1 {
		t.Fatalf("catalog = %+v", catalog)
	}
	if catalog.Prompts[0].Name != "Hello prompt" || catalog.Prompts[0].Description != "Say hello." {
		t.Errorf("prompt summary = %+v", catalog.Prompts[0])
	}

	assetResponse := getAppLibrary(t, server.URL+"/api/library/prompt/"+catalog.Prompts[0].ID, integrationAPIToken)
	if assetResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(assetResponse.Body)
		assetResponse.Body.Close()
		t.Fatalf("asset status = %d, body=%s", assetResponse.StatusCode, body)
	}
	var assetBody struct {
		Asset contentlibrary.Asset `json:"asset"`
	}
	if err := json.NewDecoder(assetResponse.Body).Decode(&assetBody); err != nil {
		assetResponse.Body.Close()
		t.Fatal(err)
	}
	assetResponse.Body.Close()
	if assetBody.Asset.Content != "Write a friendly greeting." || assetBody.Asset.ID != catalog.Prompts[0].ID {
		t.Errorf("asset response = %+v", assetBody.Asset)
	}

	missing := getAppLibrary(t, server.URL+"/api/library/prompt/"+fmt.Sprintf("%064d", 0), integrationAPIToken)
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("missing asset status = %d, want 404", missing.StatusCode)
	}
	unknownKind := getAppLibrary(t, server.URL+"/api/library/other/"+catalog.Prompts[0].ID, integrationAPIToken)
	unknownKind.Body.Close()
	if unknownKind.StatusCode != http.StatusNotFound {
		t.Errorf("unknown kind status = %d, want 404", unknownKind.StatusCode)
	}

	writeAppLibraryFile(t, filepath.Join(workspace, "setting.json"), `{"session_roots":[{"id":"work","path":"sessions"}],"libraries":null}`)
	invalidConfig := getAppLibrary(t, server.URL+"/api/library", integrationAPIToken)
	invalidConfig.Body.Close()
	if invalidConfig.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("invalid library config status = %d, want 422", invalidConfig.StatusCode)
	}
}

func getAppLibrary(t *testing.T, url, token string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func writeAppLibraryFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCustomCommandRoutesListAndExpand(t *testing.T) {
	workspace := t.TempDir()
	writeAppLibraryFile(t, filepath.Join(workspace, "setting.json"), `{"session_roots":[{"id":"work","path":"sessions"}],"libraries":{"command_dirs":["commands"]}}`)
	writeAppLibraryFile(t, filepath.Join(workspace, "commands/git/commit.md"), "---\ndescription: Commit\nargument-hint: <message>\n---\nCommit with the message: $ARGUMENTS\n")
	writeAppLibraryFile(t, filepath.Join(workspace, "commands/blank.md"), "$ARGUMENTS\n")
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: workspace, Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken}})
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

	response := getAppLibrary(t, server.URL+"/api/library", integrationAPIToken)
	var catalog contentlibrary.Catalog
	if err := json.NewDecoder(response.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(catalog.Commands) != 2 || catalog.Commands[1].Command != "git:commit" || catalog.Commands[1].ArgumentHint != "<message>" || !catalog.Commands[1].TakesArguments {
		t.Fatalf("commands = %+v", catalog.Commands)
	}
	post := func(id, body string, token string) (int, string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, server.URL+"/api/library/command/"+id+"/expand", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(data)
	}
	commit, blank := catalog.Commands[1].ID, catalog.Commands[0].ID
	if status, body := post(commit, `{"arguments":"fix the build"}`, integrationAPIToken); status != 200 || body != `{"command":"git:commit","text":"Commit with the message: fix the build"}`+"\n" {
		t.Fatalf("expand = %d %s", status, body)
	}
	if status, body := post(commit, `{}`, integrationAPIToken); status != 200 || !strings.Contains(body, `"text":"Commit with the message: "`) {
		t.Fatalf("expand without arguments = %d %s", status, body)
	}
	for name, test := range map[string]struct {
		id, body, token string
		status          int
	}{
		"no token":      {commit, `{}`, "wrong", http.StatusUnauthorized},
		"unknown field": {commit, `{"args":"x"}`, integrationAPIToken, http.StatusBadRequest},
		"not a string":  {commit, `{"arguments":1}`, integrationAPIToken, http.StatusBadRequest},
		"unknown id":    {strings.Repeat("0", 64), `{}`, integrationAPIToken, http.StatusNotFound},
		"empty text":    {blank, `{"arguments":"  "}`, integrationAPIToken, http.StatusUnprocessableEntity},
	} {
		if status, body := post(test.id, test.body, test.token); status != test.status {
			t.Errorf("%s: %d %s, want %d", name, status, body, test.status)
		}
	}
	// The asset route reads a command like any library item.
	asset := getAppLibrary(t, server.URL+"/api/library/command/"+commit, integrationAPIToken)
	asset.Body.Close()
	if asset.StatusCode != http.StatusOK {
		t.Fatalf("command asset status = %d", asset.StatusCode)
	}
}
