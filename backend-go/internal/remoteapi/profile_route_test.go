package remoteapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// fakeProfileBackend answers the profile info route and records what it was asked.
type fakeProfileBackend struct {
	*fakeBackend
	asked []ProfileRequest
}

func (b *fakeProfileBackend) ProfileInfo(_ context.Context, request ProfileRequest) (string, error) {
	b.asked = append(b.asked, request)
	return "profile " + request.Profile + " | context tokens: no usage yet", nil
}

// profileTokenRows is a token table with one row that holds the given actions and profile globs.
func profileTokenRows(actions, profiles string) string {
	return fmt.Sprintf(`{"tokens":[{"id":"t","sha256":%q,"profiles":[%s],"actions":[%s]}]}`, hashOf(secretA), profiles, actions)
}

func profileRouteFixture(t *testing.T, rows string) (*fixture, *fakeProfileBackend) {
	t.Helper()
	backend := &fakeProfileBackend{fakeBackend: &fakeBackend{profiles: map[string][]string{"main": {"review/a", "dev/coder"}}}}
	return newFixture(t, tokens(t, rows), func(opts *Options) { opts.Backend = backend }), backend
}

// A token with the profile action reaches the info route, and the backend is asked for the profile.
func TestProfileInfoRouteAnswersForAGrantedToken(t *testing.T) {
	f, backend := profileRouteFixture(t, profileTokenRows(`"profile"`, `"main/review/*"`))
	status, body := f.do(http.MethodGet, Prefix+"/profile?session=main&profile=review/a", secretA, nil)
	if status != http.StatusOK || !strings.Contains(fmt.Sprint(body["output"]), "context tokens") {
		t.Fatalf("info: status %d body %v", status, body)
	}
	if len(backend.asked) != 1 || backend.asked[0].Session != "main" || backend.asked[0].Profile != "review/a" {
		t.Fatalf("backend asked = %+v", backend.asked)
	}
}

// A token without the profile action is refused before the backend is asked.
func TestProfileInfoRouteNeedsTheProfileAction(t *testing.T) {
	f, backend := profileRouteFixture(t, profileTokenRows(`"message","transcripts"`, `"*/*/*"`))
	if status, _ := f.do(http.MethodGet, Prefix+"/profile?session=main&profile=review/a", secretA, nil); status != http.StatusForbidden {
		t.Fatalf("no profile action: status %d, want 403", status)
	}
	if len(backend.asked) != 0 {
		t.Fatalf("a refused token reached the backend: %+v", backend.asked)
	}
}

// Only session and profile are read: a remote cannot pass compact, reset or any other key.
func TestProfileInfoRouteRefusesAnyOtherKey(t *testing.T) {
	f, backend := profileRouteFixture(t, profileTokenRows(`"profile"`, `"*/*/*"`))
	for _, query := range []string{"session=main&profile=review/a&action=reset", "session=main&profile=review/a&action=compact"} {
		if status, _ := f.do(http.MethodGet, Prefix+"/profile?"+query, secretA, nil); status != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", query, status)
		}
	}
	if len(backend.asked) != 0 {
		t.Fatalf("a refused key reached the backend: %+v", backend.asked)
	}
}

// Only GET is a route: a remote cannot send a compaction or a reset through the profile route.
func TestProfileRouteIsReadOnly(t *testing.T) {
	f, backend := profileRouteFixture(t, profileTokenRows(`"profile"`, `"*/*/*"`))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if status, _ := f.do(method, Prefix+"/profile?session=main&profile=review/a", secretA, map[string]any{"action": "reset"}); status != http.StatusNotFound {
			t.Fatalf("%s /profile: status %d, want 404", method, status)
		}
	}
	if len(backend.asked) != 0 {
		t.Fatalf("a non-GET reached the backend: %+v", backend.asked)
	}
}
