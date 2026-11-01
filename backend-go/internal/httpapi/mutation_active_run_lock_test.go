package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// lockedWhileRunning behaves like the real resolver during a run: the
// transcript lock is held by the run, so LockSession waits until it is released
// (or the request is cancelled).
type lockedWhileRunning struct {
	*fakeMaintenanceResolver
	release chan struct{}
}

func (r *lockedWhileRunning) LockSession(ctx context.Context, _, _, _ string) (func(), error) {
	select {
	case <-r.release:
		return func() {}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *lockedWhileRunning) LockSessionSettings(context.Context, string) (func(), error) {
	return func() {}, nil
}

// A reset or a front compaction sent during a run is refused at once. Both used
// to wait for the transcript lock the run holds and then run after it, so the
// reset wiped the answer the run had just written.
func TestResetAndFrontCompactAreRefusedWhileARunHoldsTheTranscriptLock(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
		want string
	}{
		{"front compact", "/api/sessions/session-1/front-compact?profile=dev", `{"message_id":"session-user-o0"}`, "cannot front compact a profile while a run is active"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutation := &fakeMutation{}
			base := &fakeMaintenanceResolver{
				fakeResolver: &fakeResolver{resolved: ResolvedSession{Reference: "canonical", Profile: "dev"}},
				mutation:     mutation,
			}
			resolver := &lockedWhileRunning{fakeMaintenanceResolver: base, release: make(chan struct{})}
			defer close(resolver.release)
			handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
			defer handler.Close()
			if _, err := handler.runs.begin("canonical\x00dev", "web-active"); err != nil {
				t.Fatal(err)
			}

			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer "+testAPIToken)
			response := httptest.NewRecorder()
			finished := make(chan struct{})
			go func() {
				handler.ServeHTTP(response, request)
				close(finished)
			}()
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("the request waited for the transcript lock instead of being refused")
			}
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), tc.want) {
				t.Fatalf("status = %d body=%s, want 409 %q", response.Code, response.Body.String(), tc.want)
			}
			if mutation.reset || mutation.frontCompacted {
				t.Fatalf("the mutation ran during an active run: reset=%t front=%t", mutation.reset, mutation.frontCompacted)
			}
		})
	}
}
