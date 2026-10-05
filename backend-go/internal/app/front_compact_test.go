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

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

func TestHTTPFrontCompactArchivesSelectedTailAndInvalidatesCaches(t *testing.T) {
	service, sessionDir := newFrontCompactService(t, `{"enabled":true,"delete_mode":"recoverable"}`)
	defer service.Close()
	api := httptest.NewServer(service.Handler)
	defer api.Close()

	prefix := `{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]}`
	target := `{"type":"message","role":"user","content":[{"type":"input_text","text":"trim from here"}]}`
	tail := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"removed answer"}]}]}}`
	messagesPath := filepath.Join(sessionDir, "messages.jsonl")
	original := prefix + "\n" + target + "\n" + tail + "\n"
	if err := os.WriteFile(messagesPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"history-cache.json", "usage-cache.json", "cursor-cache.json", "statecontext.json"} {
		path := filepath.Join(sessionDir, "staging", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	request := frontCompactRequest(t, api.URL, integrationAPIToken, "session-user-o"+fmt.Sprint(len(prefix)+1))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("front-compact status=%d body=%s", response.StatusCode, body)
	}
	var result struct {
		FrontCompaction struct {
			DeleteMode  string `json:"delete_mode"`
			ArchiveName string `json:"archive_name"`
		} `json:"front_compaction"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if result.FrontCompaction.DeleteMode != "recoverable" || result.FrontCompaction.ArchiveName == "" {
		t.Fatalf("front-compaction response = %+v", result.FrontCompaction)
	}
	current, err := os.ReadFile(messagesPath)
	if err != nil || string(current) != prefix+"\n" {
		t.Fatalf("active transcript = %q err=%v", current, err)
	}
	archivePath := messagesPath + ".deleted-history/" + result.FrontCompaction.ArchiveName
	archived, err := os.ReadFile(archivePath)
	if err != nil || string(archived) != target+"\n"+tail+"\n" {
		t.Fatalf("archive = %q err=%v", archived, err)
	}
	for _, name := range []string{"history-cache.json", "usage-cache.json", "cursor-cache.json", "statecontext.json"} {
		if _, err := os.Stat(filepath.Join(sessionDir, "staging", name)); !os.IsNotExist(err) {
			t.Errorf("front compaction left cache %s: %v", name, err)
		}
	}
}

func TestHTTPFrontCompactHonorsDisabledSetting(t *testing.T) {
	service, sessionDir := newFrontCompactService(t, `{"enabled":false,"delete_mode":"permanent"}`)
	defer service.Close()
	api := httptest.NewServer(service.Handler)
	defer api.Close()

	messagesPath := filepath.Join(sessionDir, "messages.jsonl")
	original := `{"type":"message","role":"user","content":[{"type":"input_text","text":"do not remove"}]}` + "\n"
	if err := os.WriteFile(messagesPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	request := frontCompactRequest(t, api.URL, integrationAPIToken, "session-user-o0")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("disabled front-compact status=%d body=%s", response.StatusCode, body)
	}
	current, err := os.ReadFile(messagesPath)
	if err != nil || string(current) != original {
		t.Fatalf("disabled front-compact changed transcript: %q err=%v", current, err)
	}
}

func newFrontCompactService(t *testing.T, frontSetting string) (*Service, string) {
	t.Helper()
	workspace := t.TempDir()
	sessionDir := filepath.Join(workspace, "sessions", "one")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootSetting := `{"session_roots":[{"id":"local","path":"sessions"}],"server":{"max_body_bytes":4096,"sse_max_buffered_bytes":4096}}`
	if err := os.WriteFile(filepath.Join(workspace, "setting.json"), []byte(rootSetting), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionSetting := fmt.Sprintf(`{"cwd":%q,"front_compaction":%s,"persistence":{"durability":"process","max_item_bytes":1048576,"response_metadata":"full"}}`, workspace, frontSetting)
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(sessionSetting), 0o600); err != nil {
		t.Fatal(err)
	}
	model := `{"provider":"openai","model":"offline-fixture","api_key_env":"UNSET_PROVIDER_KEY"}`
	if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), []byte(model), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: workspace, Environment: map[string]string{"AGENT_API_TOKEN": integrationAPIToken}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	return service, sessionDir
}

func frontCompactRequest(t *testing.T, address, token, messageID string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, address+"/api/sessions/local:one/front-compact", strings.NewReader(`{"message_id":"`+messageID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	return request
}
