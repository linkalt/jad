package app

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"jad/backend-go/internal/transcript"
)

func (fixture *sessionStartFixture) statecontext(body string) (int, string) {
	fixture.t.Helper()
	request, err := http.NewRequest(http.MethodPost, fixture.api.URL+"/api/sessions/local:one/statecontext", strings.NewReader(body))
	if err != nil {
		fixture.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		fixture.t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		fixture.t.Fatal(err)
	}
	return response.StatusCode, string(data)
}

func userTrigger(extra map[string]any) map[string]any {
	trigger := map[string]any{"tools": []string{"process"}, "max_bytes": 4096, "max_prompt_bytes": 256}
	for key, value := range extra {
		trigger[key] = value
	}
	return map[string]any{"user": trigger}
}

func TestHTTPStateContextUserTriggerWritesABlockWithTheTypedPrompt(t *testing.T) {
	fixture := newStateContextFixture(t, true, userTrigger(map[string]any{"max_prompt_bytes": 64}))

	status, body := fixture.statecontext(`{"prompt":"  focus on the build  ","preview":true}`)
	if status != http.StatusOK || !strings.Contains(body, `"preview":true`) || !strings.Contains(body, "focus on the build") || !strings.Contains(body, "proc_saved") {
		t.Fatalf("preview = %d %s", status, body)
	}
	if _, err := os.Stat(fixture.snapshotPath()); !os.IsNotExist(err) {
		t.Fatalf("a preview wrote the state context file: %v", err)
	}

	status, body = fixture.statecontext(`{"prompt":"focus on the build"}`)
	if status != http.StatusOK || !strings.Contains(body, `"written":true`) {
		t.Fatalf("write = %d %s", status, body)
	}
	data, err := os.ReadFile(fixture.snapshotPath())
	if err != nil {
		t.Fatal(err)
	}
	var entry transcript.StateContextEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatal(err)
	}
	header, prompt, state := strings.Index(entry.Text, "STATE CONTEXT SNAPSHOT"), strings.Index(entry.Text, "focus on the build"), strings.Index(entry.Text, "proc_saved")
	if entry.Trigger != "user" || entry.Role != "developer" || header != 0 || prompt < header || state < prompt {
		t.Fatalf("entry = %#v; want trigger user, the header, then the typed prompt, then the data", entry)
	}

	fixture.run("after")
	if len(fixture.requests) != 1 || !fixture.requests[0] {
		t.Fatalf("requests carrying the user block = %v; want the next run to carry it", fixture.requests)
	}
}

func TestHTTPStateContextUserTriggerRefusals(t *testing.T) {
	notConfigured := newSessionStartFixture(t, true)
	if status, body := notConfigured.statecontext(`{"prompt":"x"}`); status != http.StatusForbidden || !strings.Contains(body, "not configured") {
		t.Fatalf("unconfigured user trigger = %d %s; want 403", status, body)
	}

	fixture := newStateContextFixture(t, true, userTrigger(map[string]any{"max_prompt_bytes": 8}))
	if status, body := fixture.statecontext(`{"prompt":"123456789"}`); status != http.StatusBadRequest || !strings.Contains(body, "max_prompt_bytes") {
		t.Fatalf("oversized prompt = %d %s; want 400 naming max_prompt_bytes", status, body)
	}
	if status, _ := fixture.statecontext(`{"prompt":5}`); status != http.StatusBadRequest {
		t.Fatalf("non-string prompt = %d; want 400", status)
	}
	if _, err := os.Stat(fixture.snapshotPath()); !os.IsNotExist(err) {
		t.Fatalf("a refused request wrote the state context file: %v", err)
	}
}

func TestHTTPStateContextUserTriggerWritesTheTypedPromptWhenStoresAreEmpty(t *testing.T) {
	fixture := newStateContextFixture(t, false, userTrigger(nil))
	if status, body := fixture.statecontext(`{}`); status != http.StatusOK || !strings.Contains(body, `"written":false`) {
		t.Fatalf("no prompt and no data = %d %s; want nothing written", status, body)
	}
	if status, body := fixture.statecontext(`{"prompt":"remember the deadline"}`); status != http.StatusOK || !strings.Contains(body, `"written":true`) {
		t.Fatalf("prompt with empty stores = %d %s; want a block", status, body)
	}
}
