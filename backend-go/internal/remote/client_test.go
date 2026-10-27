package remote

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const secretToken = "tok_SECRET0123456789abcdef0123456789"

func environment(string) string { return secretToken }

// httpEntry is the entry of a test server: plain http on its loopback address.
func httpEntry(t *testing.T, server *httptest.Server) Entry {
	t.Helper()
	entry, err := ParseEntry("box", "/x/box.json", []byte(`{"kind":"remote","remote":{"url":"`+server.URL+`","token_env":"JAD_REMOTE_T","timeout_ms":2000,"max_response_bytes":1024,"retries":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func newClient(t *testing.T, entry Entry) *Client {
	t.Helper()
	client := NewClient(entry, Options{Environment: environment, RetryBackoff: time.Millisecond})
	t.Cleanup(client.Close)
	return client
}

func TestClientHTTP(t *testing.T) {
	var gotAuth, gotHops string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotHops = r.Header.Get("Authorization"), r.Header.Get(HeaderHops)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := newClient(t, httpEntry(t, server))
	var out struct{ OK bool }
	if err := client.Do(context.Background(), Request{Method: "GET", Path: "/api/remote/v1/profiles", Hops: 2}, &out); err != nil || !out.OK {
		t.Fatalf("call: %v %+v", err, out)
	}
	if gotAuth != "Bearer "+secretToken || gotHops != "2" {
		t.Fatalf("headers: %q %q", gotAuth, gotHops)
	}
}

func TestClientRefusesRedirectAndNeverForwardsToken(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("token forwarded to the redirect target")
		}
	}))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer server.Close()
	client := newClient(t, httpEntry(t, server))
	err := client.Do(context.Background(), Request{Method: "GET", Path: "/x", Retry: true}, nil)
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("want ErrRedirect, got %v", err)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the redirect target was contacted")
	}
}

func TestClientResponseCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"text":"` + strings.Repeat("x", 4096) + `"}`))
	}))
	defer server.Close()
	client := newClient(t, httpEntry(t, server))
	var out map[string]string
	err := client.Do(context.Background(), Request{Method: "GET", Path: "/x"}, &out)
	if !errors.Is(err, ErrResponseTooLarge) || out != nil {
		t.Fatalf("an over-cap answer must be an error and never a partial value: %v %v", err, out)
	}
}

func TestClientErrorAnswerIsBoundedAndHasNoToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"denied ` + secretToken + `\u0007\n` + strings.Repeat("é", 2000) + `","code":"forbidden"}`))
	}))
	defer server.Close()
	entry := httpEntry(t, server)
	entry.MaxResponseBytes = 64 << 10
	client := newClient(t, entry)
	err := client.Do(context.Background(), Request{Method: "GET", Path: "/x"}, nil)
	var remote *Error
	if !errors.As(err, &remote) || remote.Status != 403 || remote.Code != "forbidden" {
		t.Fatalf("error: %v", err)
	}
	if strings.Contains(err.Error(), secretToken) || len(remote.Message) > errorBodyBytes || strings.ContainsAny(remote.Message, "\x07\n") {
		t.Fatalf("message not cleaned: %q", remote.Message)
	}
}

func TestClientRetriesOnlyWhenAllowed(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := newClient(t, httpEntry(t, server))
	if err := client.Do(context.Background(), Request{Method: "POST", Path: "/x", Body: map[string]string{"a": "b"}}, nil); err == nil {
		t.Fatal("a request not marked Retry must not be repeated")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls %d", calls.Load())
	}
	calls.Store(0)
	if err := client.Do(context.Background(), Request{Method: "POST", Path: "/x", Body: map[string]string{"a": "b"}, Retry: true}, nil); err != nil || calls.Load() != 3 {
		t.Fatalf("retry: %v after %d calls", err, calls.Load())
	}
	calls.Store(-100)
	err := client.Do(context.Background(), Request{Method: "GET", Path: "/x", Retry: true}, nil)
	if err == nil {
		t.Fatal("expected failure after the retries are used")
	}
}

func TestClientDoesNotRetryAClientError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()
	client := newClient(t, httpEntry(t, server))
	_ = client.Do(context.Background(), Request{Method: "GET", Path: "/x", Retry: true}, nil)
	if calls.Load() != 1 {
		t.Fatalf("a 409 was retried %d times", calls.Load())
	}
}

func TestClientIgnoresProxyEnvironment(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxied.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer server.Close()
	client := newClient(t, httpEntry(t, server))
	if err := client.Do(context.Background(), Request{Method: "GET", Path: "/x"}, nil); err != nil {
		t.Fatal(err)
	}
	if proxied.Load() != 0 {
		t.Fatal("the proxy from the environment was used")
	}
	// Go skips proxies for loopback hosts, so the request above cannot show a regression.
	if client.transport.Proxy != nil {
		t.Fatal("the transport must not have a proxy function")
	}
}

func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "jr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestClientUnixSocket(t *testing.T) {
	socket := shortSocket(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	var gotAuth string
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})}
	go server.Serve(listener)
	defer server.Close()
	entry, err := ParseEntry("box", "/x/box.json", []byte(`{"kind":"remote","remote":{"url":"unix://`+socket+`","token_env":"JAD_REMOTE_T"}}`))
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(t, entry)
	var out struct{ OK bool }
	if err := client.Do(context.Background(), Request{Method: "GET", Path: "/x"}, &out); err != nil || !out.OK {
		t.Fatalf("call: %v", err)
	}
	if gotAuth != "Bearer "+secretToken {
		t.Fatal("a unix socket remote still needs the token")
	}
}

func TestClientUnixPathMustBeASocket(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := ParseEntry("box", "/x/box.json", []byte(`{"kind":"remote","remote":{"url":"unix://`+file+`","token_env":"JAD_REMOTE_T","retries":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(t, entry)
	err = client.Do(context.Background(), Request{Method: "GET", Path: "/x"}, nil)
	if err == nil || strings.Contains(err.Error(), secretToken) {
		t.Fatalf("a regular file must be refused: %v", err)
	}
	if missing := client.Do(context.Background(), Request{Method: "GET", Path: "/x"}, nil); missing == nil {
		t.Fatal("expected failure")
	}
}

func TestClientUnixSocketOwnerAndDirectoryAreChecked(t *testing.T) {
	serve := func(socket string) *atomic.Int32 {
		var calls atomic.Int32
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Skip("unix sockets unavailable:", err)
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = w.Write([]byte(`{"ok":true}`))
		})}
		go server.Serve(listener)
		t.Cleanup(func() { server.Close() })
		return &calls
	}
	call := func(socket string) error {
		entry, err := ParseEntry("box", "/x/box.json", []byte(`{"kind":"remote","remote":{"url":"unix://`+socket+`","token_env":"JAD_REMOTE_T","retries":0}}`))
		if err != nil {
			t.Fatal(err)
		}
		return newClient(t, entry).Do(context.Background(), Request{Method: "GET", Path: "/x"}, nil)
	}

	own := shortSocket(t)
	calls := serve(own)
	if err := call(own); err != nil || calls.Load() != 1 {
		t.Fatalf("a socket of this user in a private directory: %v", err)
	}

	original := processUID
	if original() == 0 {
		t.Skip("root owns every socket it makes, and root-owned sockets are trusted")
	}
	t.Cleanup(func() { processUID = original })
	processUID = func() int { return original() + 4242 }
	if err := call(own); err == nil || !strings.Contains(err.Error(), "owned by another user") || calls.Load() != 1 {
		t.Fatalf("a socket owned by someone else must not get the token: %v", err)
	}
	processUID = original

	open := shortSocket(t)
	openCalls := serve(open)
	if err := os.Chmod(filepath.Dir(open), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := call(open); err == nil || !strings.Contains(err.Error(), "other users can write") || openCalls.Load() != 0 {
		t.Fatalf("a world-writable directory without the sticky bit must be refused: %v", err)
	}
	if err := os.Chmod(filepath.Dir(open), 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := call(open); err != nil || openCalls.Load() != 1 {
		t.Fatalf("a sticky shared directory is fine: %v", err)
	}

	grouped := shortSocket(t)
	groupedCalls := serve(grouped)
	if err := os.Chmod(filepath.Dir(grouped), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := call(grouped); err == nil || !strings.Contains(err.Error(), "other users can write") || groupedCalls.Load() != 0 {
		t.Fatalf("a group-writable directory without the sticky bit must be refused: %v", err)
	}
}

func TestClientMaxInFlight(t *testing.T) {
	var running, peak atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := running.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		<-release
		running.Add(-1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	entry := httpEntry(t, server)
	entry.MaxInFlight = 2
	client := newClient(t, entry)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = client.Do(context.Background(), Request{Method: "GET", Path: "/x"}, nil) }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for running.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if peak.Load() > 2 {
		t.Fatalf("%d requests at once, cap is 2", peak.Load())
	}
	close(release)
	wg.Wait()
}

func TestClientCancelledContextEndsAtOnce(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	client := newClient(t, httpEntry(t, server))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Do(ctx, Request{Method: "GET", Path: "/x", Retry: true}, nil) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Do did not return after cancel")
	}
}

func TestClientNoGoroutineLeak(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	entry := httpEntry(t, server)
	before := runtime.NumGoroutine()
	client := NewClient(entry, Options{Environment: environment})
	for i := 0; i < 20; i++ {
		if err := client.Do(context.Background(), Request{Method: "GET", Path: "/x"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	client.Close()
	server.Close()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines: %d before, %d after", before, after)
	}
}

func TestClientErrorsNeverContainTheToken(t *testing.T) {
	entry := Entry{Name: "box", URL: "http://127.0.0.1:1", Host: "127.0.0.1:1", TokenEnv: "JAD_REMOTE_T", Timeout: 500, MaxResponseBytes: 1024, MaxInFlight: 1, Retries: 1}
	client := newClient(t, entry)
	err := client.Do(context.Background(), Request{Method: "GET", Path: "/x", Retry: true}, nil)
	if err == nil || strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "Bearer") {
		t.Fatalf("connect failure: %v", err)
	}
	if scrubbed := scrub(errors.New("boom "+secretToken), secretToken); strings.Contains(scrubbed.Error(), secretToken) {
		t.Fatal("scrub left the token")
	}
}

func TestClientMissingTokenFailsBeforeAnyRequest(t *testing.T) {
	var reached atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer server.Close()
	client := NewClient(httpEntry(t, server), Options{Environment: func(string) string { return "" }})
	defer client.Close()
	if err := client.Do(context.Background(), Request{Method: "GET", Path: "/x"}, nil); err == nil || !strings.Contains(err.Error(), "is not set") {
		t.Fatalf("missing token: %v", err)
	}
	if reached.Load() {
		t.Fatal("a request was sent without a token")
	}
}

func TestNetworkTextHidesTheRequestURL(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "http://host.invalid:1/api/remote/v1/transcript?path=secret", Err: io.EOF}
	text := networkText(err)
	if strings.Contains(text, "host.invalid") || strings.Contains(text, "path=secret") || !strings.Contains(text, "EOF") {
		t.Fatalf("%q", text)
	}
}
