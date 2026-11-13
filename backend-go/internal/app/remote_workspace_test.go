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
	environment := map[string]string{"JAD_API_TOKEN": integrationAPIToken, "HOME": home, "FIXTURE_KEY": "key", "JAD_REMOTE_E2E": e2eToken, "JAD_REMOTE_LOOP_B": loopTokenB}
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

// sendAndWaitAs sends with a token and waits for the run with the same token.
func (w *remoteWorkspaces) sendAndWaitAs(t *testing.T, secret, session, message, requestID string) {
	t.Helper()
	status, body := w.send(t, secret, session, message, requestID)
	if status != http.StatusAccepted {
		t.Fatalf("send to %s: %d %v", session, status, body)
	}
	if done := w.waitDoneAs(t, secret, body["id"].(string)); done["state"] != "done" {
		t.Fatalf("outcome for %s: %v", session, done)
	}
}

const rowBSecret = "rowb_0123456789abcdef0123456789abcdefab"

// workspaceRow is a token row of the workspace layout: it names the one workspace it reaches.
func workspaceRow(id, secret, workspace, grant string) string {
	return tokenRow(id, secret, `"workspace":"`+workspace+`",`+grant)
}

// startRowWorkspaces is the two-workspace server with one full-grant row for each workspace:
// remoteTestSecret reaches a and rowBSecret reaches b.
func startRowWorkspaces(t *testing.T) *remoteWorkspaces {
	t.Helper()
	return startRemoteWorkspaces(t, tokenTable(workspaceRow("peer", remoteTestSecret, "a", fullGrant), workspaceRow("peerb", rowBSecret, "b", fullGrant)))
}

// A token row of a server with several workspaces reaches the one workspace it names: the
// session is written bare or as <that workspace>:<session> and means the same thing, a session
// of another workspace answers as a session that does not exist (also when only the other
// workspace holds it), and a session id both workspaces hold is no ambiguity for a row (T-040 C).
func TestRemoteAPIRowReachesOnlyItsWorkspace(t *testing.T) {
	w := startRowWorkspaces(t)

	w.sendAndWaitAs(t, remoteTestSecret, "s1", "bare-for-a", "req-row-00001")
	if !strings.Contains(w.transcript(w.a, "s1"), "bare-for-a") || w.transcript(w.b, "s1") != "" {
		t.Fatalf("a bare s1 of row a must reach workspace a only: a=%q b=%q", w.transcript(w.a, "s1"), w.transcript(w.b, "s1"))
	}
	w.sendAndWaitAs(t, remoteTestSecret, "a:s1", "qualified-for-a", "req-row-00002")
	if !strings.Contains(w.transcript(w.a, "s1"), "qualified-for-a") {
		t.Fatal("a:s1 of row a must reach workspace a")
	}
	w.sendAndWaitAs(t, rowBSecret, "s1", "bare-for-b", "req-row-00003")
	if !strings.Contains(w.transcript(w.b, "s1"), "bare-for-b") || strings.Contains(w.transcript(w.a, "s1"), "bare-for-b") {
		t.Fatalf("a bare s1 of row b must reach workspace b only: a=%q", w.transcript(w.a, "s1"))
	}
	w.sendAndWaitAs(t, rowBSecret, "only-b", "bare-unique", "req-row-00004")
	if !strings.Contains(w.transcript(w.b, "only-b"), "bare-unique") {
		t.Fatal("a session only b holds must be reachable by row b")
	}

	// Another workspace, a workspace the server does not serve, a session the row's workspace
	// does not hold and a malformed name run nothing; none of them is told apart from a missing
	// session, so a row learns nothing about a workspace that is not its own.
	runs := w.providerRuns()
	index := 0
	for _, c := range []struct{ secret, session string }{
		{remoteTestSecret, "b:s1"}, {remoteTestSecret, "only-b"}, {remoteTestSecret, "a:only-b"}, {remoteTestSecret, "c:s1"},
		{remoteTestSecret, "a:ghost"}, {rowBSecret, "a:s1"}, {rowBSecret, "b:ghost"},
	} {
		index++
		status, body := w.send(t, c.secret, c.session, "x", fmt.Sprintf("req-bad-%02d", index))
		if status != http.StatusNotFound || body["code"] != "no_session" {
			t.Errorf("%s: %d %v, want a missing session", c.session, status, body)
		}
	}
	for _, session := range []string{"a:s1:x", "a:", ":s1"} {
		index++
		if status, body := w.send(t, remoteTestSecret, session, "x", fmt.Sprintf("req-bad-%02d", index)); status != http.StatusBadRequest {
			t.Errorf("%s: %d %v, want bad_request", session, status, body)
		}
	}
	if w.providerRuns() != runs {
		t.Error("a request outside the row's workspace ran a profile")
	}
	if _, err := os.Stat(filepath.Join(w.a, ".jad", "state", "ghost")); err == nil {
		t.Fatal("a state folder was made for a session that does not exist")
	}

	// The same limit on the profile list and the profile info.
	for query, want := range map[string]int{"s1": 200, "a:s1": 200, "b:s1": 404, "only-b": 404} {
		if status, body := w.call(t, "GET", remoteapi.Prefix+"/profiles?session="+url.QueryEscape(query), remoteTestSecret, nil); status != want {
			t.Errorf("profiles of %s: %d %v, want %d", query, status, body, want)
		}
		if status, body := w.call(t, "GET", remoteapi.Prefix+"/profile?session="+url.QueryEscape(query)+"&profile=dev/base", remoteTestSecret, nil); status != want {
			t.Errorf("info of %s: %d %v, want %d", query, status, body, want)
		}
	}
	if status, body := w.call(t, "GET", remoteapi.Prefix+"/profiles?session=only-b", rowBSecret, nil); status != 200 || fmt.Sprint(body["profiles"]) != "[dev/base]" {
		t.Fatalf("profiles of only-b for row b: %d %v", status, body)
	}

	// A run is visible to the token that started it only.
	status, body := w.send(t, remoteTestSecret, "s1", "mine", "req-row-00099")
	if status != http.StatusAccepted {
		t.Fatalf("%d %v", status, body)
	}
	id := body["id"].(string)
	if status, _ := w.call(t, "GET", remoteapi.Prefix+"/runs/"+id, rowBSecret, nil); status != http.StatusNotFound {
		t.Fatalf("another row saw the run: %d", status)
	}
	w.waitDoneAs(t, remoteTestSecret, id)
}

// A row's globs are matched on the bare session inside its workspace (a colon in a glob is a
// load error, tested in remoteapi), and a glob that does not match refuses the start before any
// lookup, whichever way the session is written.
func TestRemoteAPIRowGlobsAreOverTheBareSession(t *testing.T) {
	grant := func(glob string) string {
		return `"profiles":["` + glob + `"],"actions":["message","status","stop","transcripts","profile"]`
	}
	const other = "other_0123456789abcdef0123456789abcdefab"
	w := startRemoteWorkspaces(t, tokenTable(
		workspaceRow("peer", remoteTestSecret, "a", grant("s1/dev/*")),
		workspaceRow("other", other, "a", grant("s2/dev/*")),
		workspaceRow("suffix", rowBSecret, "b", grant("*1/dev/*"))))
	for _, c := range []struct {
		secret, session string
		want            int
	}{
		{remoteTestSecret, "s1", 202}, {remoteTestSecret, "a:s1", 202}, {remoteTestSecret, "b:s1", 404},
		{other, "s1", 403}, {other, "a:s1", 403},
		{rowBSecret, "s1", 202}, {rowBSecret, "b:s1", 202}, {rowBSecret, "a:s1", 404}, {rowBSecret, "only-b", 403},
	} {
		status, body := w.send(t, c.secret, c.session, "scoped", "req-glob-"+strings.ReplaceAll(c.session, ":", "-")+c.secret[:3])
		if status != c.want {
			t.Errorf("%s with %s: %d %v, want %d", c.session, c.secret[:5], status, body, c.want)
		}
	}
}

// The remote reads (transcript, transcripts) of a row see its workspace only, with the session
// written bare or qualified, and a cursor of one row's read does not continue for another.
func TestRemoteTranscriptsOfARowAreOfItsWorkspace(t *testing.T) {
	w := startRowWorkspaces(t)
	var lines strings.Builder
	for i := 0; i < 70; i++ {
		lines.WriteString(remoteUser(fmt.Sprintf("question %d", i)))
		lines.WriteString(remoteAnswer(i+1, fmt.Sprintf("answer %d", i)))
	}
	for workspace, text := range map[string]string{w.a: remoteUser("q") + remoteAnswer(1, "the words of a:s1"), w.b: remoteUser("q") + remoteAnswer(1, "the words of b:s1")} {
		file := filepath.Join(workspace, ".jad", "state", "s1", "dev", "base", "messages.jsonl")
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	long := filepath.Join(w.b, ".jad", "state", "only-b", "dev", "base", "messages.jsonl")
	if err := os.MkdirAll(filepath.Dir(long), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(long, []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func(secret, path string) (bool, string) {
		t.Helper()
		status, body := w.call(t, "GET", remoteapi.Prefix+"/transcript?path="+url.QueryEscape(path), secret, nil)
		out, _ := body["output"].(string)
		return status == 200 && !strings.Contains(out, "error_code"), out
	}
	for _, c := range []struct {
		secret, path string
		want         string
	}{
		{remoteTestSecret, "profile:s1/dev/base", "the words of a:s1"}, {remoteTestSecret, "profile:a:s1/dev/base", "the words of a:s1"},
		{rowBSecret, "profile:s1/dev/base", "the words of b:s1"}, {rowBSecret, "profile:b:s1/dev/base", "the words of b:s1"},
		{rowBSecret, "profile:only-b/dev/base", "answer 69"},
	} {
		if ok, out := read(c.secret, c.path); !ok || !strings.Contains(out, c.want) || strings.Contains(out, "messages.jsonl") || strings.Contains(out, w.a) {
			t.Errorf("%s reading %s: %v\n%s", c.secret[:5], c.path, ok, out)
		}
	}
	for _, c := range []struct{ secret, path string }{
		{remoteTestSecret, "profile:b:s1/dev/base"}, {remoteTestSecret, "profile:only-b/dev/base"}, {remoteTestSecret, "profile:c:s1/dev/base"},
		{rowBSecret, "profile:a:s1/dev/base"},
	} {
		if ok, out := read(c.secret, c.path); ok || strings.Contains(out, "the words of") {
			t.Errorf("%s read %s outside its workspace:\n%s", c.secret[:5], c.path, out)
		}
	}
	_, first := read(rowBSecret, "profile:only-b/dev/base")
	match := remoteCursorLine.FindStringSubmatch(first)
	if match == nil {
		t.Fatalf("140 steps exceed one page, so a cursor is expected:\n%s", first)
	}
	cursor := url.QueryEscape(match[1])
	if status, body := w.call(t, "GET", remoteapi.Prefix+"/transcript?cursor="+cursor, rowBSecret, nil); status != 200 || strings.Contains(fmt.Sprint(body["output"]), "error_code") {
		t.Fatalf("the cursor did not continue for its own row: %d %v", status, body)
	}
	if status, body := w.call(t, "GET", remoteapi.Prefix+"/transcript?cursor="+cursor, remoteTestSecret, nil); status == 200 && !strings.Contains(fmt.Sprint(body["output"]), "error_code") {
		t.Fatalf("another row continued a cursor of a session outside its workspace: %v", body)
	}
}

// A hop-limited request stops before any work, as before.
func TestRemoteAPIHopLimitAppliesToQualifiedSessions(t *testing.T) {
	w := startRowWorkspaces(t)
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
