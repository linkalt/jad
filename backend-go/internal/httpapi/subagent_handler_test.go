package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type subagentResolver struct {
	*fakeResolver
	reference, profile, callID string
	body                       json.RawMessage
	err                        error
}

func (r *subagentResolver) ReadSubagentSnapshot(_ context.Context, reference, profile, callID string) (json.RawMessage, error) {
	r.reference, r.profile, r.callID = reference, profile, callID
	return r.body, r.err
}

func subagentRequest(handler *Handler, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func subagentTestConfig() Config {
	return Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute}
}

// TestSubagentRouteServesTheStepsOfOneCallAndMapsErrors: GET /api/sessions/{ref}/subagents/{callId}
// passes the session, profile and decoded call id on, is never cached, needs the token, and a
// call id with no child is the resolver's 404.
func TestSubagentRouteServesTheStepsOfOneCallAndMapsErrors(t *testing.T) {
	resolver := &subagentResolver{fakeResolver: &fakeResolver{}, body: json.RawMessage(`{"subagent":{"steps":[]}}`)}
	handler := testHandler(t, resolver, subagentTestConfig())
	defer handler.Close()

	response := subagentRequest(handler, "/api/sessions/root-a%3Asession-1/subagents/toolu%2001?profile=jad/test")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"subagent":{"steps":[]}}` {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if resolver.reference != "root-a:session-1" || resolver.profile != "jad/test" || resolver.callID != "toolu 01" {
		t.Fatalf("selection = %q %q %q", resolver.reference, resolver.profile, resolver.callID)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, a running child's steps must not be cached", got)
	}

	resolver.err = &StatusError{Code: http.StatusNotFound, Message: "subagent transcript not found"}
	if response := subagentRequest(handler, "/api/sessions/s/subagents/unknown"); response.Code != http.StatusNotFound || !json.Valid(response.Body.Bytes()) {
		t.Fatalf("unknown call id = %d %s", response.Code, response.Body.String())
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/sessions/s/subagents/c", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", unauthorized.Code)
	}
	post := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/s/subagents/c", nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	handler.ServeHTTP(post, request)
	if post.Code != http.StatusNotFound {
		t.Fatalf("POST = %d, want the route to be GET only", post.Code)
	}
}

func TestSubagentRouteIsUnavailableWithoutAReader(t *testing.T) {
	handler := testHandler(t, &fakeResolver{}, subagentTestConfig())
	defer handler.Close()
	if response := subagentRequest(handler, "/api/sessions/s/subagents/c"); response.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d", response.Code)
	}
}
