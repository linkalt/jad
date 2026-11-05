package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// GET /api/server gives the client the fork name limit the server enforces, behind the API
// token; only GET is taken.
func TestServerInfoReportsTheForkNameLimit(t *testing.T) {
	handler, err := NewHandler(Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute, MaxPageTurns: 1, ShutdownTimeout: 50 * time.Millisecond, ForkNameMaxBytes: 42}, &fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	call := func(method string, token bool) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "/api/server", nil)
		if token {
			request.Header.Set("Authorization", "Bearer "+testAPIToken)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	if got := call(http.MethodGet, true); got.Code != http.StatusOK || got.Body.String() != "{\"fork_name_max_bytes\":42}\n" && got.Body.String() != `{"fork_name_max_bytes":42}` {
		t.Fatalf("GET = %d %q", got.Code, got.Body)
	}
	if got := call(http.MethodGet, false); got.Code != http.StatusUnauthorized {
		t.Fatalf("GET without the token = %d", got.Code)
	}
	if got := call(http.MethodPost, true); got.Code != http.StatusMethodNotAllowed || got.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST = %d allow %q", got.Code, got.Header().Get("Allow"))
	}
	if template := routeTemplate("/api/server"); template != "/api/server" {
		t.Fatalf("route template = %q", template)
	}
}
