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
	for _, name := range []string{"usage-cache.json", "cursor-cache.json", "statecontext.json"} {
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
	if _, err := os.Stat(filepath.Join(sessionDir, "staging", "cursor-cache.json")); !os.IsNotExist(err) {
		t.Errorf("front compaction left the cursor cache: %v", err)
	}
	if kept, err := os.ReadFile(filepath.Join(sessionDir, "staging", "usage-cache.json")); err != nil || string(kept) == "stale" {
		t.Errorf("front compaction left the stale usage cache: %q, err=%v", kept, err)
	}
	if kept, err := os.ReadFile(filepath.Join(sessionDir, "staging", "statecontext.json")); err != nil || string(kept) != "stale" {
		t.Errorf("front compaction changed the state context snapshot: %q, err=%v", kept, err)
	}
}

// Front compaction removes the response records from the transcript, but the
// session's spend is not undone: the totals stay and only the current usage
// resets to 0, as after a normal compaction.
func TestHTTPFrontCompactKeepsUsageTotalsAndResetsCurrent(t *testing.T) {
	service, sessionDir := newFrontCompactService(t, `{"enabled":true,"delete_mode":"permanent"}`)
	defer service.Close()
	api := httptest.NewServer(service.Handler)
	defer api.Close()

	first := `{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}`
	firstResponse := `{"type":"response.completed","session_turn":1,"session_step":1,"response":{"id":"usage-response-1","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7},"output":[]}}`
	second := `{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}`
	secondResponse := `{"type":"response.completed","session_turn":2,"session_step":1,"response":{"id":"usage-response-2","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7},"output":[]}}`
	messagesPath := filepath.Join(sessionDir, "messages.jsonl")
	body := first + "\n" + firstResponse + "\n" + second + "\n" + secondResponse + "\n"
	if err := os.WriteFile(messagesPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	boundary := len(first) + len(firstResponse) + 2
	response, err := http.DefaultClient.Do(frontCompactRequest(t, api.URL, integrationAPIToken, "session-user-o"+fmt.Sprint(boundary)))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("front-compact status=%d body=%s", response.StatusCode, responseBody)
	}
	current, err := os.ReadFile(messagesPath)
	if err != nil || string(current) != first+"\n"+firstResponse+"\n" {
		t.Fatalf("active transcript = %q err=%v", current, err)
	}

	raw, err := os.ReadFile(filepath.Join(sessionDir, "staging", "usage-cache.json"))
	if err != nil {
		t.Fatalf("read usage cache: %v", err)
	}
	var cache usageCacheFile
	if err := json.Unmarshal(raw, &cache); err != nil {
		t.Fatalf("decode usage cache: %v", err)
	}
	summary := cache.Summary
	if summary.CompletedResponses != 2 || summary.TurnCount != 2 || summary.InputTokens == nil || *summary.InputTokens != 8 || summary.OutputTokens == nil || *summary.OutputTokens != 6 {
		t.Fatalf("usage totals after front compaction = %+v, want both responses kept", summary)
	}
	latest := summary.Current
	if latest == nil || latest.InputTokens == nil || *latest.InputTokens != 0 || latest.OutputTokens == nil || *latest.OutputTokens != 0 || latest.TotalTokens == nil || *latest.TotalTokens != 0 {
		t.Fatalf("current usage after front compaction = %+v, want 0", latest)
	}
	if cache.Transcript.Offset != int64(len(current)) {
		t.Fatalf("usage cache offset = %d, want %d", cache.Transcript.Offset, len(current))
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
	spec := maintenanceSpec(map[string]any{
		"persistence": map[string]any{"durability": "process", "max_item_bytes": 1048576, "response_metadata": "full"},
	}, "")
	spec.RawSettings = map[string]json.RawMessage{"front_compaction": json.RawMessage(frontSetting)}
	config, _ := layoutConfig(t, spec, map[string]string{"AGENT_API_TOKEN": integrationAPIToken})
	location, err := sessionfs.Resolve(config.Roots, "local:one")
	if err != nil {
		t.Fatal(err)
	}
	sessionSettings, err := sessionfs.LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Dir(sessionSettings.MessagesPath)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
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
