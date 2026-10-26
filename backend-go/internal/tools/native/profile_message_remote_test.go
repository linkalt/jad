package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const remoteSecret = "JADREMOTE_secret_0123456789abcdef0123456789"

// peerRun is a run on the fake peer that the test ends by hand.
type peerRun struct {
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	result  remoteapi.RunResult
	stopped []string
}

func newPeerRun() *peerRun               { return &peerRun{done: make(chan struct{})} }
func (r *peerRun) Done() <-chan struct{} { return r.done }
func (r *peerRun) Result() remoteapi.RunResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result
}
func (r *peerRun) finish(result remoteapi.RunResult) {
	r.once.Do(func() { r.mu.Lock(); r.result = result; r.mu.Unlock(); close(r.done) })
}
func (r *peerRun) Stop(reason string) {
	r.mu.Lock()
	r.stopped = append(r.stopped, reason)
	r.mu.Unlock()
	r.finish(remoteapi.RunResult{State: "stopped", Reason: reason, Text: "partial"})
}
func (r *peerRun) stops() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stopped...)
}

type peerBackend struct {
	mu       sync.Mutex
	messages []remoteapi.MessageRequest
	runs     []*peerRun
	err      error
}

func (b *peerBackend) Profiles(context.Context, string) ([]string, error) {
	return []string{"review/reviewer"}, nil
}
func (b *peerBackend) SendMessage(_ context.Context, r remoteapi.MessageRequest) (remoteapi.Run, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	b.messages = append(b.messages, r)
	run := newPeerRun()
	b.runs = append(b.runs, run)
	return run, nil
}
func (b *peerBackend) StartSubagent(context.Context, remoteapi.SubagentRequest) (remoteapi.Run, error) {
	return nil, remoteapi.ErrUnknownTarget
}
func (b *peerBackend) run(t *testing.T, index int) *peerRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		if index < len(b.runs) {
			run := b.runs[index]
			b.mu.Unlock()
			return run
		}
		b.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the peer never started run %d", index)
	return nil
}

// peer is a fake remote JAD: the real remote API over a fake backend, behind TLS.
type peer struct {
	backend *peerBackend
	server  *httptest.Server
	api     *remoteapi.Server
	handler *switchHandler
	dir     string
}

// switchHandler lets a test cut the connection or answer with something else.
type switchHandler struct {
	mu     sync.Mutex
	next   http.Handler
	cut    atomic.Bool
	polled atomic.Int32
}

func (h *switchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cut.Load() {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
	}
	if strings.Contains(r.URL.Path, "/runs/") && r.Method == http.MethodGet {
		h.polled.Add(1)
	}
	h.mu.Lock()
	next := h.next
	h.mu.Unlock()
	next.ServeHTTP(w, r)
}

func startPeer(t *testing.T, grant string) *peer {
	t.Helper()
	sum := sha256.Sum256([]byte(remoteSecret))
	parsed, err := remoteapi.ParseTokens([]byte(fmt.Sprintf(`{"tokens":[{"id":"peer","sha256":%q,%s}]}`, hex.EncodeToString(sum[:]), grant)))
	if err != nil {
		t.Fatal(err)
	}
	backend := &peerBackend{}
	api, err := remoteapi.NewServer(remoteapi.Options{Tokens: remoteapi.StaticTokenTable(parsed, nil), Backend: backend, WaitMax: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	handler := &switchHandler{next: api}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(func() { server.Close(); api.Close() })
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := fmt.Sprintf(`{"kind":"remote","description":"peer","remote":{"url":%q,"token_env":"JAD_REMOTE_PEER","ca_file":%q,"timeout_ms":3000,"poll_ms":200,"lost_after_ms":1500,"retries":1,"default_session":"main"}}`, server.URL, ca)
	if err := os.WriteFile(filepath.Join(dir, "peer.json"), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	return &peer{backend: backend, server: server, api: api, handler: handler, dir: dir}
}

const peerGrant = `"profiles":["*/*/*"],"agents":["*/*/*/*"],"actions":["message","subagent","status","stop","transcripts"]`

type remoteMessageFixture struct {
	peer    *peer
	hub     *remote.Hub
	host    *AsyncHost
	message tools.Tool
	async   tools.Tool
}

func newRemoteMessageFixture(t *testing.T, grant string, mutate func(*sessionfs.NativeProfileMessageSettings)) *remoteMessageFixture {
	t.Helper()
	p := startPeer(t, grant)
	hub := remote.NewHub(func(name string) string {
		if name == "JAD_REMOTE_PEER" {
			return remoteSecret
		}
		return ""
	})
	t.Cleanup(hub.Close)
	host, err := NewAsyncHost(filepath.Join(t.TempDir(), ".async"), asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	settings := sessionfs.NativeProfileMessageSettings{TimeoutMs: 20_000, ResultMaxBytes: 1024, MaxQueued: 4, Remotes: []string{"peer"}}
	if mutate != nil {
		mutate(&settings)
	}
	message, err := NewProfileMessageToolWithRemotes(settings, []string{"reviewer"}, &controlMessenger{}, host, hub.Directory(p.dir))
	if err != nil {
		t.Fatal(err)
	}
	async, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	return &remoteMessageFixture{peer: p, hub: hub, host: host, message: message, async: async}
}

func (f *remoteMessageFixture) send(t *testing.T, target string, block int64, execution tools.ExecutionContext) string {
	t.Helper()
	if execution.CallID == "" {
		execution.CallID = "call"
	}
	out, err := f.message.Execute(context.Background(), map[string]any{"profile": target, "message": "check it", "block_ms": json.Number(fmt.Sprint(block))}, execution)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *remoteMessageFixture) asyncCall(t *testing.T, owner string, arguments map[string]any) string {
	t.Helper()
	out, err := f.async.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "async", SessionKey: owner})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

const remoteTarget = "remote:peer/main/review/reviewer"

var remoteCaller = tools.ExecutionContext{SessionKey: "s1:main"}

func TestRemoteProfileMessageAnswersWithinBlockMs(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, nil)
	go func() { f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "looks fine"}) }()
	out := f.send(t, remoteTarget, 5000, remoteCaller)
	if !strings.Contains(out, "looks fine") || !strings.Contains(out, "answered") {
		t.Fatalf("result:\n%s", out)
	}
	request := f.peer.backend.messages[0]
	if request.Session != "main" || request.Profile != "review/reviewer" || request.Message != "check it" || request.Hops != 1 || request.Caller != "remote:peer" {
		t.Fatalf("what the peer saw: %+v", request)
	}
	if strings.Contains(out, "transcript:") || strings.Contains(out, f.peer.dir) {
		t.Fatalf("a remote result must not name a local or remote file path:\n%s", out)
	}
}

func TestRemoteProfileMessageDefaultSessionAndExplicitSession(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, nil)
	go func() { f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "a"}) }()
	f.send(t, "remote:peer/review/reviewer", 5000, remoteCaller)
	go func() { f.peer.backend.run(t, 1).finish(remoteapi.RunResult{State: "done", Text: "b"}) }()
	f.send(t, "remote:peer/other/review/reviewer", 5000, remoteCaller)
	if got := []string{f.peer.backend.messages[0].Session, f.peer.backend.messages[1].Session}; got[0] != "main" || got[1] != "other" {
		t.Fatalf("sessions: %v", got)
	}
}

func TestRemoteProfileMessageBecomesAHandleAndWaitStatusStopWork(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, nil)
	out := f.send(t, remoteTarget, 0, remoteCaller)
	id := handleIDPattern.FindString(out)
	if id == "" || !strings.Contains(out, "is still working") {
		t.Fatalf("expected a handle:\n%s", out)
	}
	status := f.asyncCall(t, remoteCaller.SessionKey, map[string]any{"action": "status", "id": id})
	if !strings.Contains(status, "remote profile") || !strings.Contains(status, "running") {
		t.Fatalf("status:\n%s", status)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "the late answer"})
	}()
	waited := f.asyncCall(t, remoteCaller.SessionKey, map[string]any{"action": "wait", "ids": []any{id}, "wait_ms": json.Number("10000")})
	if !strings.Contains(waited, "the late answer") || !strings.Contains(waited, `"state":"done"`) {
		t.Fatalf("wait:\n%s", waited)
	}
}

func TestRemoteProfileMessageStopStopsTheRemoteRun(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, nil)
	out := f.send(t, remoteTarget, 0, remoteCaller)
	id := handleIDPattern.FindString(out)
	run := f.peer.backend.run(t, 0)
	stopped := f.asyncCall(t, remoteCaller.SessionKey, map[string]any{"action": "stop", "id": id})
	if !strings.Contains(stopped, `"state":"stopped"`) {
		t.Fatalf("stop:\n%s", stopped)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(run.stops()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(run.stops()) == 0 || run.stops()[0] != "remote_stop" {
		t.Fatalf("the remote run was not stopped: %v", run.stops())
	}
}

func TestRemoteProfileMessageConnectionDropBecomesLost(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, nil)
	out := f.send(t, remoteTarget, 0, remoteCaller)
	id := handleIDPattern.FindString(out)
	f.peer.handler.cut.Store(true)
	began := time.Now()
	waited := f.asyncCall(t, remoteCaller.SessionKey, map[string]any{"action": "wait", "ids": []any{id}, "wait_ms": json.Number("20000")})
	if !strings.Contains(waited, `"state":"lost"`) {
		t.Fatalf("a dropped connection must end the handle as lost:\n%s", waited)
	}
	if elapsed := time.Since(began); elapsed < time.Second || elapsed > 10*time.Second {
		t.Fatalf("lost after %v; lost_after_ms is 1500", elapsed)
	}
	if !strings.Contains(waited, "may still be running there") && !strings.Contains(waited, "lost") {
		t.Fatalf("the text must say the run may continue:\n%s", waited)
	}
}

func TestRemoteProfileMessageRecoversFromABriefDrop(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, nil)
	out := f.send(t, remoteTarget, 0, remoteCaller)
	id := handleIDPattern.FindString(out)
	f.peer.handler.cut.Store(true)
	time.Sleep(300 * time.Millisecond)
	f.peer.handler.cut.Store(false)
	f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "survived"})
	waited := f.asyncCall(t, remoteCaller.SessionKey, map[string]any{"action": "wait", "ids": []any{id}, "wait_ms": json.Number("20000")})
	if !strings.Contains(waited, "survived") {
		t.Fatalf("a drop shorter than lost_after_ms must not lose the run:\n%s", waited)
	}
}

func TestRemoteProfileMessageRefusals(t *testing.T) {
	cases := map[string]struct {
		grant, target, want string
		mutate              func(*sessionfs.NativeProfileMessageSettings)
	}{
		"remote not enabled for the session": {peerGrant, "remote:ghost/main/review/reviewer", "unknown_remote", nil},
		"token may not reach the profile":    {`"profiles":["main/dev/*"],"actions":["message","status","stop"]`, remoteTarget, "remote_forbidden", nil},
		"token may not message":              {`"profiles":["*/*/*"],"actions":["status"]`, remoteTarget, "remote_forbidden", nil},
		"malformed reference":                {peerGrant, "remote:peer/reviewer", "bad_remote_reference", nil},
		"path in the reference":              {peerGrant, "remote:peer/main/review/../etc", "bad_remote_reference", nil},
		"session with a colon":               {peerGrant, "remote:peer/local:main/review/reviewer", "bad_remote_reference", nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRemoteMessageFixture(t, c.grant, c.mutate)
			out := f.send(t, c.target, 5000, remoteCaller)
			if !strings.Contains(out, "error_code: "+c.want) {
				t.Fatalf("want %s:\n%s", c.want, out)
			}
			if len(f.peer.backend.messages) != 0 {
				t.Fatal("a refused message reached the peer's profile")
			}
		})
	}
}

func TestRemoteProfileMessageNotOfferedWithoutRemotesSetting(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, func(s *sessionfs.NativeProfileMessageSettings) { s.Remotes = nil })
	out := f.send(t, remoteTarget, 5000, remoteCaller)
	if !strings.Contains(out, "remote_unavailable") {
		t.Fatalf("a session that did not enable remotes must not reach one:\n%s", out)
	}
	if len(f.peer.backend.messages) != 0 {
		t.Fatal("a message was sent")
	}
	if strings.Contains(f.message.Definition.Description, "remote:") {
		t.Fatalf("the model is told about remotes it cannot use:\n%s", f.message.Definition.Description)
	}
}

func TestRemoteProfileMessageDescriptionListsTheRemotes(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, nil)
	if !strings.Contains(f.message.Definition.Description, "remote:<name>/<session>/<category>/<profile> (remotes: peer)") {
		t.Fatalf("description:\n%s", f.message.Definition.Description)
	}
}

func TestRemoteProfileMessageClientAllowListNarrows(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, nil)
	entry, err := os.ReadFile(filepath.Join(f.peer.dir, "peer.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	_ = json.Unmarshal(entry, &file)
	file["remote"].(map[string]any)["profiles"] = []string{"dev/*"}
	narrowed, _ := json.Marshal(file)
	if err := os.WriteFile(filepath.Join(f.peer.dir, "peer.json"), narrowed, 0o600); err != nil {
		t.Fatal(err)
	}
	out := f.send(t, remoteTarget, 5000, remoteCaller)
	if !strings.Contains(out, "remote_not_allowed") || len(f.peer.backend.messages) != 0 {
		t.Fatalf("the entry's profiles list must narrow what a model reaches:\n%s", out)
	}
}

func TestRemoteProfileMessageHopsAreSentAndTheRemoteLimitIsReported(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, nil)
	execution := remoteCaller
	execution.RemoteHops = 3
	out := f.send(t, remoteTarget, 5000, execution)
	if !strings.Contains(out, "remote_hop_limit") || !strings.Contains(out, "too many JAD servers") {
		t.Fatalf("hop limit:\n%s", out)
	}
	if len(f.peer.backend.messages) != 0 {
		t.Fatal("a message past the hop limit reached the peer's profile")
	}
	execution.RemoteHops = 2
	go func() { f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "ok"}) }()
	f.send(t, remoteTarget, 5000, execution)
	if f.peer.backend.messages[0].Hops != 3 {
		t.Fatalf("hops sent = %d, want the caller's 2 plus one", f.peer.backend.messages[0].Hops)
	}
}

func TestRemoteProfileMessageBusyAndTokenErrorsNeverContainTheToken(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, nil)
	f.peer.backend.err = remoteapi.ErrBusy
	out := f.send(t, remoteTarget, 5000, remoteCaller)
	if !strings.Contains(out, "profile_busy") {
		t.Fatalf("busy:\n%s", out)
	}
	f.peer.backend.err = nil
	wrong := newRemoteMessageFixture(t, peerGrant, nil)
	wrong.hub.Close()
	closedHub := remote.NewHub(func(string) string { return "JADREMOTE_other_0123456789abcdef0123456789abcd" })
	t.Cleanup(closedHub.Close)
	tool, err := NewProfileMessageToolWithRemotes(sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024, Remotes: []string{"peer"}}, []string{"reviewer"}, &controlMessenger{}, nil, closedHub.Directory(f.peer.dir))
	if err != nil {
		t.Fatal(err)
	}
	result, _ := tool.Execute(context.Background(), map[string]any{"profile": remoteTarget, "message": "x"}, tools.ExecutionContext{CallID: "c"})
	if !strings.Contains(result, "did not accept this server's token") {
		t.Fatalf("wrong token:\n%s", result)
	}
	for _, text := range []string{out, result} {
		if strings.Contains(text, remoteSecret) || strings.Contains(text, "JADREMOTE_other") || strings.Contains(text, "Bearer") {
			t.Fatalf("token in a result:\n%s", text)
		}
	}
}

func TestRemoteProfileMessageRunsInlineWithoutAHandleOwner(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, nil)
	go func() { f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "inline answer"}) }()
	out := f.send(t, remoteTarget, 5000, tools.ExecutionContext{})
	if !strings.Contains(out, "inline answer") {
		t.Fatalf("inline:\n%s", out)
	}
}

func TestRemoteProfileMessageLocalTimeoutStopsTheRemoteRun(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, func(s *sessionfs.NativeProfileMessageSettings) { s.TimeoutMs = 400 })
	out := f.send(t, remoteTarget, 0, remoteCaller)
	id := handleIDPattern.FindString(out)
	run := f.peer.backend.run(t, 0)
	waited := f.asyncCall(t, remoteCaller.SessionKey, map[string]any{"action": "wait", "ids": []any{id}, "wait_ms": json.Number("10000")})
	if !strings.Contains(waited, `"state":"timed_out"`) {
		t.Fatalf("timeout:\n%s", waited)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(run.stops()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(run.stops()) == 0 {
		t.Fatal("the remote run kept going after the local timeout")
	}
}

func TestRemoteProfileMessageCloseEndsFollowersWithoutLeaks(t *testing.T) {
	before := runtime.NumGoroutine()
	f := newRemoteMessageFixture(t, peerGrant, nil)
	for i := 0; i < 3; i++ {
		f.send(t, remoteTarget, 0, remoteCaller)
	}
	f.hub.Close()
	f.host.Close()
	f.peer.server.Close()
	f.peer.api.Close()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+4 {
		t.Fatalf("goroutines: %d before, %d after closing the hub", before, after)
	}
}

func TestRemoteConfigNamesMustHaveAFile(t *testing.T) {
	config := sessionfs.SessionConfig{SettingPath: "setting.json", RemotesDir: t.TempDir(), NativeProfileMessage: &sessionfs.NativeProfileMessageSettings{Remotes: []string{"nowhere"}}}
	hub := remote.NewHub(nil)
	defer hub.Close()
	_, _, err := remoteDirectory(config, remoteMessenger{controlMessenger: &controlMessenger{}, hub: hub})
	if err == nil || !strings.Contains(err.Error(), `names remote "nowhere"`) || !strings.Contains(err.Error(), "no file") {
		t.Fatalf("a remote name without a file must be a configuration error: %v", err)
	}
}

type remoteMessenger struct {
	*controlMessenger
	hub *remote.Hub
}

func (m remoteMessenger) RemoteDirectory(dir string) *remote.Directory { return m.hub.Directory(dir) }
