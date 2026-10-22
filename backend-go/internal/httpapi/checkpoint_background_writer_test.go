package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type writerCountingResolver struct {
	*fakeMaintenanceResolver
	writers atomic.Int64
	asked   [][2]string
}

func (r *writerCountingResolver) BackgroundWriters(reference, profile string) int {
	r.asked = append(r.asked, [2]string{reference, profile})
	return int(r.writers.Load())
}

func restoreRequest(handler *Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/s/checkpoints/c1/restore?profile=dev", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestCheckpointRestoreIsRefusedWhileABackgroundWriterRuns(t *testing.T) {
	mutation := &checkpointMutation{fakeMutation: &fakeMutation{}}
	resolver := &writerCountingResolver{fakeMaintenanceResolver: &fakeMaintenanceResolver{
		fakeResolver: &fakeResolver{resolved: ResolvedSession{Reference: "canonical", Profile: "dev"}},
		mutation:     mutation,
	}}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})

	resolver.writers.Store(1)
	response := restoreRequest(handler, `{"mode":"safe"}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "background sub-agent that can change files is running") {
		t.Fatalf("restore with a writer running: status %d body %s", response.Code, response.Body.String())
	}
	if mutation.restored {
		t.Fatal("the restore ran under a background writer")
	}
	if len(resolver.asked) == 0 || resolver.asked[0] != [2]string{"canonical", "dev"} {
		t.Errorf("the writers were counted for %v, want the session canonical/dev", resolver.asked)
	}

	// A dry run changes nothing, so it is allowed while the writer runs.
	if response := restoreRequest(handler, `{"mode":"safe","dry_run":true}`); response.Code != http.StatusOK {
		t.Errorf("dry run with a writer running: status %d body %s", response.Code, response.Body.String())
	}

	resolver.writers.Store(0)
	mutation.restored = false
	if response := restoreRequest(handler, `{"mode":"safe"}`); response.Code != http.StatusOK || !mutation.restored {
		t.Fatalf("restore with no writer: status %d body %s restored %v", response.Code, response.Body.String(), mutation.restored)
	}
}
