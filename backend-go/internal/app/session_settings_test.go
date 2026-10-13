package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestHTTPProfileModelAndSessionFilesRoutes(t *testing.T) {
	projectDir := t.TempDir()
	for _, directory := range []string{
		filepath.Join(projectDir, "private"),
		filepath.Join(projectDir, "node_modules"),
		filepath.Join(projectDir, "src"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(projectDir, "README.md"):              "readme",
		filepath.Join(projectDir, ".hidden"):                "hidden",
		filepath.Join(projectDir, "private", "secret.md"):   "secret",
		filepath.Join(projectDir, "node_modules", "dep.go"): "vendor",
		filepath.Join(projectDir, "src", "main.go"):         "main",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	shared := map[string]any{"toolset": "coding/default"}
	green := map[string]any{"models": []string{"openai/model", "openai/alternate"}, "model_default": "openai/model"}
	blue := map[string]any{"models": []string{"openai/model"}, "model_default": "openai/model"}
	for key, value := range shared {
		green[key], blue[key] = value, value
	}
	config, fixture := layoutConfig(t, layouttest.Spec{
		SessionID: "settings", CWD: projectDir,
		Server: map[string]any{"max_body_bytes": 4096, "sse_max_buffered_bytes": 4096, "max_inline_part_bytes": 64},
		Models: map[string]map[string]any{
			"openai/model":     {"provider": "openai", "model": "gpt-main", "api_key_env": "FIXTURE_OPENAI_KEY"},
			"openai/alternate": {"provider": "openai", "model": "gpt-alternate", "api_key_env": "FIXTURE_OPENAI_KEY"},
		},
		Profiles:      map[string]map[string]any{"dev/green": green, "dev/blue": blue},
		ActiveProfile: "dev/green",
		Native:        map[string]any{"read": map[string]any{"paths": []map[string]string{{"action": "deny", "path": filepath.Join(projectDir, "private")}}}},
		Settings:      map[string]any{"persistence": map[string]any{"durability": "process"}},
	}, map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "fixture-key"})
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	api := httptest.NewServer(service.Handler)
	defer func() {
		service.Close()
		api.Close()
	}()

	postJSON := func(path, body string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, api.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	profileResponse := postJSON("/api/sessions/local:settings/profile", `{"active_profile":"dev/blue"}`)
	profileBody, err := io.ReadAll(profileResponse.Body)
	profileResponse.Body.Close()
	if err != nil || profileResponse.StatusCode != http.StatusOK {
		t.Fatalf("profile update status=%d err=%v body=%s", profileResponse.StatusCode, err, profileBody)
	}
	var profileUpdate struct {
		ActiveProfile string   `json:"active_profile"`
		Profiles      []string `json:"profiles"`
		ModelDefault  string   `json:"model_default"`
		Models        []string `json:"models"`
		Changed       bool     `json:"changed"`
	}
	if err := json.Unmarshal(profileBody, &profileUpdate); err != nil {
		t.Fatal(err)
	}
	if profileUpdate.ActiveProfile != "dev/blue" || !reflect.DeepEqual(profileUpdate.Profiles, []string{"dev/blue", "dev/green"}) || profileUpdate.ModelDefault != "openai/model" || !reflect.DeepEqual(profileUpdate.Models, []string{"openai/model"}) || !profileUpdate.Changed {
		t.Fatalf("profile update = %+v", profileUpdate)
	}

	var storedSession map[string]json.RawMessage
	storedBytes, err := os.ReadFile(fixture.SessionFile)
	if err != nil || json.Unmarshal(storedBytes, &storedSession) != nil {
		t.Fatalf("read updated session: err=%v", err)
	}
	var storedActive string
	if json.Unmarshal(storedSession["active_profile"], &storedActive) != nil || storedActive != "dev/blue" {
		t.Fatalf("session.json active_profile = %s, want dev/blue", storedSession["active_profile"])
	}
	var storedSettings map[string]json.RawMessage
	if json.Unmarshal(storedSession["settings"], &storedSettings) != nil || len(storedSettings["persistence"]) == 0 {
		t.Fatalf("the profile switch dropped the session settings: %s", storedSession["settings"])
	}

	filesResponse := getAPI(t, api.URL, "/api/sessions/local:settings/files?profile=dev/blue", integrationAPIToken, "")
	filesBody, err := io.ReadAll(filesResponse.Body)
	filesResponse.Body.Close()
	if err != nil || filesResponse.StatusCode != http.StatusOK {
		t.Fatalf("files status=%d err=%v body=%s", filesResponse.StatusCode, err, filesBody)
	}
	var listing struct {
		Files     []string `json:"files"`
		Truncated bool     `json:"truncated"`
	}
	if err := json.Unmarshal(filesBody, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Truncated || !reflect.DeepEqual(listing.Files, []string{"README.md", "src/main.go"}) {
		t.Fatalf("file listing = %+v", listing)
	}

	modelResponse := postJSON("/api/sessions/local:settings/model-default?profile=dev/green", `{"model_default":"openai/alternate"}`)
	modelBody, err := io.ReadAll(modelResponse.Body)
	modelResponse.Body.Close()
	if err != nil || modelResponse.StatusCode != http.StatusOK {
		t.Fatalf("model update status=%d err=%v body=%s", modelResponse.StatusCode, err, modelBody)
	}
	var modelUpdate struct {
		ActiveProfile string   `json:"active_profile"`
		Profiles      []string `json:"profiles"`
		ModelDefault  string   `json:"model_default"`
		Models        []string `json:"models"`
		Model         struct {
			Provider string `json:"provider"`
			Name     string `json:"model"`
		} `json:"model"`
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(modelBody, &modelUpdate); err != nil {
		t.Fatal(err)
	}
	if modelUpdate.ActiveProfile != "dev/green" || modelUpdate.ModelDefault != "openai/alternate" || modelUpdate.Model.Provider != "openai" || modelUpdate.Model.Name != "gpt-alternate" || !modelUpdate.Changed || !reflect.DeepEqual(modelUpdate.Models, []string{"openai/alternate", "openai/model"}) {
		t.Fatalf("model update = %+v", modelUpdate)
	}

	badProfile := postJSON("/api/sessions/local:settings/profile", `{"active_profile":"not-defined"}`)
	badProfileBody, _ := io.ReadAll(badProfile.Body)
	badProfile.Body.Close()
	if badProfile.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown profile status=%d body=%s", badProfile.StatusCode, badProfileBody)
	}

	storedProfile, err := os.ReadFile(filepath.Join(fixture.Root, "profiles", "dev", "green.json"))
	if err != nil || !strings.Contains(string(storedProfile), `"model_default": "openai/alternate"`) {
		t.Fatalf("the model choice was not written to the profile file: err=%v\n%s", err, storedProfile)
	}
}

func TestHTTPSettingsRequestsRespectBodyLimit(t *testing.T) {
	config, _ := layoutConfig(t, layouttest.Spec{
		SessionID: "missing", Server: map[string]any{"max_body_bytes": 32, "sse_max_buffered_bytes": 4096, "max_inline_part_bytes": 64},
	}, map[string]string{"JAD_API_TOKEN": integrationAPIToken})
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	api := httptest.NewServer(service.Handler)
	defer func() {
		service.Close()
		api.Close()
	}()
	request, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:missing/profile", strings.NewReader(fmt.Sprintf(`{"active_profile":"%s"}`, strings.Repeat("a", 64))))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized settings body status=%d body=%s", response.StatusCode, body)
	}
}

// A profile file that changed during an update answers 409, so the client can read it
// again and retry; the other settings errors stay 400.
func TestMapProfileSettingsErrorAnswersConflictWith409(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code int
	}{
		"conflict":     {&sessionfs.ConflictError{Message: "p.json changed while it was being updated"}, http.StatusConflict},
		"wrapped":      {fmt.Errorf("write: %w", &sessionfs.ConflictError{Message: "x"}), http.StatusConflict},
		"client error": {&sessionfs.ClientError{Message: "bad"}, http.StatusBadRequest},
	} {
		var status *httpapi.StatusError
		if !errors.As(mapProfileSettingsError(tc.err), &status) || status.Code != tc.code {
			t.Errorf("%s: got %v, want status %d", name, mapProfileSettingsError(tc.err), tc.code)
		}
	}
}

// session_files.enabled false in the session settings makes the files route answer 403.
func TestHTTPSessionFilesRouteIsForbiddenWhenDisabled(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "README.md"), []byte("readme"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		settings map[string]any
		status   int
	}{
		"enabled":  {map[string]any{"session_files": map[string]any{"enabled": true}}, http.StatusOK},
		"disabled": {map[string]any{"session_files": map[string]any{"enabled": false}}, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			config, _ := layoutConfig(t, layouttest.Spec{SessionID: "files", CWD: projectDir, Settings: tc.settings},
				map[string]string{"JAD_API_TOKEN": integrationAPIToken})
			service, err := NewService(config)
			if err != nil {
				t.Fatalf("NewService() error = %v", err)
			}
			api := httptest.NewServer(service.Handler)
			defer func() {
				service.Close()
				api.Close()
			}()
			response := getAPI(t, api.URL, "/api/sessions/local:files/files", integrationAPIToken, "")
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d, body = %s", response.StatusCode, tc.status, body)
			}
			if tc.status == http.StatusForbidden && !strings.Contains(string(body), "file listing is disabled") {
				t.Fatalf("body = %s", body)
			}
		})
	}
}
