package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

var regexpHandle = pmHandle

const e2eToken = "JADREMOTE_e2e_0123456789abcdef0123456789abcd"

// remoteClientMessenger is the client JAD's messenger: the real profile messenger of a second
// resolver, with the remote hub reading the token from the test environment.
type remoteClientMessenger struct {
	profileMessenger
	hub *remote.Hub
}

func (m remoteClientMessenger) RemoteDirectory(dir string) *remote.Directory {
	return m.hub.Directory(dir)
}

// remoteClient is a second JAD (its own resolver, async host and tools) that calls a remote.
type remoteClient struct {
	t       *testing.T
	message tools.Tool
	async   tools.Tool
	owner   string
	hub     *remote.Hub
	dir     string
	// directory is the binding directory scoped to the key the client holds.
	directory *remote.Directory
}

// startRemoteClient is a client of the workspace layout whose binding "peer" reaches the workspace
// "local" of the server at entryURL with a key (the token of JAD_REMOTE_E2E); extra is more JSON
// keys of the binding's remote object.
func startRemoteClient(t *testing.T, entryURL, extra string) *remoteClient {
	t.Helper()
	return newBoundClient(t, entryURL, "local", extra)
}

func newBoundClient(t *testing.T, entryURL, workspace, extra string) *remoteClient {
	t.Helper()
	local := newLayoutFixture(t)
	dir, keys := t.TempDir(), t.TempDir()
	binding := fmt.Sprintf(`{"kind":"remote","remote":{"url":%q,"workspace":%q,"timeout_ms":5000,"poll_ms":300,"lost_after_ms":1500,"retries":1%s}}`, entryURL, workspace, extra)
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
	async, err := native.NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := local.service.resolver.CanonicalSessionReference("local:demo")
	if err != nil {
		t.Fatal(err)
	}
	return &remoteClient{t: t, message: message, async: async, owner: sessionKey(reference, "dev/coder"), hub: hub, dir: dir, directory: directory}
}

func (c *remoteClient) call(target string, block int64) string {
	c.t.Helper()
	out, err := c.message.Execute(context.Background(), map[string]any{"profile": target, "message": "what is the answer", "block_ms": json.Number(fmt.Sprint(block))},
		tools.ExecutionContext{CallID: "call", SessionKey: c.owner})
	if err != nil {
		c.t.Fatal(err)
	}
	return out
}

func (c *remoteClient) asyncCall(arguments map[string]any) string {
	c.t.Helper()
	out, err := c.async.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "async", SessionKey: c.owner})
	if err != nil {
		c.t.Fatal(err)
	}
	return out
}

func e2eServer(t *testing.T) (*remoteServer, subagentFixture) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	return startRemoteServer(t, f, tokenTable(tokenRow("client", e2eToken, fullGrant))), f
}

func TestRemoteE2EProfileMessageOverTCP(t *testing.T) {
	server, f := e2eServer(t)
	client := startRemoteClient(t, server.url, "")
	out := client.call("remote:peer/demo/dev/coder", 30_000)
	if !strings.Contains(out, "answered") || !strings.Contains(out, "parent saw") {
		t.Fatalf("result:\n%s", out)
	}
	if got := f.provider.forModel("parent-model"); len(got) == 0 || !strings.Contains(got[0].Text, "what is the answer") {
		t.Fatal("the server's profile never saw the client's message")
	}
	// The answer is the remote model's own words (this fake provider echoes a tool result,
	// path included); the tool's own metadata must name no file of either machine.
	metadata, _, _ := strings.Cut(out, "--- CONTENT")
	if strings.Contains(metadata, "transcript") || strings.Contains(metadata, f.workspace) || strings.Contains(metadata, "messages.jsonl") {
		t.Fatalf("the result's information names a file:\n%s", metadata)
	}
	if strings.Contains(out, e2eToken) {
		t.Fatal("token in a result")
	}
}

func TestRemoteE2EProfileMessageOverAUnixSocket(t *testing.T) {
	server, f := e2eServer(t)
	dir, err := os.MkdirTemp("", "jade2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "r.sock")
	listener, err := ListenUnix(socket, 0o600)
	if err != nil {
		t.Skip("unix sockets unavailable: ", err)
	}
	socketServer := &http.Server{Handler: server.service.Remote}
	go socketServer.Serve(listener)
	t.Cleanup(func() { socketServer.Close() })
	client := startRemoteClient(t, "unix://"+socket, "")
	out := client.call("remote:peer/demo/dev/coder", 30_000)
	if !strings.Contains(out, "parent saw") {
		t.Fatalf("result over the socket:\n%s", out)
	}
	if len(f.provider.forModel("parent-model")) == 0 {
		t.Fatal("the profile never ran")
	}
}

func TestRemoteE2EHandleWaitAndStopActOnTheRemoteRun(t *testing.T) {
	server, f := e2eServer(t)
	f.provider.childBlocks = true
	f.provider.childEntered = make(chan struct{}, 8)
	f.provider.childRelease = make(chan struct{})
	t.Cleanup(func() { close(f.provider.childRelease) })
	client := startRemoteClient(t, server.url, "")
	out := client.call("remote:peer/demo/dev/coder", 0)
	id := regexpHandle.FindString(out)
	if id == "" {
		t.Fatalf("expected a handle:\n%s", out)
	}
	select {
	case <-f.provider.childEntered:
	case <-time.After(15 * time.Second):
		t.Fatal("the remote run never reached its child")
	}
	if status := client.asyncCall(map[string]any{"action": "status", "id": id}); !strings.Contains(status, "running") {
		t.Fatalf("status:\n%s", status)
	}
	stopped := client.asyncCall(map[string]any{"action": "stop", "id": id})
	if !strings.Contains(stopped, `"state":"stopped"`) {
		t.Fatalf("stop:\n%s", stopped)
	}
	reference, _ := server.service.resolver.CanonicalSessionReference("local:demo")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, running := server.service.resolver.profileRunStarter().RunningProfileRunCaller(reference, "dev/coder"); !running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the remote profile run kept going after the client stopped the handle")
}

func TestRemoteE2EConnectionDropBecomesLostAndTheRemoteKeepsItsRun(t *testing.T) {
	server, f := e2eServer(t)
	f.provider.childBlocks = true
	f.provider.childEntered = make(chan struct{}, 8)
	f.provider.childRelease = make(chan struct{})
	t.Cleanup(func() { close(f.provider.childRelease) })
	client := startRemoteClient(t, server.url, "")
	out := client.call("remote:peer/demo/dev/coder", 0)
	id := regexpHandle.FindString(out)
	select {
	case <-f.provider.childEntered:
	case <-time.After(15 * time.Second):
		t.Fatal("the remote run never reached its child")
	}
	server.gate.cut.Store(true)
	waited := client.asyncCall(map[string]any{"action": "wait", "ids": []any{id}, "wait_ms": json.Number("20000")})
	if !strings.Contains(waited, `"state":"lost"`) {
		t.Fatalf("a long outage must end the handle as lost:\n%s", waited)
	}
	reference, _ := server.service.resolver.CanonicalSessionReference("local:demo")
	if _, running := server.service.resolver.profileRunStarter().RunningProfileRunCaller(reference, "dev/coder"); !running {
		t.Fatal("losing the connection must not stop the remote run: the client cannot know it")
	}
}

func TestRemoteE2ERefusalsFromTheServerReachTheModelWithoutLeaks(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	server := startRemoteServer(t, f, tokenTable(tokenRow("client", e2eToken, `"profiles":["demo/other/*"],"actions":["message","status","stop"]`)))
	client := startRemoteClient(t, server.url, "")
	out := client.call("remote:peer/demo/dev/coder", 5000)
	if !strings.Contains(out, "remote_forbidden") || strings.Contains(out, e2eToken) {
		t.Fatalf("scope refusal:\n%s", out)
	}
	if len(f.provider.forModel("parent-model")) != 0 {
		t.Fatal("a refused message ran the profile")
	}
	// A binding written for the https era is refused by the loader (jad check names it): the model
	// is refused and never sees the token.
	https := "https://" + strings.TrimPrefix(server.url, "http://")
	client = startRemoteClient(t, https, "")
	out = client.call("remote:peer/demo/dev/coder", 5000)
	if !strings.Contains(out, "error_code") || strings.Contains(out, e2eToken) {
		t.Fatalf("an https remote must be refused:\n%s", out)
	}
}

func TestRemoteE2ELocalProfileMessageIsUnchangedWithoutRemotes(t *testing.T) {
	p := newPMFixture(t, nil)
	out := p.send(t, "alpha-1")
	if !strings.Contains(out, "pm_") && !strings.Contains(out, "answered") {
		t.Fatalf("local message:\n%s", out)
	}
	if strings.Contains(p.message.Definition.Description, "remote:") {
		t.Fatalf("a session without remotes must not mention them:\n%s", p.message.Definition.Description)
	}
}
