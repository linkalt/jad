package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// remoteWorkspaces is a remote-API server of the workspace layout with two workspaces, a and b,
// that both hold the session s1 (b also holds only-b), each with the profile dev/base, behind a
// fake provider that answers "ok". Which workspace a message reached is read from the
// transcript of that workspace's profile.
type remoteWorkspaces struct {
	*remoteServer
	a, b string
	mu   sync.Mutex
	runs int
}

func (w *remoteWorkspaces) transcript(workspace, session string) string {
	data, _ := os.ReadFile(filepath.Join(workspace, ".jad", "state", session, "dev", "base", "messages.jsonl"))
	return string(data)
}

func (w *remoteWorkspaces) providerRuns() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runs
}

func startRemoteWorkspaces(t *testing.T, tokens string) *remoteWorkspaces {
	t.Helper()
	return startRemoteWorkspacesWith(t, tokens, nil)
}

// startRemoteWorkspacesWith is startRemoteWorkspaces with the part a test changes: loop, when
// set, makes the server call itself (see remote_loop_test.go).
func startRemoteWorkspacesWith(t *testing.T, tokens string, loop *remoteLoop) *remoteWorkspaces {
	t.Helper()
	w := &remoteWorkspaces{}
	provider := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		w.runs++
		w.mu.Unlock()
		item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`
		if loop != nil {
			item = loop.answer(raw, item)
		}
		rw.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
			`{"type":"response.completed","response":{"id":"r1","status":"completed","model":"gpt-test","output":[]}}`,
			"[DONE]",
		} {
			_, _ = fmt.Fprintf(rw, "data: %s\n\n", event)
		}
	}))
	t.Cleanup(provider.Close)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	global := filepath.Join(home, ".jad")
	w.a, w.b = filepath.Join(base, "a"), filepath.Join(base, "b")
	put := func(path, text string) { writeWorkspaceTestFile(t, path, text) }
	put(filepath.Join(global, "global.json"), `{"workspaces":[{"id":"a","path":"`+w.a+`"},{"id":"b","path":"`+w.b+`"}]}`)
	put(filepath.Join(global, "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY","base_url":"`+provider.URL+`/v1"}`)
	for _, dir := range []string{w.a, w.b} {
		put(filepath.Join(dir, ".jad", "workspace.json"), `{}`)
		put(filepath.Join(dir, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
		put(filepath.Join(dir, ".jad", "profiles", "dev", "base.json"), `{"models":["openai/default"],"model_default":"openai/default"}`)
	}
	put(filepath.Join(w.b, ".jad", "sessions", "only-b.json"), `{"schema_version":1}`)
	environment := map[string]string{"JAD_API_TOKEN": integrationAPIToken, "HOME": home, "FIXTURE_KEY": "key", "JAD_REMOTE_E2E": e2eToken}
	var listener net.Listener
	if loop != nil {
		listener = loop.prepare(t, put, global, provider.URL, w.a, w.b)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: w.a, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	table := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(table, []byte(tokens), 0o600); err != nil {
		t.Fatal(err)
	}
	config.RemoteAPI = sessionfs.RemoteAPISettings{Enabled: true, TokensFile: table, MaxHops: 3, MaxRuns: 16, RunTimeoutMs: 60_000, RunRetentionMs: 60_000, WaitMaxMs: 5_000, BodyMaxBytes: 1 << 20, ResultMaxBytes: 64 << 10}
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	handlers := service.HandlersFor(config)
	server := httptest.NewUnstartedServer(handlers.Main)
	if listener != nil {
		// The server listens where the remote entry already points (it names its own address).
		_ = server.Listener.Close()
		server.Listener = listener
	}
	server.Start()
	t.Cleanup(server.Close)
	w.remoteServer = &remoteServer{config: config, service: service, url: server.URL, table: table, handlers: handlers, server: server}
	return w
}

func (w *remoteWorkspaces) send(t *testing.T, secret, session, message, requestID string) (int, map[string]any) {
	t.Helper()
	return w.call(t, "POST", remoteapi.Prefix+"/messages", secret, map[string]any{"session": session, "profile": "dev/base", "message": message, "request_id": requestID})
}

func (w *remoteWorkspaces) sendAndWait(t *testing.T, session, message, requestID string) {
	t.Helper()
	status, body := w.send(t, remoteTestSecret, session, message, requestID)
	if status != http.StatusAccepted {
		t.Fatalf("send to %s: %d %v", session, status, body)
	}
	if done := w.waitDone(t, body["id"].(string)); done["state"] != "done" {
		t.Fatalf("outcome for %s: %v", session, done)
	}
}

// A session id that two workspaces hold is reached as <workspace>:<session>: each address runs
// the profile of that workspace only, the bare id is refused as ambiguous before anything runs,
// and an id only one workspace holds still works bare (T-036).
func TestRemoteAPIReachesASessionOfEachWorkspaceByItsQualifiedName(t *testing.T) {
	w := startRemoteWorkspaces(t, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))

	status, body := w.send(t, remoteTestSecret, "s1", "msg-bare", "req-ws-00001")
	if status != http.StatusConflict || body["code"] != "ambiguous_session" {
		t.Fatalf("bare s1: %d %v", status, body)
	}
	if text := fmt.Sprint(body["error"]); !strings.Contains(text, "<workspace>:<session>") || strings.Contains(text, w.a) || strings.Contains(text, w.b) {
		t.Fatalf("the ambiguity text must give the form and no workspace path: %q", text)
	}
	if w.providerRuns() != 0 || w.transcript(w.a, "s1") != "" || w.transcript(w.b, "s1") != "" {
		t.Fatal("an ambiguous message ran a profile")
	}

	w.sendAndWait(t, "a:s1", "msg-for-a", "req-ws-00002")
	if !strings.Contains(w.transcript(w.a, "s1"), "msg-for-a") || w.transcript(w.b, "s1") != "" {
		t.Fatalf("a:s1 must reach workspace a only: a=%q b=%q", w.transcript(w.a, "s1"), w.transcript(w.b, "s1"))
	}
	w.sendAndWait(t, "b:s1", "msg-for-b", "req-ws-00003")
	if !strings.Contains(w.transcript(w.b, "s1"), "msg-for-b") || strings.Contains(w.transcript(w.a, "s1"), "msg-for-b") {
		t.Fatalf("b:s1 must reach workspace b only: a=%q b=%q", w.transcript(w.a, "s1"), w.transcript(w.b, "s1"))
	}
	w.sendAndWait(t, "only-b", "msg-bare-unique", "req-ws-00004")
	if !strings.Contains(w.transcript(w.b, "only-b"), "msg-bare-unique") {
		t.Fatal("a bare id one workspace holds must keep working")
	}

	// Unknown workspace, a session the named workspace does not hold and a malformed name never
	// run anything; the first two answer as a missing session, whichever workspace holds it.
	index := 0
	for session, want := range map[string]int{"c:s1": 404, "a:only-b": 404, "a:ghost": 404, "a:s1:x": 400, "a:": 400, ":s1": 400} {
		before := w.providerRuns()
		index++
		status, body := w.send(t, remoteTestSecret, session, "x", fmt.Sprintf("req-bad-%02d", index))
		if status != want || (want == 404 && body["code"] != "no_session") {
			t.Errorf("%s: %d %v, want %d", session, status, body, want)
		}
		if w.providerRuns() != before {
			t.Errorf("%s ran a profile", session)
		}
	}
	for _, workspace := range []string{w.a, w.b} {
		if _, err := os.Stat(filepath.Join(workspace, ".jad", "state", "ghost")); err == nil {
			t.Fatal("a state folder was made for a session that does not exist")
		}
	}

	// The profile list of a session answers as a missing one when the id is ambiguous, so it
	// does not tell a token that the id exists twice; the qualified id lists.
	if status, body := w.call(t, "GET", remoteapi.Prefix+"/profiles?session=s1", remoteTestSecret, nil); status != 404 || body["code"] != "no_session" {
		t.Fatalf("profiles of the bare ambiguous id: %d %v", status, body)
	}
	if status, body := w.call(t, "GET", remoteapi.Prefix+"/profiles?session="+url.QueryEscape("b:s1"), remoteTestSecret, nil); status != 200 || fmt.Sprint(body["profiles"]) != "[dev/base]" {
		t.Fatalf("profiles of b:s1: %d %v", status, body)
	}
	// profile info names the ambiguity (the token's globs have admitted the session id by then).
	if status, body := w.call(t, "GET", remoteapi.Prefix+"/profile?session=s1&profile=dev/base", remoteTestSecret, nil); status != 409 || body["code"] != "ambiguous_session" {
		t.Fatalf("info of the bare ambiguous id: %d %v", status, body)
	}
	if status, body := w.call(t, "GET", remoteapi.Prefix+"/profile?session=a:s1&profile=dev/base", remoteTestSecret, nil); status != 200 {
		t.Fatalf("info of a:s1: %d %v", status, body)
	}
}

// A token's globs name sessions as the caller writes them: a glob that only means bare ids
// ("*1", every id ending in 1) or one workspace does not reach another workspace's session, and one that names the
// workspace ("a:*") reaches that workspace only. "*" still means every session.
func TestRemoteAPITokenGlobsDoNotReachQualifiedSessionsByAccident(t *testing.T) {
	grant := func(glob string) string {
		return `"profiles":["` + glob + `"],"actions":["message","status","stop","transcripts","profile"]`
	}
	w := startRemoteWorkspaces(t, tokenTable(
		tokenRow("suffix", "suffix_0123456789abcdef0123456789abcdef", grant("*1/dev/*")),
		tokenRow("workspace", "workspace_0123456789abcdef0123456789abcd", grant("a:*/dev/*")),
		tokenRow("bare", "bare_0123456789abcdef0123456789abcdefab", grant("only-b/dev/*")),
		tokenRow("any", remoteTestSecret, grant("*/dev/*"))))
	for _, c := range []struct {
		secret, session string
		want            int
	}{
		{"suffix_0123456789abcdef0123456789abcdef", "a:s1", 403},
		{"suffix_0123456789abcdef0123456789abcdef", "b:s1", 403},
		{"workspace_0123456789abcdef0123456789abcd", "b:s1", 403},
		{"workspace_0123456789abcdef0123456789abcd", "a:s1", 202},
		{"workspace_0123456789abcdef0123456789abcd", "only-b", 403},
		{"bare_0123456789abcdef0123456789abcdefab", "b:only-b", 403},
		{"bare_0123456789abcdef0123456789abcdefab", "only-b", 202},
		{remoteTestSecret, "b:s1", 202},
		{remoteTestSecret, "a:s1", 202},
	} {
		status, body := w.send(t, c.secret, c.session, "scoped", "req-tok-"+strings.ReplaceAll(c.session, ":", "-")+c.secret[:3])
		if status != c.want {
			t.Errorf("%s with %s: %d %v, want %d", c.session, c.secret[:6], status, body, c.want)
		}
	}
	// The bare ambiguous id is refused as ambiguous only for a token whose globs admit it: a
	// token that does not reach s1 gets the answer for a missing session.
	if status, body := w.send(t, "suffix_0123456789abcdef0123456789abcdef", "s1", "x", "req-tok-ambig-01"); status != 409 || body["code"] != "ambiguous_session" {
		t.Fatalf("a token admitted by *1: %d %v", status, body)
	}
	if status, body := w.send(t, "bare_0123456789abcdef0123456789abcdefab", "s1", "x", "req-tok-ambig-02"); status != 403 {
		t.Fatalf("a token that does not reach s1 must not learn it is ambiguous: %d %v", status, body)
	}
}

// The remote reads (transcript, transcripts) work in a server with several workspaces: the
// reader of the remote API is not a session, so it is not confined to one workspace, and it
// names a session as <workspace>:<session>.
func TestRemoteTranscriptsReadTheQualifiedSessionOfEachWorkspace(t *testing.T) {
	w := startRemoteWorkspaces(t, tokenTable(tokenRow("client", e2eToken, fullGrant)))
	for workspace, text := range map[string]string{w.a: "the answer of workspace a", w.b: "the answer of workspace b"} {
		file := filepath.Join(workspace, ".jad", "state", "s1", "dev", "base", "messages.jsonl")
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(remoteUser("question")+remoteAnswer(1, text)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	readers := startRemoteReaders(t, w.remoteServer, true, "peer")
	for session, want := range map[string]string{"a:s1": "workspace a", "b:s1": "workspace b"} {
		out := readers.run(t, readers.transcript, map[string]any{"path": "remote:peer/profile:" + session + "/dev/base"})
		if !strings.Contains(out, want) || strings.Contains(out, "messages.jsonl") || strings.Contains(out, w.a) {
			t.Errorf("%s:\n%s", session, out)
		}
	}
	if out := readers.run(t, readers.transcript, map[string]any{"path": "remote:peer/profile:s1/dev/base"}); strings.Contains(out, "workspace a") || strings.Contains(out, "workspace b") || !strings.Contains(out, "error_code") {
		t.Fatalf("a bare ambiguous id must read nothing:\n%s", out)
	}
	if out := readers.run(t, readers.transcript, map[string]any{"path": "remote:peer/profile:c:s1/dev/base"}); strings.Contains(out, "workspace") || !strings.Contains(out, "error_code") {
		t.Fatalf("an unknown workspace:\n%s", out)
	}
}

// The client end: the real profile_message tool of another JAD reaches the right workspace
// of the server through remote:<name>/<workspace>:<session>/..., and the ambiguity of a bare id
// is told with the form to write.
func TestRemoteE2EProfileMessageNamesTheWorkspace(t *testing.T) {
	w := startRemoteWorkspaces(t, tokenTable(tokenRow("client", e2eToken, fullGrant)))
	client := startRemoteClient(t, w.url, "")
	if out := client.call("remote:peer/b:s1/dev/base", 30_000); !strings.Contains(out, "ok") || strings.Contains(out, "error_code") {
		t.Fatalf("b:s1:\n%s", out)
	}
	if !strings.Contains(w.transcript(w.b, "s1"), "what is the answer") || w.transcript(w.a, "s1") != "" {
		t.Fatalf("the message must reach workspace b only: a=%q", w.transcript(w.a, "s1"))
	}
	runs := w.providerRuns()
	out := client.call("remote:peer/s1/dev/base", 30_000)
	if !strings.Contains(out, "error_code: remote_ambiguous_session") || !strings.Contains(out, "<workspace>:<session>") {
		t.Fatalf("bare s1:\n%s", out)
	}
	if w.providerRuns() != runs || w.transcript(w.a, "s1") != "" {
		t.Fatal("an ambiguous message ran a profile")
	}
	if out := client.call("remote:peer/only-b/dev/base", 30_000); !strings.Contains(out, "ok") || strings.Contains(out, "error_code") {
		t.Fatalf("only-b:\n%s", out)
	}
	if out := client.call("remote:peer/a:s1:x/dev/base", 30_000); !strings.Contains(out, "bad_remote_reference") {
		t.Fatalf("two colons:\n%s", out)
	}
}

// A hop-limited request for a qualified session stops before any work, as an unqualified one.
func TestRemoteAPIHopLimitAppliesToQualifiedSessions(t *testing.T) {
	w := startRemoteWorkspaces(t, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))
	status, body := w.call(t, "POST", remoteapi.Prefix+"/messages", remoteTestSecret, map[string]any{
		"session": "a:s1", "profile": "dev/base", "message": "x", "request_id": "req-hop-ws-01"}, remoteapi.HeaderHops, "4")
	if status != http.StatusLoopDetected || body["code"] != "remote_hop_limit" || w.providerRuns() != 0 {
		t.Fatalf("%d %v", status, body)
	}
}

// The local profile_message never leaves the caller's workspace, however the other workspace's
// session is written: b:s1 and only-b (a session only b holds) answer as a session that does not
// exist, so the answer does not tell that another workspace has it (design Q6).
func TestProfileMessageToAnotherWorkspaceReadsLikeAMissingSession(t *testing.T) {
	m := twoWorkspaces(t)
	settings := sessionfs.NativeProfileMessageSettings{TimeoutMs: 60_000, ResultMaxBytes: 4096, MaxChain: 2, MaxQueued: 4, Sessions: []string{"*"}}
	tool, err := native.NewProfileMessageTool(settings, []string{"dev/other"}, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := func(profile string) string {
		t.Helper()
		out, err := tool.Execute(context.Background(), map[string]any{"profile": profile, "message": "hi"}, tools.ExecutionContext{CallID: "call", SessionKey: "a:s1\x00dev/coder"})
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(out, profile, "PROFILE")
	}
	missing := run("ghost/dev/coder")
	if !strings.Contains(missing, "error_code: unknown_profile") {
		t.Fatalf("a missing session: %s", missing)
	}
	for _, profile := range []string{"b:s1/dev/coder", "only-b/dev/coder", "b:only-b/dev/coder"} {
		if got := run(profile); got != missing {
			t.Errorf("%s:\n got %s\nwant %s", profile, got, missing)
		}
	}
}
