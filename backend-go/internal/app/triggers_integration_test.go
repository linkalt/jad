package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// A signed webhook starts a real run on the session: the prompt with the payload reaches the
// model with the trigger's profile and model, and GET /api/triggers reports the run.
func TestTriggerWebhookRunsTheSession(t *testing.T) {
	f := newLayoutFixtureWithServer(t, map[string]any{"triggers": map[string]any{"enabled": true, "entries": map[string]any{
		"ci": map[string]any{"session": "local:demo", "profile": "dev/coder", "model": "openai/large", "prompt": "CI says {payload}", "webhook": map[string]any{"secret_env": "FIXTURE_KEY"}},
	}}})
	f.mu.Lock()
	f.captureBodies = true
	f.mu.Unlock()
	body := `{"job":"unit","conclusion":"failure"}`
	mac := hmac.New(sha256.New, []byte("k"))
	mac.Write([]byte(body))
	request, _ := http.NewRequest(http.MethodPost, f.api.URL+"/api/triggers/ci", strings.NewReader(body))
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook: %d %s", response.StatusCode, reply)
	}
	var listed struct {
		Runs []struct {
			Status, TurnID, Error string
		} `json:"runs"`
	}
	for deadline := time.Now().Add(10 * time.Second); len(listed.Runs) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the trigger run did not end")
		}
		status, text := f.do(t, http.MethodGet, "/api/triggers", "")
		if status != http.StatusOK {
			t.Fatalf("list: %d %s", status, text)
		}
		if err := json.Unmarshal([]byte(text), &listed); err != nil {
			t.Fatal(err)
		}
	}
	if run := listed.Runs[0]; run.Status != "completed" || run.Error != "" {
		t.Fatalf("run = %+v", run)
	}
	if got := f.lastModel(); got != "model-large" {
		t.Fatalf("model = %q", got)
	}
	f.mu.Lock()
	sent := f.bodies[len(f.bodies)-1]
	f.mu.Unlock()
	if !strings.Contains(sent, `CI says {\"job\":\"unit\",\"conclusion\":\"failure\"}`) {
		t.Fatalf("the provider did not get the prompt with the payload: %s", sent)
	}
	// Without a signature nothing runs; the rest of the API still needs its token.
	unsigned, _ := http.Post(f.api.URL+"/api/triggers/ci", "application/json", strings.NewReader(body))
	unsigned.Body.Close()
	listAnonymous, _ := http.Get(f.api.URL + "/api/triggers")
	listAnonymous.Body.Close()
	if unsigned.StatusCode != http.StatusUnauthorized || listAnonymous.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned %d, anonymous list %d", unsigned.StatusCode, listAnonymous.StatusCode)
	}
}

func TestTriggerWebhookWithoutItsSecretStopsTheServerStart(t *testing.T) {
	f := newLayoutFixture(t)
	path := filepath.Join(f.global, "global.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	root["server"] = map[string]any{"triggers": map[string]any{"enabled": true, "entries": map[string]any{
		"ci": map[string]any{"session": "local:demo", "prompt": "p", "webhook": map[string]any{"secret_env": "JAD_NO_SUCH_SECRET"}},
	}}}
	data, _ = json.Marshal(root)
	f.overwrite(t, "global.json", string(data))
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: f.cwd, GlobalDir: f.global, Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken}})
	if err != nil {
		t.Fatal(err)
	}
	if service, err := NewService(config); err == nil || !strings.Contains(err.Error(), "JAD_NO_SUCH_SECRET is not set or empty") {
		service.Close()
		t.Fatalf("err = %v", err)
	}
	// jad run and jad eval never run triggers, so the missing secret does not stop them.
	service, _, err := NewHeadless(config)
	if err != nil {
		t.Fatalf("headless: %v", err)
	}
	service.Close()
}
