package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
