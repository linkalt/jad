package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/session"
)

func TestIdleRunStreamAndResolverNotFound(t *testing.T) {
	resolver := &fakeResolver{resolved: ResolvedSession{
		Reference: "canonical-idle",
		Runner: runnerFunc(func(_ context.Context, _ json.RawMessage, _ session.RunOptions) (session.RunResult, error) {
			return session.RunResult{}, errors.New("unexpected run")
		}),
	}}
	handler := testHandler(t, resolver, Config{
		APIToken:            testAPIToken,
		MaxBodyBytes:        1024,
		SSEMaxBufferedBytes: 1024,
		RunStateRetention:   time.Minute,
	})
	defer handler.Close()

	request := httptest.NewRequest(http.MethodGet, "/api/sessions/idle/run/stream", nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("idle stream status = %d, body=%s", response.Code, response.Body.String())
	}
	events := parseEvents(t, response.Body.String())
	if got, want := eventNames(events), []string{"idle"}; !equalStrings(got, want) {
		t.Fatalf("idle events = %v, want %v", got, want)
	}

	resolver.mu.Lock()
	resolver.err = &StatusError{Code: http.StatusNotFound, Message: "session not found"}
	resolver.mu.Unlock()
	unknown := httptest.NewRequest(http.MethodGet, "/api/sessions/missing/run/stream", nil)
	unknown.Header.Set("Authorization", "Bearer "+testAPIToken)
	unknownResponse := httptest.NewRecorder()
	handler.ServeHTTP(unknownResponse, unknown)
	if unknownResponse.Code != http.StatusNotFound || !strings.Contains(unknownResponse.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unknown session response = %d %s", unknownResponse.Code, unknownResponse.Body.String())
	}
}
