package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gateAtRelease reports whether the mutation gate is still set when a maintenance action gives
// its process lock back, so the test can tell the order of the release.
type gateAtRelease struct {
	*fakeMaintenanceResolver
	handler *Handler
	waiting chan struct{}
	release chan struct{}
	held    atomic.Bool
}

func (r *gateAtRelease) LockSession(ctx context.Context, _, _, _ string) (func(), error) {
	select {
	case r.waiting <- struct{}{}:
	default:
	}
	select {
	case <-r.release:
		return func() {
			r.handler.mutationMu.Lock()
			_, held := r.handler.mutations["canonical\x00dev"]
			r.handler.mutationMu.Unlock()
			r.held.Store(held)
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *gateAtRelease) LockSessionSettings(context.Context, string) (func(), error) {
	return func() {}, nil
}

// A maintenance action that waits for the process lock must clear its mutation gate before it
// gives the lock back, as compaction does, so a start queued behind it does not find the gate set.
func TestMaintenanceClearsItsMutationGateBeforeReleasingTheProcessLock(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{"reset", "/api/sessions/session-1/reset?profile=dev", ""},
		{"front compact", "/api/sessions/session-1/front-compact?profile=dev", `{"message_id":"session-user-o0"}`},
		{"state context", "/api/sessions/session-1/statecontext?profile=dev", `{"prompt":"p"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := &fakeMaintenanceResolver{
				fakeResolver: &fakeResolver{resolved: ResolvedSession{Reference: "canonical", Profile: "dev"}},
				mutation:     &fakeMutation{},
			}
			resolver := &gateAtRelease{fakeMaintenanceResolver: base, waiting: make(chan struct{}, 1), release: make(chan struct{})}
			handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
			defer handler.Close()
			resolver.handler = handler

			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer "+testAPIToken)
			response := httptest.NewRecorder()
			finished := make(chan struct{})
			go func() {
				handler.ServeHTTP(response, request)
				close(finished)
			}()
			select {
			case <-resolver.waiting:
			case <-time.After(2 * time.Second):
				t.Fatal("the action did not wait for the process lock")
			}
			close(resolver.release)
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("the action never finished")
			}
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			if resolver.held.Load() {
				t.Fatal("the process lock was released while the mutation gate was still set")
			}
		})
	}
}
