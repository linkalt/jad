package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
)

const remoteTestSecret = "remote_SECRET_0123456789abcdef0123456789"

func remoteHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

type remoteServer struct {
	f        subagentFixture
	service  *Service
	url      string
	table    string
	handlers RemoteHandlers
	server   *httptest.Server
	gate     *outageGate
}

// outageGate lets a test cut every connection to the server, as a network outage does.
type outageGate struct {
	next http.Handler
	cut  atomic.Bool
}

func (g *outageGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.cut.Load() {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
			return
		}
	}
	g.next.ServeHTTP(w, r)
}

// startRemoteServer turns the sub-agent fixture's config into a remote-API server: the same
// sessions, profiles and provider as the fixture, plus a token table.
func startRemoteServer(t *testing.T, f subagentFixture, tokens string) *remoteServer {
	t.Helper()
	table := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(table, []byte(tokens), 0o600); err != nil {
		t.Fatal(err)
	}
	config := f.config
	config.RemoteAPI = sessionfs.RemoteAPISettings{Enabled: true, TokensFile: table, MaxHops: 3, MaxRuns: 16, RunTimeoutMs: 60_000, RunRetentionMs: 60_000, WaitMaxMs: 5_000, BodyMaxBytes: 1 << 20, ResultMaxBytes: 64 << 10}
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	handlers := service.HandlersFor(config)
	gate := &outageGate{next: handlers.Main}
	server := httptest.NewServer(gate)
	t.Cleanup(server.Close)
	return &remoteServer{f: f, service: service, url: server.URL, table: table, handlers: handlers, server: server, gate: gate}
}

func tokenTable(rows ...string) string { return `{"tokens":[` + strings.Join(rows, ",") + `]}` }

func tokenRow(id, secret, extra string) string {
	return fmt.Sprintf(`{"id":%q,"sha256":%q,%s}`, id, remoteHash(secret), extra)
}

func (s *remoteServer) call(t *testing.T, method, path, secret string, body any, headers ...string) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	request, _ := http.NewRequest(method, s.url+path, reader)
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := s.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(response.Body).Decode(&out)
	return response.StatusCode, out
}

func (s *remoteServer) waitDone(t *testing.T, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		status, body := s.call(t, "GET", remoteapi.Prefix+"/runs/"+id+"?wait_ms=2000", remoteTestSecret, nil)
		if status != 200 {
			t.Fatalf("run status %d %v", status, body)
		}
		if body["state"] != "running" {
			return body
		}
	}
	t.Fatal("remote run did not end")
	return nil
}

const fullGrant = `"profiles":["*/*/*"],"agents":["*/*/*/*"],"actions":["message","subagent","status","stop","transcripts"]`

func TestRemoteAPIMessageRunsTheTargetProfileAndReturnsItsAnswer(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))

	status, body := server.call(t, "GET", remoteapi.Prefix+"/profiles?session=demo", remoteTestSecret, nil)
	profiles, _ := body["profiles"].([]any)
	if status != 200 || len(profiles) == 0 {
		t.Fatalf("profiles: %d %v", status, body)
	}
	status, body = server.call(t, "POST", remoteapi.Prefix+"/messages", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "message": "hello over the wire", "request_id": "req-msg-00001"})
	if status != 202 {
		t.Fatalf("start: %d %v", status, body)
	}
	done := server.waitDone(t, body["id"].(string))
	if done["state"] != "done" || done["text"] == "" {
		t.Fatalf("outcome: %v", done)
	}
	parent := f.provider.forModel("parent-model")
	if len(parent) == 0 || !strings.Contains(parent[0].Text, "hello over the wire") {
		t.Fatal("the target profile never saw the message")
	}
	// A repeat of the same request id does not send the message again.
	before := len(f.provider.forModel("parent-model"))
	status, again := server.call(t, "POST", remoteapi.Prefix+"/messages", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "message": "hello over the wire", "request_id": "req-msg-00001"})
	if status != 200 || again["id"] != body["id"] || len(f.provider.forModel("parent-model")) != before {
		t.Fatalf("repeat: %d %v (provider calls %d -> %d)", status, again, before, len(f.provider.forModel("parent-model")))
	}
}

func TestRemoteAPIRefusalsNeverReachTheProfile(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	server := startRemoteServer(t, f, tokenTable(
		tokenRow("narrow", remoteTestSecret, `"profiles":["demo/other/*"],"actions":["message","status"]`),
		tokenRow("gone", "revoked_0123456789abcdef0123456789abcd", `"revoked":true,`+fullGrant)))
	for name, c := range map[string]struct {
		secret string
		body   map[string]any
		want   int
	}{
		"profile outside the token's globs": {remoteTestSecret, map[string]any{"session": "demo", "profile": "dev/coder", "message": "x", "request_id": "req-ref-00001"}, 403},
		"revoked token":                     {"revoked_0123456789abcdef0123456789abcd", map[string]any{"session": "demo", "profile": "dev/coder", "message": "x", "request_id": "req-ref-00002"}, 401},
		"no token":                          {"", map[string]any{"session": "demo", "profile": "dev/coder", "message": "x", "request_id": "req-ref-00003"}, 401},
	} {
		if status, body := server.call(t, "POST", remoteapi.Prefix+"/messages", c.secret, c.body); status != c.want {
			t.Fatalf("%s: %d %v", name, status, body)
		}
	}
	if len(f.provider.forModel("parent-model")) != 0 {
		t.Fatal("a refused request ran the profile")
	}
	// The UI token does not open the remote routes, and a remote token does not open the UI routes.
	if status, _ := server.call(t, "GET", remoteapi.Prefix+"/profiles?session=demo", integrationAPIToken, nil); status != 401 {
		t.Fatalf("UI token on the remote API: %d", status)
	}
	if status, _ := server.call(t, "GET", "/api/sessions", remoteTestSecret, nil); status != 401 {
		t.Fatalf("remote token on the UI API: %d", status)
	}
}

func TestRemoteAPIHopLimitStopsBeforeAnyWork(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))
	status, body := server.call(t, "POST", remoteapi.Prefix+"/messages", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "message": "x", "request_id": "req-hop-00001"}, remoteapi.HeaderHops, "4")
	if status != http.StatusLoopDetected || body["code"] != "remote_hop_limit" || len(f.provider.forModel("parent-model")) != 0 {
		t.Fatalf("%d %v", status, body)
	}
}

func TestRemoteAPIUnknownSessionAndProfileAreNotCreated(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))
	status, body := server.call(t, "POST", remoteapi.Prefix+"/messages", remoteTestSecret, map[string]any{
		"session": "ghost", "profile": "dev/coder", "message": "x", "request_id": "req-gone-0001"})
	if status != 404 || body["code"] != "no_session" {
		t.Fatalf("unknown session: %d %v", status, body)
	}
	status, body = server.call(t, "POST", remoteapi.Prefix+"/messages", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/ghost", "message": "x", "request_id": "req-gone-0002"})
	if status != 404 || body["code"] != "unknown_target" {
		t.Fatalf("unknown profile: %d %v", status, body)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, ".jad", "state", "ghost")); err == nil {
		t.Fatal("a state folder was created for a session that does not exist")
	}
}

func TestRemoteAPISubagentRunsWaitsAndAnswers(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))
	status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "find the answer", "request_id": "req-sub-00001"})
	if status != 202 {
		t.Fatalf("start: %d %v", status, body)
	}
	done := server.waitDone(t, body["id"].(string))
	if done["state"] != "done" || !strings.Contains(fmt.Sprint(done["text"]), "the answer is 42") {
		t.Fatalf("outcome: %v", done)
	}
	if status, body = server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "nobody", "task": "x", "request_id": "req-sub-00002"}); status != 404 || body["code"] != "unknown_target" {
		t.Fatalf("unknown agent: %d %v", status, body)
	}
}

func TestRemoteAPISubagentCanBeStopped(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	f.provider.childBlocks = true
	f.provider.childEntered = make(chan struct{}, 4)
	f.provider.childRelease = make(chan struct{})
	t.Cleanup(func() { close(f.provider.childRelease) })
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))
	status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "find the answer", "request_id": "req-stp-00001"})
	if status != 202 {
		t.Fatalf("start: %d %v", status, body)
	}
	id := body["id"].(string)
	select {
	case <-f.provider.childEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the child never reached the provider")
	}
	if status, body = server.call(t, "GET", remoteapi.Prefix+"/runs/"+id, remoteTestSecret, nil); status != 200 || body["state"] != "running" {
		t.Fatalf("running: %d %v", status, body)
	}
	if status, body = server.call(t, "POST", remoteapi.Prefix+"/runs/"+id+"/stop", remoteTestSecret, nil); status != 200 {
		t.Fatalf("stop: %d %v", status, body)
	}
	if done := server.waitDone(t, id); done["state"] != "stopped" {
		t.Fatalf("after stop: %v", done)
	}
}

func TestUnixSocketListenerIsPrivateAndReplacesAStaleSocketOnly(t *testing.T) {
	dir, err := os.MkdirTemp("", "jad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "r.sock")
	listener, err := ListenUnix(path, 0o600)
	if err != nil {
		t.Skip("unix sockets unavailable: ", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode: %v %v", info, err)
	}
	if _, err := ListenUnix(path, 0o600); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("a live socket must not be taken over: %v", err)
	}
	listener.Close()
	// Closing a Go unix listener removes the file; recreate a stale one by hand.
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	again, err := ListenUnix(path, 0o660)
	if err != nil {
		t.Fatalf("a stale socket file must be replaced: %v", err)
	}
	defer again.Close()
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o660 {
		t.Fatalf("mode %v", info.Mode())
	}
	again.Close()
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenUnix(path, 0o600); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("a regular file must never be removed: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "not a socket" {
		t.Fatal("the file was changed")
	}
}

// The remote routes answer on the plain listener: that is how a second JAD reaches this one.
func TestPlainListenerServesTheRemoteAPI(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))
	plain := httptest.NewServer(server.handlers.Main)
	t.Cleanup(plain.Close)
	call := func(token string) int {
		request, _ := http.NewRequest("GET", plain.URL+remoteapi.Prefix+"/profiles?session=demo", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if status := call(remoteTestSecret); status != 200 {
		t.Fatalf("a remote token over the plain listener: %d", status)
	}
	if status := call(integrationAPIToken); status != 401 {
		t.Fatalf("the UI token must not open the remote routes: %d", status)
	}
}
