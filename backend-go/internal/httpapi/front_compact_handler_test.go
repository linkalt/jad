package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/session"
)

func TestFrontCompactRoutePreservesRequestAndArchiveResponse(t *testing.T) {
	mutation := &fakeMutation{}
	resolver := &fakeMaintenanceResolver{
		fakeResolver: &fakeResolver{resolved: ResolvedSession{
			Reference: "canonical",
			Profile:   "dev",
			Runner: runnerFunc(func(_ context.Context, _ json.RawMessage, _ session.RunOptions) (session.RunResult, error) {
				return session.RunResult{}, nil
			}),
		}},
		mutation: mutation,
	}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()

	request := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/front-compact?profile=dev", strings.NewReader(`{"message_id":"session-user-o12"}`))
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("front-compact status = %d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		FrontCompaction FrontCompactionResponse `json:"front_compaction"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.FrontCompaction.DeleteMode != "recoverable" || body.FrontCompaction.ArchiveName != "messages-test.jsonl" {
		t.Fatalf("front-compact response = %+v", body.FrontCompaction)
	}
	if !mutation.frontCompacted || mutation.frontMessageID != "session-user-o12" {
		t.Fatalf("front-compact mutation: called=%t message_id=%q", mutation.frontCompacted, mutation.frontMessageID)
	}
	if len(resolver.calls) != 1 || resolver.calls[0] != [2]string{"session-1", "dev"} {
		t.Fatalf("maintenance resolution calls = %#v", resolver.calls)
	}
}

func TestFrontCompactRouteRejectsMalformedMessageIDBeforeResolve(t *testing.T) {
	resolver := &fakeMaintenanceResolver{
		fakeResolver: &fakeResolver{resolved: ResolvedSession{Reference: "canonical", Profile: "dev"}},
		mutation:     &fakeMutation{},
	}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()

	request := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/front-compact", strings.NewReader(`{"message_id":"session-user-o01"}`))
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("front-compact malformed ID status = %d body=%s", response.Code, response.Body.String())
	}
	if len(resolver.calls) != 0 {
		t.Fatalf("maintenance resolver called for malformed message ID: %#v", resolver.calls)
	}
}
