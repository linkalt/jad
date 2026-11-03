package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

var remoteRunHandle = regexp.MustCompile(`rs_[0-9a-f]{8}`)

const remoteAgent = "remote:peer/main/review/reviewer/explore"

// remoteSeamBound is how long a cap-slot test waits for a remote goroutine to reach its seam or
// finish. It is an upper bound: a passing run never waits for it.
const remoteSeamBound = 10 * time.Second

// remoteDrainBound bounds the cleanup wait for the manager's remote goroutines. The longest drain
// measured under -race is 20.0s (a run that stays open ends at RemoteTimeoutMs), and the bound is
// twice that.
const remoteDrainBound = 40 * time.Second

type remoteSubagentFixture struct {
	peer    *peer
	hub     *remote.Hub
	host    *AsyncHost
	tool    tools.Tool
	async   tools.Tool
	local   *fakeLauncher
	manager *SubagentManager
}

func newRemoteSubagentFixture(t *testing.T, grant string, mutate func(*sessionfs.NativeSubagentSettings)) *remoteSubagentFixture {
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
	settings := subagentSettings(agentNamed("explore"))
	settings.Remotes, settings.RemoteTimeoutMs, settings.ResultMaxBytes = []string{"peer"}, 20_000, 4096
	if mutate != nil {
		mutate(&settings)
	}
	local := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{Text: "local child"}, nil
	}}
	parent := withExecutors(t, readOnlyTool("read"))
	manager := NewSubagentManager()
	tool, err := newSubagentToolRemotes(settings, parent, nil, local, nil, manager, nil, host, hub.Directory(p.dir))
	if err != nil {
		t.Fatal(err)
	}
	async, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	// Registered first, so it runs last: every remote goroutine this fixture starts has returned
	// before the test does. A test that has already failed for any reason skips this wait, so a stuck
	// goroutine costs at most one drain bound here, and none when the test failed. Trade-off: in that
	// failing run a late goroutine can still read the next test's seam; it only changes the result of
	// an already red run.
	t.Cleanup(func() {
		if !t.Failed() {
			waitForRemoteEnds(t, manager)
		}
	})
	return &remoteSubagentFixture{peer: p, hub: hub, host: host, tool: tool, async: async, local: local, manager: manager}
}

func (f *remoteSubagentFixture) start(t *testing.T, agent string, block int64, execution tools.ExecutionContext) string {
	t.Helper()
	if execution.CallID == "" {
		execution.CallID = "call"
	}
	out, err := f.tool.Execute(context.Background(), map[string]any{"task": "find the answer", "agent": agent, "block_ms": json.Number(fmt.Sprint(block))}, execution)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *remoteSubagentFixture) asyncCall(t *testing.T, arguments map[string]any) string {
	t.Helper()
	out, err := f.async.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "async", SessionKey: remoteCaller.SessionKey})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRemoteSubagentAnswersWithinBlockMs(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	go func() { f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "the answer is 42"}) }()
	out := f.start(t, remoteAgent, 5000, remoteCaller)
	if !strings.Contains(out, "the answer is 42") || !strings.Contains(out, "finished") {
		t.Fatalf("result:\n%s", out)
	}
	request := f.peer.backend.subs[0]
	if request.Session != "main" || request.Profile != "review/reviewer" || request.Agent != "explore" || request.Task != "find the answer" || request.Hops != 1 || request.Caller != "remote:peer" {
		t.Fatalf("what the peer saw: %+v", request)
	}
	if f.local.count() != 0 {
		t.Fatal("a remote agent started a local child")
	}
	if strings.Contains(out, "transcript:") {
		t.Fatalf("a remote result must not name a transcript file:\n%s", out)
	}
}

func TestRemoteSubagentHandleWaitStatusAndStop(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	out := f.start(t, remoteAgent, 0, remoteCaller)
	id := remoteRunHandle.FindString(out)
	if id == "" || !strings.Contains(out, "still running") {
		t.Fatalf("expected a handle:\n%s", out)
	}
	if status := f.asyncCall(t, map[string]any{"action": "status", "id": id}); !strings.Contains(status, "remote agent") || !strings.Contains(status, "running") {
		t.Fatalf("status:\n%s", status)
	}
	if list := f.asyncCall(t, map[string]any{"action": "list"}); !strings.Contains(list, `"kind":"remote_subagent"`) {
		t.Fatalf("list:\n%s", list)
	}
	go func() {
		f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "found it"})
	}()
	waited := f.asyncCall(t, map[string]any{"action": "wait", "ids": []any{id}, "wait_ms": json.Number("10000")})
	if !strings.Contains(waited, "found it") || !strings.Contains(waited, `"state":"done"`) {
		t.Fatalf("wait:\n%s", waited)
	}

	second := remoteRunHandle.FindString(f.start(t, remoteAgent, 0, remoteCaller))
	run := f.peer.backend.run(t, 1)
	stopped := f.asyncCall(t, map[string]any{"action": "stop", "id": second})
	if !strings.Contains(stopped, `"state":"stopped"`) {
		t.Fatalf("stop:\n%s", stopped)
	}
	eventually(t, "the async stop to reach the remote run", func() bool { return len(run.stops()) > 0 })
}

func TestRemoteSubagentConnectionDropBecomesLost(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	id := remoteRunHandle.FindString(f.start(t, remoteAgent, 0, remoteCaller))
	f.peer.handler.cut.Store(true)
	waited := f.asyncCall(t, map[string]any{"action": "wait", "ids": []any{id}, "wait_ms": json.Number("20000")})
	if !strings.Contains(waited, `"state":"lost"`) || !strings.Contains(waited, "may still be running there") {
		t.Fatalf("a dropped connection must end the handle as lost:\n%s", waited)
	}
}

func TestRemoteSubagentLocalTimeoutStopsTheRemoteRun(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) { s.RemoteTimeoutMs = 400 })
	id := remoteRunHandle.FindString(f.start(t, remoteAgent, 0, remoteCaller))
	run := f.peer.backend.run(t, 0)
	waited := f.asyncCall(t, map[string]any{"action": "wait", "ids": []any{id}, "wait_ms": json.Number("10000")})
	if !strings.Contains(waited, `"state":"timed_out"`) {
		t.Fatalf("timeout:\n%s", waited)
	}
	eventually(t, "the remote run to be asked to stop after the local timeout", func() bool { return len(run.stops()) > 0 })
}

func TestRemoteSubagentRefusals(t *testing.T) {
	cases := map[string]struct{ grant, agent, want string }{
		"remote not listed for agents":  {peerGrant, "remote:ghost/main/review/reviewer/explore", "unknown_remote"},
		"token may not reach the agent": {`"profiles":["*/*/*"],"agents":["main/dev/*/*"],"actions":["subagent","status","stop"]`, remoteAgent, "remote_forbidden"},
		"token may not start agents":    {`"profiles":["*/*/*"],"agents":["*/*/*/*"],"actions":["message","status"]`, remoteAgent, "remote_forbidden"},
		"profile reference only":        {peerGrant, "remote:peer/main/review", "bad_remote_reference"},
		"path in the reference":         {peerGrant, "remote:peer/main/review/reviewer/..", "bad_remote_reference"},
		"unknown agent on the remote":   {peerGrant, "remote:peer/main/review/reviewer/ghost", "remote_unknown_target"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRemoteSubagentFixture(t, c.grant, nil)
			out := f.start(t, c.agent, 5000, remoteCaller)
			if !strings.Contains(out, "error_code: "+c.want) {
				t.Fatalf("want %s:\n%s", c.want, out)
			}
			if len(f.peer.backend.subs) != 0 {
				t.Fatal("a refused call reached the peer's agent")
			}
		})
	}
}

func TestRemoteSubagentIsOffUnlessTheSessionListsTheRemote(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) { s.Remotes = nil })
	out := f.start(t, remoteAgent, 5000, remoteCaller)
	if !strings.Contains(out, "remote_unavailable") || len(f.peer.backend.subs) != 0 {
		t.Fatalf("a session that did not enable remotes must not reach one:\n%s", out)
	}
	if strings.Contains(f.tool.Definition.Description, "remote:") {
		t.Fatalf("the model is told about remotes it cannot use:\n%s", f.tool.Definition.Description)
	}
}

func TestRemoteSubagentDescriptionNamesTheRemotes(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	if !strings.Contains(f.tool.Definition.Description, "remote:<name>/<session>/<category>/<profile>/<agent> (remotes: peer)") {
		t.Fatalf("description:\n%s", f.tool.Definition.Description)
	}
}

func TestRemoteSubagentNeverChangesLocalCheckpointOrLimits(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	if f.tool.MayChangeFiles(map[string]any{"task": "x", "agent": remoteAgent}) {
		t.Fatal("a remote agent changes files on the remote machine, not under this session's checkpoint")
	}
	if !f.tool.ReadOnly {
		t.Fatal("the tool stays read-only for local scheduling")
	}
	for i := 0; i < 5; i++ {
		f.start(t, remoteAgent, 0, remoteCaller)
	}
	if running := f.tool.Definition.Name; running == "" || f.local.count() != 0 {
		t.Fatal("remote starts must not use the local sub-agent manager's slots")
	}
}

func TestRemoteSubagentHopsAreSentAndLimited(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	execution := remoteCaller
	execution.RemoteHops = 3
	out := f.start(t, remoteAgent, 5000, execution)
	if !strings.Contains(out, "remote_hop_limit") || len(f.peer.backend.subs) != 0 {
		t.Fatalf("hop limit:\n%s", out)
	}
	execution.RemoteHops = 1
	go func() { f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "ok"}) }()
	f.start(t, remoteAgent, 5000, execution)
	if f.peer.backend.subs[0].Hops != 2 {
		t.Fatalf("hops = %d", f.peer.backend.subs[0].Hops)
	}
}

func TestRemoteSubagentLocalAgentsAreUntouched(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	out, err := f.tool.Execute(context.Background(), map[string]any{"task": "local work", "agent": "explore", "block_ms": json.Number("5000")}, tools.ExecutionContext{CallID: "c", SessionKey: "s1:main"})
	if err != nil || !strings.Contains(out, "local child") {
		t.Fatalf("a local agent still runs locally: %v\n%s", err, out)
	}
	if len(f.peer.backend.subs) != 0 {
		t.Fatal("a local agent call went to the remote")
	}
}

func TestRemoteSubagentCloseLeavesNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	for i := 0; i < 3; i++ {
		f.start(t, remoteAgent, 0, remoteCaller)
	}
	f.hub.Close()
	f.host.Close()
	f.peer.server.Close()
	f.peer.api.Close()
	eventually(t, fmt.Sprintf("goroutines to drop back to %d (%d before)", before+4, before), func() bool { return runtime.NumGoroutine() <= before+4 })
}

func TestRemoteSubagentConfigNamesMustHaveAFile(t *testing.T) {
	config := sessionfs.SessionConfig{SettingPath: "setting.json", RemotesDir: t.TempDir()}
	hub := remote.NewHub(nil)
	defer hub.Close()
	_, _, err := remoteDirectory(config, "subagent", []string{"nowhere"}, remoteMessenger{controlMessenger: &controlMessenger{}, hub: hub})
	if err == nil || !strings.Contains(err.Error(), "tools.native.subagent.remotes") {
		t.Fatalf("%v", err)
	}
}

func (f *fakeLauncher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func TestRemoteSubagentInlineCancelDoesNotWaitForAnUnreachableRemote(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		_, err := f.tool.Execute(ctx, map[string]any{"task": "find the answer", "agent": remoteAgent, "block_ms": json.Number("0")}, tools.ExecutionContext{CallID: "call"})
		returned <- err
	}()
	f.peer.backend.run(t, 0)
	f.peer.handler.cut.Store(true)
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(700 * time.Millisecond):
		t.Fatal("the cancelled inline call still waits for the remote (lost_after_ms is 1500)")
	}
}

func TestRemoteSubagentStartRefusedByAClosedHubStopsTheRemoteRun(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	f.peer.backend.onSubagentStart = f.hub.Close
	out := f.start(t, remoteAgent, 0, remoteCaller)
	if !strings.Contains(out, "shutting down") || !strings.Contains(out, "asked to stop") {
		t.Fatalf("result:\n%s", out)
	}
	if stops := f.peer.backend.run(t, 0).stops(); len(stops) != 1 {
		t.Fatalf("the remote run was asked to stop %d times, want 1", len(stops))
	}
}

// A remote agent is gated by the nesting depth the way a local one is, and by nothing else:
// the run caps count local runs only.
func TestRemoteSubagentStartHonoursMaxDepth(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) { s.MaxDepth = 2 })
	atMax := remoteCaller
	atMax.AgentDepth, atMax.RootRun = 2, "sub_00000001"
	out := f.start(t, remoteAgent, 0, atMax)
	if !strings.Contains(out, "subagent_depth_exceeded") || len(f.peer.backend.subs) != 0 {
		t.Fatalf("a remote start at max_depth reached the peer:\n%s", out)
	}
	below := remoteCaller
	below.AgentDepth, below.RootRun = 1, "sub_00000001"
	go func() { f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "ok"}) }()
	if out := f.start(t, remoteAgent, 5000, below); !strings.Contains(out, "finished") || len(f.peer.backend.subs) != 1 {
		t.Fatalf("a remote start one level below max_depth was refused:\n%s", out)
	}
}

func TestRemoteSubagentStartIsRefusedAtTheSessionRunCap(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) { s.MaxDepth, s.MaxRunsPerSession = 2, 1 })
	nested := remoteCaller
	nested.AgentDepth, nested.RootRun = 1, "sub_00000001"
	// The first run stays going: it is the one run the cap of 1 allows.
	go func() { f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "ok"}) }()
	f.start(t, remoteAgent, 0, nested)
	if out := f.start(t, remoteAgent, 0, nested); !strings.Contains(out, "subagent_limit_reached") {
		t.Fatalf("a remote start at the session cap was not refused with subagent_limit_reached:\n%s", out)
	}
}

func TestRemoteSubagentStartIsRefusedAtTheRootRunCap(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) { s.MaxDepth, s.MaxRunsPerRoot = 2, 1 })
	// The top-level run counts under its own id, as a local one does; the cap of 1 then admits
	// one nested run below it.
	f.manager.mu.Lock()
	f.manager.byRoot["sub_00000001"] = 1
	f.manager.mu.Unlock()
	nested := remoteCaller
	nested.AgentDepth, nested.RootRun = 1, "sub_00000001"
	// The first nested run stays going, so its place stays taken.
	first := make(chan *peerRun, 1)
	go func() { first <- f.peer.backend.run(t, 0) }()
	go func() { f.start(t, remoteAgent, 5000, nested) }()
	held := <-first
	if out := f.start(t, remoteAgent, 0, nested); !strings.Contains(out, "subagent_limit_reached") {
		t.Fatalf("a remote start at the root cap was not refused with subagent_limit_reached:\n%s", out)
	}
	held.finish(remoteapi.RunResult{State: "done", Text: "ok"})
}

func TestRemoteRunReleasesItsCapSlotOnEveryEndPath(t *testing.T) {
	ends := map[string]remoteapi.RunResult{
		"completion": {State: "done", Text: "ok"},
		"failure":    {State: "failed", Code: "profile_failed", Message: "boom"},
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) { s.MaxDepth, s.MaxRunsPerSession = 2, 1 })
			nested := remoteCaller
			nested.AgentDepth, nested.RootRun = 1, "sub_00000001"
			held := make(chan int, 1)
			go func() {
				run := f.peer.backend.run(t, 0)
				held <- f.manager.sessionRuns(remoteCaller.SessionKey)
				run.finish(end)
			}()
			f.start(t, remoteAgent, 5000, nested)
			if got := <-held; got != 1 {
				t.Fatalf("while the remote run was going the session counted %d runs, want 1", got)
			}
			if got := f.manager.sessionRuns(remoteCaller.SessionKey); got != 0 {
				t.Fatalf("the session still counts %d remote runs after the run ended, want 0", got)
			}
		})
	}
}

func TestRemoteRunReleasesItsCapSlotOnCancel(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) { s.MaxDepth, s.MaxRunsPerSession = 2, 1 })
	nested := remoteCaller
	nested.AgentDepth, nested.RootRun = 1, "sub_00000001"
	ctx, cancel := context.WithCancel(context.Background())
	held := make(chan *peerRun, 1)
	go func() { held <- f.peer.backend.run(t, 0) }()
	go func() {
		run := <-held
		cancel()
		run.finish(remoteapi.RunResult{State: "stopped", Reason: "caller_cancelled"})
	}()
	_, _ = f.tool.Execute(ctx, map[string]any{"task": "find the answer", "agent": remoteAgent, "block_ms": json.Number("5000")}, nested)
	// The place is held until the remote run ends, not until the call returns: poll for the end.
	deadline := time.Now().Add(5 * time.Second)
	for f.manager.sessionRuns(remoteCaller.SessionKey) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := f.manager.sessionRuns(remoteCaller.SessionKey); got != 0 {
		t.Fatalf("a cancelled remote start still counts %d runs after the remote run ended, want 0", got)
	}
}

// A second release of one reservation must not free a place that another run holds: with a cap of
// two, after the double release one place is still taken.
func TestReleaseRemoteTwiceDoesNotFreeAnotherRunsPlace(t *testing.T) {
	manager := NewSubagentManager()
	first, err := manager.reserveRemote("sub_00000001", remoteCaller.SessionKey, 0, 2, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.reserveRemote("sub_00000002", remoteCaller.SessionKey, 0, 2, 1, 2); err != nil {
		t.Fatal(err)
	}
	manager.releaseRemote(first)
	manager.releaseRemote(first)
	if got := manager.sessionRuns(remoteCaller.SessionKey); got != 1 {
		t.Fatalf("a double release left the session at %d runs, want 1 (the other run's place)", got)
	}
}

// The inline result and the async result name their remote run the same way: remote_run is
// "<name>/rr_…", and it names the run the call started (the remote's own id for it, which the
// peer hands out). The async handle stays the run key on both.
func TestRemoteInlineAndAsyncNameTheRemoteRunTheSameWay(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	go func() { f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "inline answer"}) }()
	inline := f.start(t, remoteAgent, 5000, remoteCaller)

	go func() { f.peer.backend.run(t, 1).finish(remoteapi.RunResult{State: "done", Text: "async answer"}) }()
	started := f.start(t, remoteAgent, 0, remoteCaller)
	for _, out := range []string{inline, started} {
		if ref := informationValue(out, "remote_run"); !regexp.MustCompile(`^peer/rr_[0-9a-f]{16}$`).MatchString(ref) {
			t.Fatalf("remote_run is %q, want peer/rr_<16 hex>:\n%s", ref, out)
		}
	}
	if informationValue(started, "run") == "" {
		t.Fatalf("the async result lost its run key:\n%s", started)
	}
}

// informationValue is the value of one information key of a rendered result, or "".
func informationValue(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), key+": "); ok {
			return value
		}
	}
	return ""
}

// A remote resume names the source as "<name>/rr_…", the form remote_run shows. A value of another
// form, or one that names another remote than the agent's, is refused before any request, with the
// expected form in the message.
func TestRemoteResumeRejectsAWrongFormBeforeAnyRequest(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	for name, resume := range map[string]string{
		"no name":      "rr_0123456789abcdef",
		"other name":   "other/rr_0123456789abcdef",
		"local id":     "sub_00000001",
		"no rr prefix": "peer/0123456789abcdef",
	} {
		t.Run(name, func(t *testing.T) {
			before := len(f.peer.backend.subs)
			out, err := f.tool.Execute(context.Background(), map[string]any{"task": "more", "agent": remoteAgent, "resume": resume}, remoteCaller)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "peer/rr_") {
				t.Fatalf("the refusal does not name the expected form peer/rr_…:\n%s", out)
			}
			if len(f.peer.backend.subs) != before {
				t.Fatalf("a request reached the peer for resume %q", resume)
			}
		})
	}
}

// A remote resume's result names its source and its config note, as a local resume's does: the
// peer's view carries both, the client decodes them from the wire, and the caller's result shows them.
func TestRemoteResumeResultNamesItsSourceAndConfigNote(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	go func() {
		f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "the answer is 42", ResumedFrom: "rr_0123456789abcdef", ConfigNote: "the agent changed since the source run"})
	}()
	out, err := f.tool.Execute(context.Background(), map[string]any{"task": "more", "agent": remoteAgent, "resume": "peer/rr_0123456789abcdef", "block_ms": json.Number("5000")}, remoteCaller)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"resumed_from: rr_0123456789abcdef", "config_note: the agent changed since the source run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the remote resume result lacks %q:\n%s", want, out)
		}
	}
}

// resumeRemote starts a resume of the remote run rr_0123456789abcdef with block_ms as given.
func (f *remoteSubagentFixture) resumeRemote(t *testing.T, block int64) string {
	t.Helper()
	out, err := f.tool.Execute(context.Background(), map[string]any{"task": "more", "agent": remoteAgent, "resume": "peer/rr_0123456789abcdef", "block_ms": json.Number(fmt.Sprint(block))}, remoteCaller)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRemoteResumeStoppedResultNamesItsSourceAndConfigNote(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	go func() {
		f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "stopped", Reason: "caller_cancelled", Text: "partial", ResumedFrom: "rr_0123456789abcdef", ConfigNote: "the agent changed since the source run"})
	}()
	out := f.resumeRemote(t, 5000)
	for _, want := range []string{"was stopped", "resumed_from: rr_0123456789abcdef", "config_note: the agent changed since the source run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the stopped remote resume result lacks %q:\n%s", want, out)
		}
	}
}

func TestRemoteResumeFailedResultNamesItsSourceAndConfigNote(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	go func() {
		f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "failed", Code: "profile_failed", Message: "boom", ResumedFrom: "rr_0123456789abcdef", ConfigNote: "the agent changed since the source run"})
	}()
	out := f.resumeRemote(t, 5000)
	for _, want := range []string{"subagent_failed", "resumed_from: rr_0123456789abcdef", "config_note: the agent changed since the source run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the failed remote resume result lacks %q:\n%s", want, out)
		}
	}
}

func TestRemoteResumeAsyncWaitNamesItsSourceAndConfigNote(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	id := remoteRunHandle.FindString(f.resumeRemote(t, 0))
	if id == "" {
		t.Fatal("the resume did not become a handle")
	}
	go func() {
		f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "the answer is 42", ResumedFrom: "rr_0123456789abcdef", ConfigNote: "the agent changed since the source run"})
	}()
	waited := f.asyncCall(t, map[string]any{"action": "wait", "ids": []any{id}, "wait_ms": json.Number("10000")})
	for _, want := range []string{`"resumed_from":"rr_0123456789abcdef"`, `"config_note":"the agent changed since the source run"`} {
		if !strings.Contains(waited, want) {
			t.Fatalf("the async wait of a remote resume lacks %s:\n%s", want, waited)
		}
	}
}

// newSyncRemoteTool is the fixture's tool with no async host: a call with a session is then not
// handled, so its reply comes from the sync branch.
func newSyncRemoteTool(t *testing.T, f *remoteSubagentFixture) tools.Tool {
	t.Helper()
	settings := subagentSettings(agentNamed("explore"))
	settings.Remotes, settings.RemoteTimeoutMs, settings.ResultMaxBytes = []string{"peer"}, 20_000, 4096
	settings.MaxDepth, settings.MaxRunsPerSession = 2, 1
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentToolRemotes(settings, parent, nil, f.local, nil, f.manager, nil, nil, f.hub.Directory(f.peer.dir))
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

// seam holds one remote goroutine at a seam var. reached closes when the goroutine enters the
// seam; exited closes once it has returned from the seam, so it no longer reads the var.
type seam struct {
	reached, exited, release chan struct{}
	reachedOnce, exitedOnce  sync.Once
	releaseOnce              sync.Once
}

func newSeam() *seam {
	return &seam{reached: make(chan struct{}), exited: make(chan struct{}), release: make(chan struct{})}
}

// hook is the function set on the seam var: it holds the goroutine until letGo.
func (s *seam) hook() {
	s.reachedOnce.Do(func() { close(s.reached) })
	<-s.release
	s.exitedOnce.Do(func() { close(s.exited) })
}

func (s *seam) letGo() { s.releaseOnce.Do(func() { close(s.release) }) }

// remoteSeamWait waits for ch within remoteSeamBound and reports whether it closed in time.
func remoteSeamWait(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(remoteSeamBound):
		return false
	}
}

// waitForRemoteEnds waits for the manager's remote goroutines to return, for at most remoteDrainBound.
// A goroutine still running after the bound is a missing Done or a run that never ended, so the test
// fails with a named message.
func waitForRemoteEnds(t *testing.T, manager *SubagentManager) {
	t.Helper()
	ended := make(chan struct{}, 1)
	go func() {
		manager.remoteEnds.Wait()
		ended <- struct{}{}
	}()
	select {
	case <-ended:
	case <-time.After(remoteDrainBound):
		t.Fatalf("a remote goroutine did not return within %v; a Done is missing or a run never ended", remoteDrainBound)
	}
}

// releaseSeamVars is the cleanup of a test that set seam vars. It lets every goroutine it held go,
// waits for the manager's remote goroutines to return, and clears the vars even when that wait fails
// (the deferred clear runs on the fatal exit). A goroutine returns after it has read its seam, so a
// clear after a successful wait cannot race a read; after a failed wait the failure is already reported.
func releaseSeamVars(t *testing.T, manager *SubagentManager, seams ...*seam) {
	defer func() {
		beforeRemoteBackgroundRelease = nil
		beforeRemoteAsyncComplete = nil
	}()
	for _, s := range seams {
		s.letGo()
	}
	waitForRemoteEnds(t, manager)
}

// The sync reply must come after the early release: the background release is held at its seam,
// so the place can only have been given back by the reply path, and the count is read at the reply.
func TestRemoteSyncRunGivesItsCapSlotBackBeforeTheReply(t *testing.T) {
	bg := newSeam()
	beforeRemoteBackgroundRelease = bg.hook
	f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) { s.MaxDepth, s.MaxRunsPerSession = 2, 1 })
	t.Cleanup(func() { releaseSeamVars(t, f.manager, bg) })
	f.tool = newSyncRemoteTool(t, f)
	nested := tools.ExecutionContext{AgentDepth: 1, RootRun: "sub_00000001", SessionKey: remoteCaller.SessionKey}
	go func() { f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "ok"}) }()
	f.start(t, remoteAgent, 0, nested)
	if !remoteSeamWait(bg.reached) {
		t.Fatal("the background release did not reach its seam")
	}
	if got := f.manager.sessionRuns(remoteCaller.SessionKey); got != 0 {
		t.Fatalf("the sync reply came back while the session still counts %d remote runs, want 0", got)
	}
}

// The async completion gives the place back before the handle completes: the background release is
// held, so only the completion can free the place, and the completion is held at its seam before
// Complete. The count is read while the handle is pending.
func TestRemoteAsyncRunGivesItsCapSlotBackBeforeItsHandleCompletes(t *testing.T) {
	bg := newSeam()
	done := newSeam()
	beforeRemoteBackgroundRelease = bg.hook
	beforeRemoteAsyncComplete = done.hook
	f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) { s.MaxDepth, s.MaxRunsPerSession = 2, 1 })
	t.Cleanup(func() { releaseSeamVars(t, f.manager, bg, done) })
	nested := remoteCaller
	nested.AgentDepth, nested.RootRun = 1, "sub_00000001"
	id := remoteRunHandle.FindString(f.start(t, remoteAgent, 0, nested))
	go func() { f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "found it"}) }()
	if !remoteSeamWait(done.reached) {
		t.Fatal("the async completion did not reach its seam")
	}
	if got := f.manager.sessionRuns(remoteCaller.SessionKey); got != 0 {
		t.Fatalf("the handle is pending while the session still counts %d remote runs, want 0", got)
	}
	bg.letGo()
	done.letGo()
	if waited := f.asyncCall(t, map[string]any{"action": "wait", "ids": []any{id}, "wait_ms": json.Number("10000")}); !strings.Contains(waited, "found it") {
		t.Fatalf("wait:\n%s", waited)
	}
}
