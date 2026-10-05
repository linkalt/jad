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

func TestRunStartRejectsProfileMutationInProgress(t *testing.T) {
	resolver := &fakeMaintenanceResolver{
		fakeResolver: &fakeResolver{resolved: ResolvedSession{
			Reference: "canonical",
			Profile:   "dev",
			Runner: runnerFunc(func(_ context.Context, _ json.RawMessage, _ session.RunOptions) (session.RunResult, error) {
				return session.RunResult{}, nil
			}),
		}},
		mutation: &fakeMutation{},
	}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()
	key := "canonical\x00dev"
	if !handler.beginMutation(key) {
		t.Fatal("could not register in-progress mutation")
	}
	defer handler.endMutation(key)

	request := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/run", strings.NewReader(`{"input":"hello"}`))
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("run start during mutation status = %d, body=%s", response.Code, response.Body.String())
	}
}
