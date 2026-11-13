package app

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

// startLoop starts p1 of a:s1 through the row "peer" and returns when its run ends.
func startLoop(t *testing.T, loop *remoteLoop, w *remoteWorkspaces) {
	t.Helper()
	status, body := w.call(t, "POST", remoteapi.Prefix+"/messages", remoteTestSecret, map[string]any{
		"session": "a:s1", "profile": "dev/p1", "message": "start the loop", "request_id": "req-bind-00001"})
	if status != http.StatusAccepted {
		t.Fatalf("start: %d %v", status, body)
	}
	if done := w.waitDone(t, body["id"].(string)); done["state"] != "done" {
		t.Fatalf("the first run did not end well: %v", done)
	}
}

func loopTranscript(w *remoteWorkspaces, workspace, profile string) string {
	data, _ := os.ReadFile(filepath.Join(workspace, ".jad", "state", "s1", "dev", profile, "messages.jsonl"))
	return string(data)
}

// A binding the tool lists but the session has no key for is not usable: it is not offered to the
// model, a call to it is refused as an unknown remote before any request, and nothing reaches the
// other server. A key never grants reach by itself: it only supplies the credential.
func TestListedBindingWithoutAKeyIsNotOfferedAndNotReached(t *testing.T) {
	loop := &remoteLoop{noKeys: true}
	w := startRemoteWorkspacesWith(t, loopRows(), loop)
	startLoop(t, loop, w)
	if n := loop.requests("model-p2"); n != 0 {
		t.Fatalf("p2 was reached %d times without a key", n)
	}
	first := loopTranscript(w, w.a, "p1")
	if !strings.Contains(first, "unknown_remote") && !strings.Contains(first, "does not enable remote profiles") {
		t.Fatalf("p1 was not told the binding is unusable:\n%s", first)
	}
	loop.mu.Lock()
	body := loop.first
	loop.mu.Unlock()
	if strings.Contains(body, "self-a") || strings.Contains(body, "self-b") {
		t.Fatalf("an unkeyed binding is offered to the model:\n%s", body)
	}
}

// A workspace's deny rule without from also refuses an incoming remote message into it (a rule
// with from is about local callers and does not match a remote one). The remote answers like a
// token that does not reach the target; the loop stops there and the target does not run.
func TestWorkspaceDenyRuleWithoutFromRefusesIncomingRemoteMessages(t *testing.T) {
	for name, c := range map[string]struct {
		rule      string
		p2Runs    bool
		p2Refused bool
	}{
		"to only":      {`{"to":"local:b/*/dev/p2"}`, false, true},
		"to with from": {`{"from":"local:a/*/*/*","to":"local:b/*/dev/p2"}`, true, false},
		"other target": {`{"to":"local:b/*/dev/p9"}`, true, false},
		"reset only":   {`{"to":"local:b/*/dev/p2","actions":["reset"]}`, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			loop := &remoteLoop{workspaceB: `,"profile_message":{"deny":[` + c.rule + `]}`}
			w := startRemoteWorkspacesWith(t, loopRows(), loop)
			startLoop(t, loop, w)
			ran := loop.requests("model-p2") > 0
			if ran != c.p2Runs {
				t.Fatalf("p2 ran %v, want %v", ran, c.p2Runs)
			}
			first := loopTranscript(w, w.a, "p1")
			if c.p2Refused && (!strings.Contains(first, "remote_forbidden") || strings.Contains(first, "profile_denied")) {
				t.Fatalf("p1 must see the answer for a token that does not reach the target (remote_forbidden), and no rule text:\n%s", first)
			}
		})
	}
}

// The global deny rules reach incoming remote messages too.
func TestGlobalDenyRuleRefusesIncomingRemoteMessages(t *testing.T) {
	loop := &remoteLoop{global: `,"profile_message":{"deny":[{"to":"local:*/*/dev/p2"}]}`}
	w := startRemoteWorkspacesWith(t, loopRows(), loop)
	startLoop(t, loop, w)
	if n := loop.requests("model-p2"); n != 0 {
		t.Fatalf("p2 ran %d times despite a global rule", n)
	}
	if !strings.Contains(loopTranscript(w, w.a, "p1"), "remote_forbidden") {
		t.Fatal("p1 did not see the refusal")
	}
}

// bindingClient is startRemoteClient for a client of the workspace layout: the remote is a binding
// of one workspace, with a key file, and the directory is scoped to that key.
func bindingClient(t *testing.T, entryURL, workspace string) *remoteClient {
	t.Helper()
	local := newLayoutFixture(t)
	dir, keys := t.TempDir(), t.TempDir()
	binding := fmt.Sprintf(`{"kind":"remote","remote":{"url":%q,"workspace":%q,"timeout_ms":5000,"poll_ms":300,"lost_after_ms":1500,"retries":1}}`, entryURL, workspace)
	if err := os.WriteFile(filepath.Join(dir, "peer.json"), []byte(binding), 0o600); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(keys, "peer-key.json")
	if err := os.WriteFile(keyFile, []byte(`{"kind":"remote_key","binding":"peer","token":"`+e2eToken+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	hub := remote.NewHub(nil)
	t.Cleanup(hub.Close)
	directory := hub.Directory(dir).Scoped(remote.Scope{Workspace: true, Keys: map[string]remote.Key{"peer": {Name: "peer-key", File: keyFile}}})
	host, err := local.service.resolver.AsyncHost(native.AsyncHostConfig{Dir: native.AsyncDir(filepath.Join(local.cwd, "pm")), Config: asyncmgr.DefaultConfig()})
	if err != nil {
		t.Fatal(err)
	}
	settings := sessionfs.NativeProfileMessageSettings{TimeoutMs: 60_000, ResultMaxBytes: 4096, MaxChain: 2, MaxQueued: 4, Remotes: []string{"peer"}}
	messenger := remoteClientMessenger{profileMessenger: profileMessenger{resolver: local.service.resolver}, hub: hub}
	message, err := native.NewProfileMessageToolWithRemotes(settings, []string{"review/reviewer"}, messenger, host, directory)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := local.service.resolver.CanonicalSessionReference("local:demo")
	if err != nil {
		t.Fatal(err)
	}
	return &remoteClient{t: t, message: message, owner: sessionKey(reference, "dev/coder"), hub: hub, dir: dir}
}

// A client of the workspace layout reaches a v1 server (several roots, or one) through a binding
// whose workspace is the server's root id: the session goes out as <root>:<session>, which a v1
// token row admits only with a glob whose session part is * or names the root (T-036). A row of
// bare globs refuses it; that is the documented limit of a v2 client against a v1 server.
func TestBindingClientReachesAV1ServerOnlyThroughQualifiedGlobs(t *testing.T) {
	server, _ := e2eServer(t)
	client := bindingClient(t, server.url, "local")
	if out := client.call("remote:peer/demo/dev/coder", 30_000); !strings.Contains(out, "answered") || strings.Contains(out, "error_code") || strings.Contains(out, e2eToken) {
		t.Fatalf("a row with * globs:\n%s", out)
	}
	// The old forms are not binding addresses.
	for _, ref := range []string{"remote:peer/dev/coder", "remote:peer/local:demo/dev/coder", "remote:peer/demo/dev/coder/extra"} {
		if out := client.call(ref, 30_000); !strings.Contains(out, "invalid_profile_ref") {
			t.Errorf("%s:\n%s", ref, out)
		}
	}

	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	bare := startRemoteServer(t, f, tokenTable(tokenRow("client", e2eToken, `"profiles":["demo/dev/*"],"actions":["message","status","stop"]`)))
	narrow := bindingClient(t, bare.url, "local")
	if out := narrow.call("remote:peer/demo/dev/coder", 30_000); !strings.Contains(out, "remote_forbidden") {
		t.Fatalf("a bare-glob row must refuse a qualified session:\n%s", out)
	}
}

// jad check warns of each binding that reaches the server itself, and says nothing when the remote
// API is off or the bindings point elsewhere. The warning never fails the check.
func TestCheckWarnsOfABindingThatPointsAtTheServerItself(t *testing.T) {
	w := startRemoteWorkspacesWith(t, loopRows(), &remoteLoop{})
	address, err := url.Parse(w.url)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(address.Port())
	check := func(server sessionfs.ServerSettings) string {
		var out strings.Builder
		CheckSelfBindings(w.config.Roots, server, &out)
		return out.String()
	}
	on := sessionfs.ServerSettings{Host: "127.0.0.1", Port: port, RemoteAPI: sessionfs.RemoteAPISettings{Enabled: true}}
	text := check(on)
	for _, binding := range []string{"self-a", "self-b"} {
		if !strings.Contains(text, "remote binding "+binding+" points at this server itself") {
			t.Errorf("no warning for %s:\n%s", binding, text)
		}
	}
	if strings.Contains(text, loopTokenA) || strings.Contains(text, "key-a") {
		t.Errorf("the warning names a secret or a key:\n%s", text)
	}
	off := on
	off.RemoteAPI.Enabled = false
	other := on
	other.Port = port + 1
	if got := check(off) + check(other); got != "" {
		t.Errorf("no warning expected, got:\n%s", got)
	}
}

// jad check prints the unkeyed bindings as notes, and they do not make it fail.
func TestCheckFilesPrintsUnkeyedBindingsAsNotes(t *testing.T) {
	w := startRemoteWorkspacesWith(t, loopRows(), &remoteLoop{noKeys: true})
	var out strings.Builder
	valid, err := CheckFiles(w.config.Roots, &out)
	if err != nil || !valid {
		t.Fatalf("%v %v\n%s", valid, err, out.String())
	}
	if !strings.Contains(out.String(), "note: ") || !strings.Contains(out.String(), "lists binding self-a, but no key in remote_keys reaches it") {
		t.Fatalf("no note:\n%s", out.String())
	}
}
