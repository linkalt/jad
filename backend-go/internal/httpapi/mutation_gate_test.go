package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResetRejectsAnActiveRunForResolvedProfile(t *testing.T) {
	mutation := &fakeMutation{}
	resolver := &fakeMaintenanceResolver{
		fakeResolver: &fakeResolver{resolved: ResolvedSession{Reference: "canonical", Profile: "dev"}},
		mutation:     mutation,
	}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()
	if _, err := handler.runs.begin("canonical\x00dev", "web-active"); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/reset?profile=dev", nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("reset during active run status = %d, body=%s", response.Code, response.Body.String())
	}
	if mutation.reset {
		t.Fatal("reset mutation ran while the profile had an active run")
	}
}
