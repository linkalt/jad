package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// A redirect to another origin is refused before the request is made, so the second
// origin is never contacted: httptest servers share the host 127.0.0.1 and differ only in
// the port, which makes this case the narrowest possible origin change.
func TestHeaderHTTPClientRefusesACrossOriginRedirect(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client := newHeaderHTTPClient(origin.URL, map[string]string{"Authorization": "Bearer secret"}, 1<<20)
	request, err := http.NewRequest(http.MethodPost, origin.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	request.Header.Set("Content-Type", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("the client followed a redirect to another origin")
	}
	if !strings.Contains(err.Error(), "refusing redirect to another origin") {
		t.Fatalf("error = %v; want the refusal named", err)
	}
	if count := reached.Load(); count != 0 {
		t.Fatalf("the second origin received %d requests; want none", count)
	}
}

// A redirect inside the configured origin is still followed, and the configured headers
// still arrive, since they are attached to the configured origin only.
func TestHeaderHTTPClientFollowsASameOriginRedirectWithHeaders(t *testing.T) {
	var authorization atomic.Value
	var finalHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
		case "/final":
			finalHits.Add(1)
			authorization.Store(r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newHeaderHTTPClient(server.URL, map[string]string{"Authorization": "Bearer secret"}, 1<<20)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/start", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	request.Header.Set("Content-Type", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("a same-origin redirect failed: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"result"`) {
		t.Fatalf("same-origin redirect gave status %d body %q", response.StatusCode, body)
	}
	if hits := finalHits.Load(); hits != 1 {
		t.Fatalf("the redirect target was reached %d times; want once", hits)
	}
	if value, _ := authorization.Load().(string); value != "Bearer secret" {
		t.Fatalf("Authorization at the redirect target = %q; want the configured header", value)
	}
}

// The same-origin redirect limit is unchanged.
func TestHeaderHTTPClientStopsAfterTenSameOriginRedirects(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/loop", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client := newHeaderHTTPClient(server.URL, nil, 1<<20)
	response, err := client.Get(server.URL + "/loop")
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("a redirect loop was followed without a limit")
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("error = %v; want the redirect limit named", err)
	}
}

// The manager's streamable_http connection goes through that client, so a configured
// server that redirects elsewhere fails to load its tools instead of reaching the target.
func TestManagerRefusesAStreamableHTTPCrossOriginRedirect(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"other","version":"1"}}}`)
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	manager := NewManager(nil)
	t.Cleanup(func() { _ = manager.Close() })
	loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{serverReference(t, "redirect", origin.URL, 10_000)}, t.TempDir())
	if err == nil {
		t.Fatalf("LoadTools() followed a cross-origin redirect and loaded %d tools", len(loaded))
	}
	if count := reached.Load(); count != 0 {
		t.Fatalf("the second origin received %d requests; want none", count)
	}
}
