package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// A child that may nest has no checkpoint of its own to cover a grandchild's writes, and the call at
// the top is the only one the parent's checkpoint and ordered slot belong to. So an agent that may
// nest is judged by the agents it can reach, at every depth: it is a writer exactly when one is.
func TestAgentThatMayNestIsAWriterWhenAWriterIsReachable(t *testing.T) {
	f := newPlanMCPFixture(t)
	for _, agent := range []string{"nester", "mcponly", "plain", "fixer"} {
		call := subagentFunctionCall(`{"task":"x","agent":"` + agent + `"}`)
		if !f.catalog.CallMayChangeFiles(call) || !f.catalog.ChangesState(call) {
			t.Errorf("%s: CallMayChangeFiles %v ChangesState %v, want true for the checkpoint and ordered scheduling", agent, f.catalog.CallMayChangeFiles(call), f.catalog.ChangesState(call))
		}
	}
	reader := subagentFunctionCall(`{"task":"x","agent":"mcpread"}`)
	if f.catalog.CallMayChangeFiles(reader) || f.catalog.ChangesState(reader) {
		t.Error("a reader that cannot nest must stay a reader")
	}
}

// Without a writer to reach, or without room to nest, an agent that lists subagent stays a reader:
// legitimate read-only nesting is not serialized or checkpointed.
func TestAgentThatMayNestStaysAReaderWithNoWriterToReach(t *testing.T) {
	read := func(settings sessionfs.NativeSubagentSettings, mcp ...tools.Tool) *tools.Catalog {
		t.Helper()
		catalog, err := buildWithMCP(t, settings, &fakeLauncher{}, mcp, readOnlyTool("read"), writeTool("exec"))
		if err != nil {
			t.Fatal(err)
		}
		return catalog
	}
	nester := agentWithTools("nester", "read", "subagent")
	reader := agentWithTools("reader", "read")
	readers := subagentSettings(nester, reader)
	readers.MaxDepth = 3
	call := subagentFunctionCall(`{"task":"x","agent":"nester"}`)
	if catalog := read(readers); catalog.CallMayChangeFiles(call) {
		t.Error("a nesting agent with only readers to reach is a reader")
	}
	// ReadOnly MCP tools are readers too.
	if catalog := read(readers, withReadOnly(mcpStub(t, "zvec", "resources"))); catalog.CallMayChangeFiles(call) {
		t.Error("ReadOnly MCP tools do not make a nesting agent a writer")
	}
	// A writer exists, but max_depth 1 gives no child the subagent tool, so nothing can be reached.
	flat := subagentSettings(nester, agentWithTools("fixer", "read", "exec"))
	flat.MaxDepth = 1
	if catalog := read(flat); catalog.CallMayChangeFiles(call) {
		t.Error("an agent that cannot nest at max_depth 1 is judged by its own tools")
	}
	// With room and a writer it is a writer, and an MCP write tool is enough to be that writer.
	deep := subagentSettings(nester, reader)
	deep.MaxDepth = 3
	if catalog := read(deep, mcpStub(t, "zvec", "reindex")); catalog.CallMayChangeFiles(call) {
		// reader lists read only, so the MCP tool reaches nobody listed; the nester lists no MCP tool.
		t.Error("an MCP write tool no agent shows is not reachable")
	}
	deep.Agents = append(deep.Agents, agentWithTools("mcponly", "mcp:zvec:reindex"))
	if catalog := read(deep, mcpStub(t, "zvec", "reindex")); !catalog.CallMayChangeFiles(call) {
		t.Error("a nesting agent that can reach an MCP-capable writer must be a writer")
	}
}

// Plan mode refuses each call at its own depth, so a nesting reader still runs there and only its
// writer grandchildren are refused: the transitive classification is for scheduling and the
// checkpoint, not for plan mode.
func TestNestingReaderStillRunsInPlanModeAndItsWriterGrandchildIsRefused(t *testing.T) {
	f := newPlanMCPFixture(t)
	f.behave["nester"] = func(_ context.Context, request SubagentRequest) string {
		return childCall(t, context.Background(), request, "subagent", `{"task":"deeper","agent":"mcponly"}`)
	}
	plan, err := f.catalog.PlanMode(nil)
	if err != nil {
		t.Fatal(err)
	}
	call := subagentFunctionCall(`{"task":"x","agent":"nester"}`)
	if plan.CallMayChangeFiles(call) {
		t.Error("plan mode must keep the nesting reader's own call, so no checkpoint is taken for it")
	}
	callOutput(t, plan, call, planExecution)
	if !isPlanRefusal(f.outputs["nester"]) || f.startedAgents() != "nester" {
		t.Fatalf("grandchild in plan mode: %q started %q", f.outputs["nester"], f.startedAgents())
	}
}

// Deeper nesting: every level that may nest is judged the same way, so a child's own catalog, which
// schedules the grandchild calls it makes, counts the call that nests again as a change, and one to a
// reader that cannot nest as none.
func TestNestedCallsAreClassifiedAtEveryDepth(t *testing.T) {
	f := newPlanMCPFixture(t)
	seen := map[string]string{}
	classify := func(level string) func(context.Context, SubagentRequest) string {
		return func(_ context.Context, request SubagentRequest) string {
			for _, agent := range []string{"nester", "mcpread", "mcponly"} {
				call := subagentFunctionCall(`{"task":"deeper","agent":"` + agent + `"}`)
				seen[level+">"+agent] = fmt.Sprint(request.Tools.ChangesState(call))
			}
			return ""
		}
	}
	f.behave["nester"] = classify("child")
	f.behave["plain"] = func(ctx context.Context, request SubagentRequest) string {
		classify("grandchild")(ctx, request)
		return childCall(t, ctx, request, "subagent", `{"task":"deepest","agent":"nester"}`)
	}
	callOutput(t, f.catalog, subagentFunctionCall(`{"task":"x","agent":"plain"}`), planExecution)
	want := map[string]string{
		"grandchild>nester": "true", "grandchild>mcpread": "false", "grandchild>mcponly": "true",
		"child>nester": "true", "child>mcpread": "false", "child>mcponly": "true",
	}
	for key, value := range want {
		if seen[key] != value {
			t.Errorf("%s: ChangesState %q, want %q", key, seen[key], value)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("classified %v; the nested runs did not all happen (started %q)", seen, f.startedAgents())
	}
}

// remoteEntries writes entries beside the fixture's peer entry, named by the url they reach. The tool
// reads each entry once when it is built (remoteSnapshot), so a test that wants the tool to see them
// writes them first (newRemoteSubagentFixtureWith); one written later is an edit.
func (f *remoteSubagentFixture) remoteEntries(t *testing.T, urls map[string]string) {
	t.Helper()
	writeRemoteEntries(t, f.peer.dir, urls)
}

func writeRemoteEntries(t *testing.T, dir string, urls map[string]string) {
	t.Helper()
	for name, url := range urls {
		entry := fmt.Sprintf(`{"kind":"remote","description":%q,"remote":{"url":%q,"workspace":"w"}}`, name, url)
		if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(entry), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// newRemoteSubagentFixtureWith is newRemoteSubagentFixture with extra entries written before the tool is
// built; entries gets the url of the fixture's peer server.
func newRemoteSubagentFixtureWith(t *testing.T, entries func(peerURL string) map[string]string, mutate func(*sessionfs.NativeSubagentSettings)) *remoteSubagentFixture {
	t.Helper()
	p := startPeer(t, peerGrant)
	writeRemoteEntries(t, p.dir, entries(p.server.URL))
	return newRemoteSubagentFixtureOn(t, p, mutate)
}

// A remote on this machine (a loopback address or a unix socket) can be given the session's folder, so
// a call to it is a writer for scheduling and the checkpoint. A remote at another machine's address
// cannot change this machine's files as far as this host can tell, and stays a reader.
func TestRemoteAgentOnThisHostIsAWriterForSchedulingAndCheckpoint(t *testing.T) {
	f := newRemoteSubagentFixtureWith(t, func(peerURL string) map[string]string {
		return map[string]string{
			"near": peerURL, "far": "http://192.0.2.7:8080", "sock": "unix:///run/jad/peer.sock",
			"named": "http://localhost:8080", "v6": "http://[::1]:8080",
		}
	}, func(s *sessionfs.NativeSubagentSettings) {
		s.Remotes = []string{"peer", "near", "far", "sock", "named", "v6", "gone"}
	})
	for name, want := range map[string]bool{
		"near": true, "sock": true, "named": true, "v6": true, // this machine (the peer is placed on another by the fixture)
		"peer": false,
		"far":  false, // another machine
		"gone": true,  // no entry file: unknown is conservative
	} {
		arguments := map[string]any{"task": "x", "agent": "remote:" + name + "/main/review/reviewer/explore"}
		if got := f.tool.MayChangeFiles(arguments); got != want {
			t.Errorf("remote %s: MayChangeFiles = %v, want %v", name, got, want)
		}
	}
	if f.tool.MayChangeFiles(map[string]any{"task": " ", "agent": remoteAgent}) {
		t.Error("listing is not a write")
	}
	if !f.tool.ReadOnly {
		t.Fatal("the tool stays read-only for calls that do not change files")
	}
}

// A nesting reader that can reach a remote on this host is a writer, one that can reach only remotes of
// other machines (and no writer agent) is not.
func TestNestingReaderIsAWriterWhenItCanReachARemoteOnThisHost(t *testing.T) {
	for name, tc := range map[string]struct {
		remotes []string
		want    bool
	}{
		"loopback remote": {[]string{"near"}, true},
		"far remote":      {[]string{"far"}, false},
		"both":            {[]string{"far", "near"}, true},
		"no remote":       {nil, false},
	} {
		f := newRemoteSubagentFixtureWith(t, func(peerURL string) map[string]string {
			return map[string]string{"near": peerURL, "far": "http://192.0.2.7:8080"}
		}, func(s *sessionfs.NativeSubagentSettings) {
			s.Agents = []sessionfs.NativeSubagentAgent{agentWithTools("nester", "read", "subagent"), agentWithTools("reader", "read")}
			s.MaxDepth = 3
			s.Remotes = tc.remotes
		})
		if got := f.tool.MayChangeFiles(map[string]any{"task": "x", "agent": "nester"}); got != tc.want {
			t.Errorf("%s: MayChangeFiles = %v, want %v", name, got, tc.want)
		}
	}
}

// An explicit sandbox choice that asks for a worktree is refused for an agent that changes files
// through MCP tools alone, instead of running it in the session's own folder; a reader that has
// nothing to isolate still runs, as it did.
func TestWorktreeChoiceIsRefusedForAnMCPOnlyAgent(t *testing.T) {
	launcher := &fakeLauncher{}
	settings := choiceSettings(agentWithTools("mcponly", "mcp:zvec:reindex"), agentWithTools("reader", "read"), agentWithTools("fixer", "read", "exec"))
	catalog, err := buildWithMCP(t, settings, launcher, []tools.Tool{mcpStub(t, "zvec", "reindex")}, readOnlyTool("read"), writeTool("exec"))
	if err != nil {
		t.Fatal(err)
	}
	out := callOutput(t, catalog, subagentFunctionCall(`{"task":"x","agent":"mcponly","sandbox":"isolate"}`), planExecution)
	if !strings.Contains(out, sandboxUnavailableCode) || !strings.Contains(out, "worktree") || !strings.Contains(out, "MCP tools") {
		t.Fatalf("an MCP-only agent in a worktree choice must be refused clearly:\n%s", out)
	}
	launcher.mu.Lock()
	started := len(launcher.requests)
	launcher.mu.Unlock()
	if started != 0 {
		t.Fatalf("%d run(s) started in the session's folder after the refusal", started)
	}
	if out := callOutput(t, catalog, subagentFunctionCall(`{"task":"x","agent":"reader","sandbox":"isolate"}`), planExecution); strings.Contains(out, sandboxUnavailableCode) {
		t.Fatalf("a reader with nothing to isolate must still run:\n%s", out)
	}
	if out := callOutput(t, catalog, subagentFunctionCall(`{"task":"x","agent":"fixer","sandbox":"isolate"}`), planExecution); strings.Contains(out, sandboxUnavailableCode) {
		t.Fatalf("a writer with a change tool must still be isolated:\n%s", out)
	}
}

// A remote on this machine can change the files the call's checkpoint and ordered slot cover, so a call
// to it is never handed off: it waits for the remote run to end whatever block_ms says, and no handle
// outlives the call. A remote of another machine is handed off as it was.
func TestRemoteOnThisHostIsNeverHandedOff(t *testing.T) {
	f := newRemoteSubagentFixtureWith(t, func(peerURL string) map[string]string { return map[string]string{"near": peerURL} },
		func(s *sessionfs.NativeSubagentSettings) { s.Remotes = []string{"peer", "near"} })
	near := "remote:near/main/review/reviewer/explore"

	type reply struct {
		out string
		err error
	}
	replies := make(chan reply, 1)
	go func() {
		out, err := f.tool.Execute(context.Background(), map[string]any{"task": "edit", "agent": near, "block_ms": json.Number("0")}, tools.ExecutionContext{CallID: "near", SessionKey: remoteCaller.SessionKey})
		replies <- reply{out, err}
	}()
	run := f.peer.backend.run(t, 0)
	select {
	case got := <-replies:
		t.Fatalf("a call to a remote on this machine returned while the remote still runs (block_ms 0):\n%s", got.out)
	case <-time.After(400 * time.Millisecond):
	}
	if list := f.asyncCall(t, map[string]any{"action": "list"}); strings.Contains(list, "remote_subagent") {
		t.Fatalf("a remote on this machine left a handle:\n%s", list)
	}
	run.finish(remoteapi.RunResult{State: "done", Text: "edited on this machine"})
	select {
	case got := <-replies:
		if got.err != nil || !strings.Contains(got.out, "edited on this machine") || strings.Contains(got.out, "still running") {
			t.Fatalf("the call must end with the remote run's answer: %v\n%s", got.err, got.out)
		}
	case <-time.After(remoteSeamBound):
		t.Fatal("the call did not end with the remote run")
	}

	// The other machine's remote (the peer) is handed off at once, as before.
	out := f.start(t, remoteAgent, 0, remoteCaller)
	if !strings.Contains(out, "still running") || remoteRunHandle.FindString(out) == "" {
		t.Fatalf("a remote of another machine is handed off:\n%s", out)
	}
	f.peer.backend.run(t, 1).finish(remoteapi.RunResult{State: "done", Text: "far"})
}

// Cancelling the call to a remote on this machine stops the remote run: nothing is left running
// outside the checkpoint.
func TestRemoteOnThisHostIsStoppedWithItsCall(t *testing.T) {
	f := newRemoteSubagentFixtureWith(t, func(peerURL string) map[string]string { return map[string]string{"near": peerURL} },
		func(s *sessionfs.NativeSubagentSettings) { s.Remotes = []string{"near"} })
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := f.tool.Execute(ctx, map[string]any{"task": "edit", "agent": "remote:near/main/review/reviewer/explore", "block_ms": json.Number("0")}, tools.ExecutionContext{CallID: "near", SessionKey: remoteCaller.SessionKey})
		errs <- err
	}()
	run := f.peer.backend.run(t, 0)
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled call returned %v", err)
	}
	eventually(t, "the remote run on this machine to be stopped by the call's cancel", func() bool { return len(run.stops()) > 0 })
}

// The tool judges a remote by the snapshot of its entry taken when it was built, for scheduling, the
// checkpoint, the handle decision and the start alike. A remote on another machine whose entry file is
// edited to reach another address, this machine included, after the call was scheduled as a reader is
// refused at start, and nothing reaches the server: it is never run as a reader that may write here.
func TestRemoteEntryEditedBetweenSchedulingAndStartIsRefused(t *testing.T) {
	f := newRemoteSubagentFixtureWith(t, func(string) map[string]string { return map[string]string{"far": "http://192.0.2.7:8080"} },
		func(s *sessionfs.NativeSubagentSettings) { s.Remotes = []string{"far"} })
	far := "remote:far/main/review/reviewer/explore"
	call := map[string]any{"task": "edit", "agent": far, "block_ms": json.Number("0")}
	if f.tool.MayChangeFiles(call) {
		t.Fatal("a remote of another machine is scheduled as a reader")
	}
	// The entry is edited between scheduling and start: it now reaches the loopback server.
	f.remoteEntries(t, map[string]string{"far": f.peer.server.URL})
	if f.tool.MayChangeFiles(call) {
		t.Fatal("the classification changed with the file: scheduling and start would read different entries")
	}
	out, err := f.tool.Execute(context.Background(), call, tools.ExecutionContext{CallID: "moved", SessionKey: remoteCaller.SessionKey})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "error_code: remote_changed") {
		t.Fatalf("a call scheduled as a reader must be refused when its entry moved:\n%s", out)
	}
	f.peer.backend.mu.Lock()
	reached := len(f.peer.backend.subs) + len(f.peer.backend.runs)
	f.peer.backend.mu.Unlock()
	if reached != 0 {
		t.Fatalf("a refused call reached the server (%d requests)", reached)
	}
}

// A remote the snapshot put on this machine stays a writer whatever the file says later, and an edit
// that leaves the address alone does not refuse a call.
func TestRemoteEntryEditsThatAreSafeDoNotChangeTheCall(t *testing.T) {
	f := newRemoteSubagentFixtureWith(t, func(peerURL string) map[string]string {
		return map[string]string{"near": peerURL, "far": "http://192.0.2.7:8080"}
	},
		func(s *sessionfs.NativeSubagentSettings) { s.Remotes = []string{"peer", "near", "far"} })
	near := map[string]any{"task": "x", "agent": "remote:near/main/review/reviewer/explore"}
	if !f.tool.MayChangeFiles(near) {
		t.Fatal("a remote on this machine is a writer")
	}
	f.remoteEntries(t, map[string]string{"near": "http://192.0.2.7:8080"})
	if !f.tool.MayChangeFiles(near) {
		t.Fatal("an edit that moves a same-host remote away must not turn a scheduled writer into a reader mid-run")
	}
	// An edit of something other than the address (here the description) does not refuse the call.
	path := filepath.Join(f.peer.dir, "peer.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), `"description":"peer"`, `"description":"edited"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out := f.start(t, remoteAgent, 0, remoteCaller)
	if !strings.Contains(out, "still running") {
		t.Fatalf("an edit that keeps the address must not refuse the call:\n%s", out)
	}
	f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "ok"})
}
