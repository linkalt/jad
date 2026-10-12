package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/session"
)

type fakeMaintenanceResolver struct {
	*fakeResolver
	mutation SessionMutation
	calls    [][2]string
}

func (r *fakeMaintenanceResolver) ResolveMaintenance(_ context.Context, reference, profile string) (ResolvedMutation, error) {
	r.calls = append(r.calls, [2]string{reference, profile})
	return ResolvedMutation{Reference: r.resolved.Reference, Profile: r.resolved.Profile, Session: r.mutation}, nil
}

type fakeMutation struct {
	compacted      bool
	reset          bool
	frontCompacted bool
	frontMessageID string
	statePrompt    string
	statePreview   bool
}

func (m *fakeMutation) Compact(context.Context) (CompactionResponse, error) {
	m.compacted = true
	return CompactionResponse{Compacted: true, CompactedItems: 6, RetainedItems: 2, Usage: json.RawMessage(`{"completed_responses":1}`)}, nil
}

func (m *fakeMutation) Reset(context.Context) error {
	m.reset = true
	return nil
}

func (m *fakeMutation) FrontCompact(_ context.Context, messageID string) (FrontCompactionResponse, error) {
	m.frontCompacted = true
	m.frontMessageID = messageID
	return FrontCompactionResponse{DeleteMode: "recoverable", ArchiveName: "messages-test.jsonl"}, nil
}

func (m *fakeMutation) StateContext(_ context.Context, prompt string, preview bool) (StateContextResponse, error) {
	m.statePrompt = prompt
	m.statePreview = preview
	return StateContextResponse{Written: !preview, Preview: preview, Role: "developer", Text: "block:" + prompt, Bytes: len(prompt)}, nil
}

func TestCompactAndResetRoutesUseResolvedProfileAndFrontendPayloads(t *testing.T) {
	resolver := &fakeMaintenanceResolver{
		fakeResolver: &fakeResolver{resolved: ResolvedSession{Reference: "canonical", Profile: "dev", Runner: runnerFunc(func(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error) {
			return session.RunResult{}, nil
		})}},
		mutation: &fakeMutation{},
	}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()

	request := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/compact?profile=dev", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated compact status = %d, want 401", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/compact?profile=dev", nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("compact status = %d, body=%s", response.Code, response.Body.String())
	}
	var compact struct {
		Compaction struct {
			Compacted      bool `json:"compacted"`
			CompactedItems int  `json:"compacted_items"`
			RetainedItems  int  `json:"retained_items"`
		} `json:"compaction"`
		Usage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &compact); err != nil {
		t.Fatal(err)
	}
	if !compact.Compaction.Compacted || compact.Compaction.CompactedItems != 6 || compact.Compaction.RetainedItems != 2 || compact.Usage["completed_responses"] != 1 {
		t.Fatalf("unexpected compact response: %+v", compact)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/reset?profile=dev", nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "{\"reset\":true}\n" {
		t.Fatalf("reset response = %d %s", response.Code, response.Body.String())
	}
	mutation := resolver.mutation.(*fakeMutation)
	if !mutation.compacted || !mutation.reset {
		t.Fatalf("mutation calls: compact=%t reset=%t", mutation.compacted, mutation.reset)
	}
	if len(resolver.calls) != 2 || resolver.calls[0] != [2]string{"session-1", "dev"} || resolver.calls[1] != [2]string{"session-1", "dev"} {
		t.Fatalf("maintenance resolution calls = %#v", resolver.calls)
	}
}

func TestStateContextRoutePassesPromptAndPreviewAndIsRefusedDuringARun(t *testing.T) {
	mutation := &fakeMutation{}
	resolver := &fakeMaintenanceResolver{
		fakeResolver: &fakeResolver{resolved: ResolvedSession{Reference: "canonical", Profile: "dev"}},
		mutation:     mutation,
	}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()
	post := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/statecontext?profile=dev", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+testAPIToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	response := post(`{"prompt":"hello","preview":true}`)
	if response.Code != http.StatusOK || mutation.statePrompt != "hello" || !mutation.statePreview || !strings.Contains(response.Body.String(), `"statecontext"`) {
		t.Fatalf("preview = %d %s prompt=%q preview=%t", response.Code, response.Body.String(), mutation.statePrompt, mutation.statePreview)
	}
	if response := post(`{"prompt":1}`); response.Code != http.StatusBadRequest {
		t.Fatalf("non-string prompt status = %d, want 400", response.Code)
	}
	if response := post(`{"preview":"yes"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("non-boolean preview status = %d, want 400", response.Code)
	}
	if _, err := handler.runs.begin("canonical\x00dev", "web-active"); err != nil {
		t.Fatal(err)
	}
	if response := post(`{}`); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "while a run is active") {
		t.Fatalf("during a run = %d %s, want 409", response.Code, response.Body.String())
	}
}
