package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// A run stores the user message first and only then reads the model file,
// connects tool servers and resolves the credential, as the TypeScript backend
// does. A failure in any of those steps is reported on the event stream (HTTP
// 200 with an error event), and the message stays in the transcript.
func TestRunStoresInputBeforeModelToolsAndCredentialAreResolved(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "sessions")
	cases := []struct {
		name  string
		model string
	}{
		{"missing credential", `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_UNSET_KEY"}`},
		{"invalid model file", `{"provider":"openai","api_key_env":"not a name"}`},
	}
	for _, test := range cases {
		dir := filepath.Join(root, strings.ReplaceAll(test.name, " ", "-"))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, filepath.Join(dir, "setting.json"), `{"cwd":`+quote(workspace)+`}`)
		writeFixtureFile(t, filepath.Join(dir, "model.json"), test.model)
	}

	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir: workspace,
		Environment: map[string]string{
			"AGENT_API_TOKEN":       integrationAPIToken,
			"JAD_SESSIONS_DIR":      root,
			"FIXTURE_KEY":           "k",
			"JAD_ROOT_SETTING_PATH": "",
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

	for _, test := range cases {
		id := strings.ReplaceAll(test.name, " ", "-")
		t.Run(test.name, func(t *testing.T) {
			response := postRunBody(t, api.URL+"/api/sessions/"+id+"/run", `{"input":"keep this message"}`)
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("run status = %d, want 200 with an error event; body=%s", response.StatusCode, body)
			}
			text := string(body)
			if !strings.Contains(text, "event: error") || !strings.Contains(text, "The run failed. Response details were withheld; check server logs for diagnostics.") {
				t.Fatalf("run stream lacks the generic error event:\n%s", text)
			}
			if strings.Contains(text, "api_key_env") || strings.Contains(text, "FIXTURE_UNSET_KEY") {
				t.Fatalf("run stream leaked configuration detail:\n%s", text)
			}
			transcript, err := os.ReadFile(filepath.Join(root, id, "messages.jsonl"))
			if err != nil || !strings.Contains(string(transcript), "keep this message") {
				t.Fatalf("user message was not stored before the failure: %v\n%s", err, transcript)
			}
		})
	}

	// The list reads only the settings, so a session whose model file fails
	// validation is still listed, with the placeholder model summary.
	list := getAPI(t, api.URL, "/api/sessions", integrationAPIToken, "")
	listBody, _ := io.ReadAll(list.Body)
	list.Body.Close()
	if !strings.Contains(string(listBody), `"id":"invalid-model-file"`) || !strings.Contains(string(listBody), `"model":{"provider":"openai","model":"invalid-model-file"}`) {
		t.Fatalf("session with an invalid model file is missing from the list or mis-summarised:\n%s", listBody)
	}
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func quote(value string) string { return `"` + strings.ReplaceAll(value, `\`, `\\`) + `"` }

func postRunBody(t *testing.T, address, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, address, strings.NewReader(body))
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
