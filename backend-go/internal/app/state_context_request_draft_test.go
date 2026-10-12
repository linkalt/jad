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
	"sync"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/sessionfs/layouttest"
	"example.com/agent-harness/backend-go/internal/tools/native"
	"example.com/agent-harness/backend-go/internal/transcript"
)

func newRequestDraftStore(t *testing.T) *transcript.TurnStore {
	t.Helper()
	root := t.TempDir()
	journal, err := transcript.NewJournal(filepath.Join(root, "messages.jsonl"), transcript.DurabilityTurn)
	if err != nil {
		t.Fatal(err)
	}
	store, err := transcript.NewTurnStore(journal, filepath.Join(root, "staging"), transcript.TurnStoreOptions{
		MaxItemBytes: 1 << 20, AcceptedRequestPath: filepath.Join(root, "staging", "accepted-request.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func requestDraftStateContextConfig(maxBytes int64) sessionfs.SessionConfig {
	return sessionfs.SessionConfig{
		NativeRequestDraft: &sessionfs.NativeRequestDraftSettings{},
		StateContext:       sessionfs.StateContextSettings{Enabled: true, Trigger: []string{"compaction"}, Tools: []string{"request_draft"}, MaxBytes: maxBytes},
	}
}

func TestRequestDraftProviderCopiesTheAcceptedDraftIntoTheSnapshot(t *testing.T) {
	store := newRequestDraftStore(t)
	if _, err := store.WriteAcceptedRequest(json.RawMessage(`{"goal":"ship","note":"<keep>"}`), "timeout", "update", "call-7"); err != nil {
		t.Fatal(err)
	}
	snapshot, include, err := collectStateContextSnapshot(requestDraftStateContextConfig(4096), stateContextSources{requestDraft: store}, "compaction")
	if err != nil || !include {
		t.Fatalf("collect = include %v, error %v", include, err)
	}
	var document struct {
		Providers struct {
			RequestDraft struct {
				Items []struct {
					Source     string          `json:"source"`
					Action     string          `json:"action"`
					AcceptedAt string          `json:"accepted_at"`
					Draft      json.RawMessage `json:"draft"`
				} `json:"items"`
				HasMore bool `json:"has_more"`
			} `json:"request_draft"`
		} `json:"providers"`
	}
	body := strings.TrimPrefix(snapshot.Text, stateContextPreamble)
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		t.Fatalf("snapshot is not valid JSON after the preamble: %v\n%s", err, snapshot.Text)
	}
	items := document.Providers.RequestDraft.Items
	if len(items) != 1 || items[0].Source != "timeout" || items[0].Action != "update" || items[0].AcceptedAt == "" || document.Providers.RequestDraft.HasMore {
		t.Fatalf("request_draft section = %+v", document.Providers.RequestDraft)
	}
	var draft map[string]string
	if err := json.Unmarshal(items[0].Draft, &draft); err != nil || draft["goal"] != "ship" || draft["note"] != "<keep>" {
		t.Fatalf("draft = %s, err=%v", items[0].Draft, err)
	}
	if strings.Contains(snapshot.Text, "call-7") {
		t.Fatalf("the tool call id leaked into the snapshot: %s", snapshot.Text)
	}
}

func TestRequestDraftProviderIsEmptyWithoutAnAcceptedDraft(t *testing.T) {
	snapshot, include, err := collectStateContextSnapshot(requestDraftStateContextConfig(4096), stateContextSources{requestDraft: newRequestDraftStore(t)}, "compaction")
	if err != nil || !include {
		t.Fatalf("collect = include %v, error %v", include, err)
	}
	if !strings.Contains(snapshot.Text, `"items": []`) || !strings.Contains(snapshot.Text, `"has_more": false`) {
		t.Fatalf("empty request_draft section = %s", snapshot.Text)
	}
}

func TestRequestDraftProviderNeedsItsStoreAndItsToolConfig(t *testing.T) {
	if _, _, err := collectStateContextSnapshot(requestDraftStateContextConfig(4096), stateContextSources{}, "compaction"); err == nil || !strings.Contains(err.Error(), "request_draft statecontext store is not available") {
		t.Fatalf("missing store error = %v", err)
	}
	config := requestDraftStateContextConfig(4096)
	config.NativeRequestDraft = nil
	if _, _, err := collectStateContextSnapshot(config, stateContextSources{requestDraft: newRequestDraftStore(t)}, "compaction"); err == nil || !strings.Contains(err.Error(), "request_draft is not configured under tools.native") {
		t.Fatalf("unconfigured provider error = %v", err)
	}
}

func TestRequestDraftProviderIsDroppedWhenItDoesNotFitMaxBytes(t *testing.T) {
	store := newRequestDraftStore(t)
	if _, err := store.WriteAcceptedRequest(json.RawMessage(`{"payload":"`+strings.Repeat("x", 4_000)+`"}`), "user", "accept", "call-1"); err != nil {
		t.Fatal(err)
	}
	snapshot, include, err := collectStateContextSnapshot(requestDraftStateContextConfig(1024), stateContextSources{requestDraft: store}, "compaction")
	if err != nil || !include {
		t.Fatalf("collect = include %v, error %v", include, err)
	}
	if strings.Contains(snapshot.Text, strings.Repeat("x", 32)) || !strings.Contains(snapshot.Text, `"items": []`) || !strings.Contains(snapshot.Text, `"has_more": true`) {
		t.Fatalf("oversized draft was not dropped and flagged: %s", snapshot.Text)
	}
}

func processAndRequestDraftConfig(maxBytes int64) sessionfs.SessionConfig {
	config := processStateContextConfig(maxBytes, 10)
	config.StateContext.Tools = []string{"process", "request_draft"}
	config.NativeRequestDraft = &sessionfs.NativeRequestDraftSettings{}
	return config
}

func twoLongProcesses() *processStateSourceStub {
	longPath := "/workspace/" + strings.Repeat("x", 700)
	return &processStateSourceStub{snapshots: []native.ProcessSnapshot{
		processStateSnapshot("proc_1", native.ProcessRunning, "2026-10-03T00:00:00.000Z", longPath),
		processStateSnapshot("proc_2", native.ProcessRunning, "2026-10-02T00:00:00.000Z", longPath),
	}}
}

func TestRequestDraftProviderKeepsTheDraftWhileOtherProvidersGiveUpTheirItems(t *testing.T) {
	store := newRequestDraftStore(t)
	if _, err := store.WriteAcceptedRequest(json.RawMessage(`{"goal":"keep me"}`), "user", "accept", "call-1"); err != nil {
		t.Fatal(err)
	}
	config := processAndRequestDraftConfig(1024)
	config.StateContext.Tools = []string{"request_draft", "process"}
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{process: twoLongProcesses(), requestDraft: store}, "compaction")
	if err != nil || !include {
		t.Fatalf("collect = include %v, error %v", include, err)
	}
	if int64(len(snapshot.Text)) > config.StateContext.MaxBytes {
		t.Fatalf("snapshot is %d bytes, over %d", len(snapshot.Text), config.StateContext.MaxBytes)
	}
	if !strings.Contains(snapshot.Text, "keep me") {
		t.Fatalf("the draft was trimmed before the process items: %s", snapshot.Text)
	}
	if !strings.Contains(snapshot.Text, `"omitted_by_snapshot":`) {
		t.Fatalf("the process items were not trimmed: %s", snapshot.Text)
	}
}

func TestRequestDraftProviderIsDroppedOnlyAfterEveryOtherProviderIsEmpty(t *testing.T) {
	store := newRequestDraftStore(t)
	if _, err := store.WriteAcceptedRequest(json.RawMessage(`{"payload":"`+strings.Repeat("y", 4_000)+`"}`), "user", "accept", "call-1"); err != nil {
		t.Fatal(err)
	}
	config := processAndRequestDraftConfig(1024)
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{process: twoLongProcesses(), requestDraft: store}, "compaction")
	if err != nil || !include {
		t.Fatalf("collect = include %v, error %v", include, err)
	}
	if int64(len(snapshot.Text)) > config.StateContext.MaxBytes || strings.Contains(snapshot.Text, strings.Repeat("y", 32)) {
		t.Fatalf("the oversized draft was kept (%d bytes): %s", len(snapshot.Text), snapshot.Text)
	}
	if strings.Contains(snapshot.Text, "proc_1") || strings.Contains(snapshot.Text, "proc_2") {
		t.Fatalf("process items remain although the draft had to go too: %s", snapshot.Text)
	}
}

func TestHTTPCompactCopiesTheAcceptedDraftIntoTheStateContextOfEveryLaterRequest(t *testing.T) {
	manifest, err := filepath.Abs(filepath.Join("..", "..", "..", "sample", "tools", "native", "request_draft.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var withDraft []bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid Responses request", http.StatusBadRequest)
			return
		}
		found := false
		for _, item := range request.Input {
			text := string(item)
			if strings.Contains(text, "STATE CONTEXT SNAPSHOT") && strings.Contains(text, "ship the porting plan") {
				found = true
			}
		}
		mu.Lock()
		withDraft = append(withDraft, found)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"continued"}]}}`,
			`{"type":"response.completed","response":{"id":"resp-after","status":"completed","model":"gpt-test","output":[]}}`,
			"[DONE]",
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	defer provider.Close()

	config, fixture := layoutConfig(t, layouttest.Spec{
		SessionID:           "one",
		CWD:                 t.TempDir(),
		ToolDefinitionPaths: []string{manifest},
		Settings: map[string]any{
			"compaction":   map[string]any{"keepbatch": map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}}},
			"statecontext": map[string]any{"enabled": true, "trigger": []string{"compaction"}, "tools": []string{"request_draft"}, "role": "developer", "max_bytes": 4096},
		},
		Native: map[string]any{"request_draft": map[string]any{}},
		Models: map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}},
	}, map[string]string{"AGENT_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"})

	messages := strings.Join([]string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"old context"}]}`,
		`{"type":"response.completed","response":{"id":"resp_old","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]}]}}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"latest context"}]}`,
		`{"type":"response.completed","response":{"id":"resp_latest","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"latest answer"}]}]}}`,
	}, "\n") + "\n"
	messagesPath := filepath.Join(profileState(fixture), "messages.jsonl")
	if err := os.MkdirAll(filepath.Dir(messagesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(messagesPath, []byte(messages), 0o600); err != nil {
		t.Fatal(err)
	}
	acceptedPath := filepath.Join(profileState(fixture), "staging", "accepted-request.json")
	if err := os.MkdirAll(filepath.Dir(acceptedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	accepted := `{"schema_version":1,"version":3,"accepted_at":"2026-10-12T00:00:00.000Z","source":"user","action":"accept","call_id":"call-1","draft":{"goal":"ship the porting plan"}}` + "\n"
	if err := os.WriteFile(acceptedPath, []byte(accepted), 0o600); err != nil {
		t.Fatal(err)
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
	post := func(path, body string) (int, string) {
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
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(data)
	}

	if status, body := post("/api/sessions/local:one/run", `{"input":"before compaction"}`); status != http.StatusOK {
		t.Fatalf("first run status=%d body=%s", status, body)
	}
	if status, body := post("/api/sessions/local:one/compact", `{}`); status != http.StatusOK || !strings.Contains(body, `"compacted":true`) {
		t.Fatalf("compact status=%d body=%s", status, body)
	}
	for _, input := range []string{"after compaction", "and again"} {
		if status, body := post("/api/sessions/local:one/run", `{"input":"`+input+`"}`); status != http.StatusOK || !strings.Contains(body, "continued") {
			t.Fatalf("run %q status=%d body=%s", input, status, body)
		}
	}

	mu.Lock()
	got := append([]bool(nil), withDraft...)
	mu.Unlock()
	if len(got) != 3 || got[0] || !got[1] || !got[2] {
		t.Fatalf("provider requests carrying the draft = %v, want [false true true] (none before the first compaction, every request after it)", got)
	}
	transcriptBytes, err := os.ReadFile(messagesPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transcriptBytes), "ship the porting plan") {
		t.Fatalf("the draft was written into messages.jsonl: %s", transcriptBytes)
	}
	if kept, err := os.ReadFile(acceptedPath); err != nil || string(kept) != accepted {
		t.Fatalf("accepted-request.json changed: %q, err=%v", kept, err)
	}
}

func TestHTTPRunAutomaticCompactionCopiesTheAcceptedDraftIntoThePersistedSnapshot(t *testing.T) {
	manifest, err := filepath.Abs(filepath.Join("..", "..", "..", "sample", "tools", "native", "request_draft.json"))
	if err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after automatic compaction"}]}}`,
			`{"type":"response.completed","response":{"id":"resp-auto","status":"completed","model":"gpt-test","usage":{"input_tokens":100},"output":[]}}`,
			"[DONE]",
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	defer provider.Close()

	config, fixture := layoutConfig(t, layouttest.Spec{
		SessionID:           "one",
		CWD:                 t.TempDir(),
		ToolDefinitionPaths: []string{manifest},
		Settings: map[string]any{
			"compaction":   map[string]any{"auto_enabled": true, "trigger_tokens": 1, "keepbatch": map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}}},
			"statecontext": map[string]any{"enabled": true, "trigger": []string{"compaction"}, "tools": []string{"request_draft"}, "role": "developer", "max_bytes": 4096},
		},
		Native: map[string]any{"request_draft": map[string]any{}},
		Models: map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}},
	}, map[string]string{"AGENT_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"})
	messages := strings.Join([]string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"old context"}]}`,
		`{"type":"response.completed","response":{"id":"resp_old","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]}]}}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"latest context"}]}`,
		`{"type":"response.completed","response":{"id":"resp_latest","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"latest answer"}]}]}}`,
	}, "\n") + "\n"
	messagesPath := filepath.Join(profileState(fixture), "messages.jsonl")
	if err := os.MkdirAll(filepath.Dir(messagesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(messagesPath, []byte(messages), 0o600); err != nil {
		t.Fatal(err)
	}
	acceptedPath := filepath.Join(profileState(fixture), "staging", "accepted-request.json")
	if err := os.MkdirAll(filepath.Dir(acceptedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	accepted := `{"schema_version":1,"version":1,"accepted_at":"2026-10-12T00:00:00.000Z","source":"user","action":"accept","call_id":"call-1","draft":{"goal":"ship the porting plan"}}` + "\n"
	if err := os.WriteFile(acceptedPath, []byte(accepted), 0o600); err != nil {
		t.Fatal(err)
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
	request, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:one/run", strings.NewReader(`{"input":"continue"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "after automatic compaction") {
		t.Fatalf("run status=%d body=%s err=%v", response.StatusCode, body, err)
	}

	location, err := sessionfs.Resolve(config.Roots, "local:one")
	if err != nil {
		t.Fatal(err)
	}
	sessionConfig, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	snapshotBytes, err := os.ReadFile(sessionConfig.StoragePaths.StateContext)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot transcript.StateContextSnapshot
	if err := json.Unmarshal(snapshotBytes, &snapshot); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot.Text, "ship the porting plan") || !strings.Contains(snapshot.Text, `"request_draft"`) {
		t.Fatalf("automatic compaction snapshot lacks the accepted draft: %s", snapshot.Text)
	}
}
