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

	"example.com/agent-harness/backend-go/internal/httpapi"
	"example.com/agent-harness/backend-go/internal/sessionfs"
)

func TestHTTPProfileModelAndSessionFilesRoutes(t *testing.T) {
	workspace := t.TempDir()
	projectDir := filepath.Join(workspace, "project")
	sessionDir := filepath.Join(workspace, "sessions", "settings")
	for _, directory := range []string{
		sessionDir,
		projectDir,
		filepath.Join(projectDir, "private"),
		filepath.Join(projectDir, "vendor"),
		filepath.Join(projectDir, "src"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(projectDir, "README.md"):            "readme",
		filepath.Join(projectDir, ".hidden"):              "hidden",
		filepath.Join(projectDir, "private", "secret.md"): "secret",
		filepath.Join(projectDir, "vendor", "dep.go"):     "vendor",
		filepath.Join(projectDir, "src", "main.go"):       "main",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	setting := map[string]any{
		"cwd":   projectDir,
		"paths": json.RawMessage(`{"active_profile":"green","profiles":{"green":{"models":["model.json","alternate.json"]},"blue":{"models":["model.json"],"model_default":"model.json"}}}`),
		"session_files": map[string]any{
			"enabled":            true,
			"max_depth":          4,
			"max_entries":        100,
			"max_results":        100,
			"ignore_directories": []string{"vendor"},
		},
		"tools": map[string]any{
			"native": map[string]any{
				"read": map[string]any{
					"paths": []map[string]string{{"action": "deny", "path": filepath.Join(projectDir, "private")}},
				},
			},
		},
		"untouched":   map[string]any{"enabled": true},
		"persistence": map[string]any{"durability": "process"},
	}
	settingBytes, err := json.Marshal(setting)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), settingBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, model := range map[string]string{
		"model.json":     `{"provider":"openai","model":"gpt-main","api_key_env":"FIXTURE_OPENAI_KEY"}`,
		"alternate.json": `{"provider":"openai","model":"gpt-alternate","api_key_env":"FIXTURE_OPENAI_KEY"}`,
	} {
		if err := os.WriteFile(filepath.Join(sessionDir, name), []byte(model), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	rootSetting := `{"session_roots":[{"id":"alpha","path":"sessions"}],"server":{"max_body_bytes":4096,"sse_max_buffered_bytes":4096,"max_inline_part_bytes":64}}`
	if err := os.WriteFile(filepath.Join(workspace, "setting.json"), []byte(rootSetting), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir: workspace,
		Environment: map[string]string{
			"AGENT_API_TOKEN":    integrationAPIToken,
			"FIXTURE_OPENAI_KEY": "fixture-key",
		},
	})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
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

	profileResponse := postJSON("/api/sessions/alpha:settings/profile", `{"active_profile":"blue"}`)
	profileBody, err := io.ReadAll(profileResponse.Body)
	profileResponse.Body.Close()
	if err != nil || profileResponse.StatusCode != http.StatusOK {
		t.Fatalf("profile update status=%d err=%v body=%s", profileResponse.StatusCode, err, profileBody)
	}
	var profileUpdate struct {
		ActiveProfile string   `json:"active_profile"`
		PathProfiles  []string `json:"path_profiles"`
		ModelDefault  string   `json:"model_default"`
		Models        []string `json:"models"`
		Changed       bool     `json:"changed"`
	}
	if err := json.Unmarshal(profileBody, &profileUpdate); err != nil {
		t.Fatal(err)
	}
	if profileUpdate.ActiveProfile != "blue" || !reflect.DeepEqual(profileUpdate.PathProfiles, []string{"green", "blue"}) || profileUpdate.ModelDefault != "model.json" || !reflect.DeepEqual(profileUpdate.Models, []string{"model.json"}) || !profileUpdate.Changed {
		t.Fatalf("profile update = %+v", profileUpdate)
	}

	var writtenSetting map[string]json.RawMessage
	storedSetting, err := os.ReadFile(filepath.Join(sessionDir, "setting.json"))
	if err != nil || json.Unmarshal(storedSetting, &writtenSetting) != nil {
		t.Fatalf("read updated setting: err=%v", err)
	}
	var paths map[string]json.RawMessage
	if err := json.Unmarshal(writtenSetting["paths"], &paths); err != nil {
		t.Fatal(err)
	}
	var profiles map[string]json.RawMessage
	if err := json.Unmarshal(paths["profiles"], &profiles); err != nil {
		t.Fatal(err)
	}
	var green map[string]json.RawMessage
	var blue map[string]json.RawMessage
	if json.Unmarshal(profiles["green"], &green) != nil || json.Unmarshal(profiles["blue"], &blue) != nil {
		t.Fatal("profile storage settings were not written as objects")
	}
	for profileName, profilePaths := range map[string]map[string]json.RawMessage{"green": green, "blue": blue} {
		for _, field := range []string{"messages", "messages_backup", "compaction_cache", "usage_cache", "cursor_cache", "statecontext", "staging", "http_debug"} {
			if len(profilePaths[field]) == 0 {
				t.Errorf("profile %s missing pinned storage path %s", profileName, field)
			}
		}
	}
	var untouched map[string]bool
	if json.Unmarshal(writtenSetting["untouched"], &untouched) != nil || !untouched["enabled"] {
		t.Fatalf("unrelated setting was not preserved: %s", writtenSetting["untouched"])
	}

	filesResponse := getAPI(t, api.URL, "/api/sessions/alpha:settings/files?profile=blue", integrationAPIToken, "")
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

	modelResponse := postJSON("/api/sessions/alpha:settings/model-default?profile=green", `{"model_default":"alternate.json"}`)
	modelBody, err := io.ReadAll(modelResponse.Body)
	modelResponse.Body.Close()
	if err != nil || modelResponse.StatusCode != http.StatusOK {
		t.Fatalf("model update status=%d err=%v body=%s", modelResponse.StatusCode, err, modelBody)
	}
	var modelUpdate struct {
		ActiveProfile string   `json:"active_profile"`
		PathProfiles  []string `json:"path_profiles"`
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
	if modelUpdate.ActiveProfile != "green" || modelUpdate.ModelDefault != "alternate.json" || modelUpdate.Model.Provider != "openai" || modelUpdate.Model.Name != "gpt-alternate" || !modelUpdate.Changed || !reflect.DeepEqual(modelUpdate.Models, []string{"model.json", "alternate.json"}) {
		t.Fatalf("model update = %+v", modelUpdate)
	}

	badProfile := postJSON("/api/sessions/alpha:settings/profile", `{"active_profile":"not-defined"}`)
	badProfileBody, _ := io.ReadAll(badProfile.Body)
	badProfile.Body.Close()
	if badProfile.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown profile status=%d body=%s", badProfile.StatusCode, badProfileBody)
	}

	setting["session_files"].(map[string]any)["enabled"] = false
	disabledBytes, err := json.Marshal(setting)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), disabledBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	disabled := getAPI(t, api.URL, "/api/sessions/alpha:settings/files", integrationAPIToken, "")
	disabledBody, _ := io.ReadAll(disabled.Body)
	disabled.Body.Close()
	if disabled.StatusCode != http.StatusForbidden || !strings.Contains(string(disabledBody), "file listing is disabled") {
		t.Fatalf("disabled files status=%d body=%s", disabled.StatusCode, disabledBody)
	}
}

func TestHTTPSettingsRequestsRespectBodyLimit(t *testing.T) {
	workspace := t.TempDir()
	rootSetting := `{"session_roots":[{"id":"alpha","path":"sessions"}],"server":{"max_body_bytes":32,"sse_max_buffered_bytes":4096,"max_inline_part_bytes":64}}`
	if err := os.WriteFile(filepath.Join(workspace, "setting.json"), []byte(rootSetting), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: workspace, Environment: map[string]string{"AGENT_API_TOKEN": integrationAPIToken}})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	api := httptest.NewServer(service.Handler)
	defer func() {
		service.Close()
		api.Close()
	}()
	request, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/alpha:missing/profile", strings.NewReader(fmt.Sprintf(`{"active_profile":"%s"}`, strings.Repeat("a", 64))))
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
