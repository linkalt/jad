package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeCatalogSessions struct {
	*fakeResolver
	mu        sync.Mutex
	checked   []string
	deleted   []string
	cancelled []string
	checkErr  error
	// onCancel runs when the handler asks the session's children to stop.
	onCancel func()
}

func (f *fakeCatalogSessions) ListSessionConfigs(context.Context) ([]CatalogFileInfo, error) {
	return nil, nil
}
func (f *fakeCatalogSessions) ReadSessionConfig(context.Context, string) (CatalogFile, error) {
	return CatalogFile{}, nil
}
func (f *fakeCatalogSessions) WriteSessionConfig(context.Context, string, []byte, string, string) (CatalogFileInfo, bool, error) {
	return CatalogFileInfo{}, false, nil
}
func (f *fakeCatalogSessions) CheckSessionConfigDelete(_ context.Context, reference, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = append(f.checked, reference)
	if f.checkErr != nil {
		return "", f.checkErr
	}
	return "canonical", nil
}
func (f *fakeCatalogSessions) DeleteSessionConfig(_ context.Context, reference, _ string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, reference)
	return nil
}
func (f *fakeCatalogSessions) CancelAllSessionChildren(reference string) int {
	f.mu.Lock()
	f.cancelled = append(f.cancelled, reference)
	f.mu.Unlock()
	if f.onCancel != nil {
		f.onCancel()
	}
	return 0
}

func deleteSession(t *testing.T, handler *Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodDelete, "/api/catalog/sessions/demo"+query, nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	request.Header.Set("If-Match", `"v1"`)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func newCatalogSessionsHandler(t *testing.T, wait time.Duration) (*Handler, *fakeCatalogSessions) {
	t.Helper()
	resolver := &fakeCatalogSessions{fakeResolver: &fakeResolver{}}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute, SessionDeleteStopWait: wait})
	t.Cleanup(handler.Close)
	return handler, resolver
}

func TestSessionDeleteRefusedWhenARunDoesNotEndInTime(t *testing.T) {
	handler, resolver := newCatalogSessionsHandler(t, 120*time.Millisecond)
	run, err := handler.runs.begin("canonical\x00dev", "web-stuck")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	reply := deleteSession(t, handler, "")
	if reply.Code != http.StatusConflict {
		t.Fatalf("status = %d %s, want 409", reply.Code, reply.Body.String())
	}
	if waited := time.Since(started); waited < 120*time.Millisecond || waited > 3*time.Second {
		t.Fatalf("waited %v, want about 120ms", waited)
	}
	if run.ctx.Err() == nil {
		t.Fatal("the run was not asked to stop")
	}
	if len(resolver.deleted) != 0 {
		t.Fatalf("deleted although a run was active: %v", resolver.deleted)
	}
	if len(resolver.cancelled) != 1 || resolver.cancelled[0] != "canonical" {
		t.Fatalf("children cancelled for %v, want the canonical reference", resolver.cancelled)
	}
}

func TestSessionDeleteStopsRunsOfEveryProfileAndWaitsForThem(t *testing.T) {
	handler, resolver := newCatalogSessionsHandler(t, 2*time.Second)
	first, _ := handler.runs.begin("canonical\x00dev", "web-1")
	second, _ := handler.runs.begin("canonical\x00review", "web-2")
	other, _ := handler.runs.begin("other\x00dev", "web-3")
	go func() {
		<-first.ctx.Done()
		first.finish("aborted", nil)
		<-second.ctx.Done()
		time.Sleep(50 * time.Millisecond)
		second.finish("aborted", nil)
	}()
	reply := deleteSession(t, handler, "")
	if reply.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s, want 204", reply.Code, reply.Body.String())
	}
	if first.state() == "running" || second.state() == "running" {
		t.Fatalf("the delete returned while a run was going: %s %s", first.state(), second.state())
	}
	if other.ctx.Err() != nil {
		t.Fatal("a run of another session was stopped")
	}
	if len(resolver.deleted) != 1 {
		t.Fatalf("deleted = %v", resolver.deleted)
	}
}

func TestSessionDeleteChecksThePreconditionBeforeStoppingAnything(t *testing.T) {
	handler, resolver := newCatalogSessionsHandler(t, time.Second)
	resolver.checkErr = &StatusError{Code: http.StatusPreconditionFailed, Message: "changed"}
	run, _ := handler.runs.begin("canonical\x00dev", "web-1")
	if reply := deleteSession(t, handler, ""); reply.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", reply.Code)
	}
	if run.ctx.Err() != nil {
		t.Fatal("a refused delete stopped a run")
	}
	if len(resolver.cancelled) != 0 || len(resolver.deleted) != 0 {
		t.Fatalf("a refused delete acted: cancelled %v deleted %v", resolver.cancelled, resolver.deleted)
	}
}

func TestSessionDeleteRejectsAnUnknownStateValue(t *testing.T) {
	handler, resolver := newCatalogSessionsHandler(t, time.Second)
	if reply := deleteSession(t, handler, "?state=all"); reply.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", reply.Code)
	}
	if len(resolver.checked) != 0 {
		t.Fatal("the precondition was checked for a bad request")
	}
}

func TestNewHandlerRefusesAnOutOfRangeSessionDeleteStopWait(t *testing.T) {
	for _, wait := range []time.Duration{-time.Millisecond, 301 * time.Second} {
		config := Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute, SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 1024, SessionDeleteStopWait: wait}
		if _, err := NewHandler(config, &fakeResolver{}); err == nil {
			t.Errorf("a stop wait of %v was accepted", wait)
		}
	}
	config := Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute, SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 1024, SessionDeleteStopWait: 300 * time.Second}
	handler, err := NewHandler(config, &fakeResolver{})
	if err != nil {
		t.Fatalf("the maximum was refused: %v", err)
	}
	handler.Close()
}
