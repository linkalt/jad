package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// ProfileInfo is one GET to the profile route with the session and the profile in the query. The
// client has no compaction or reset call, so none can reach the remote from here.
func TestClientProfileInfoIsOneReadOnlyGet(t *testing.T) {
	var method, path, query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query = r.Method, r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte(`{"output":"profile dev/coder | context tokens: 7"}`))
	}))
	defer server.Close()
	client := newClient(t, httpEntry(t, server))
	output, err := client.ProfileInfo(context.Background(), "main", "dev/coder", 1)
	if err != nil || output != "profile dev/coder | context tokens: 7" {
		t.Fatalf("info: %q, %v", output, err)
	}
	if method != http.MethodGet || path != "/api/remote/v1/profile" {
		t.Fatalf("the remote saw %s %s; want one GET to the profile route", method, path)
	}
	values, _ := url.ParseQuery(query)
	if values.Get("session") != "main" || values.Get("profile") != "dev/coder" || len(values) != 2 {
		t.Fatalf("query = %q; want session and profile only", query)
	}
}
