package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// The lifetime of a narrowing's MCP manager (mcpScopeHold): held by each run of the narrowing until
// the run and what it left going under its lineage key ended, closed by the last release.

// heldDriver is an async driver whose handles end only when the test completes them.
type heldDriver struct{}

func (heldDriver) Kind() asyncmgr.Kind { return "held_work" }
func (heldDriver) Stop(string, string) {}

// sendNarrowed sends review/reviewer a message from a caller whose sandbox is sandbox and waits
// for the message's run to end.
func sendNarrowed(t *testing.T, f *layoutFixture, sandbox *sessionfs.SandboxSettings) native.ProfileMessageOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(callerContext(t, f, context.Background()), time.Minute)
	defer cancel()
	plan, err := profileMessenger{resolver: f.service.resolver}.Prepare(ctx, native.ProfileMessageRequest{
		OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "look",
		Environment: &tools.Environment{Sandbox: sandbox}, CallerPlace: sessionfs.SandboxPlace{Workspace: f.cwd, CWD: f.cwd},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := plan.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.Done():
	case <-ctx.Done():
		t.Fatal("the message did not end")
	}
	return run.Outcome()
}

// A message run that leaves work going under its lineage keeps its narrowing's servers until that
// work ends: an unrelated live handle does not keep them, a second message narrowed the same way
// meanwhile shares them (no new start) and its own end does not close them, and the end of the
// first message's work closes them.
func TestANarrowingsMCPServersStayWhileTheMessagesWorkGoesAndStopOnceItEnds(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh for the command sandbox")
	}
	f := newLayoutFixture(t)
	logs := t.TempDir()
	starts, wrapped := filepath.Join(logs, "starts"), filepath.Join(logs, "wrapped")
	writeConfinedMCPTarget(t, f, starts)
	resolver := f.service.resolver
	host, err := resolver.AsyncHost(native.AsyncHostConfig{Dir: t.TempDir(), Config: asyncmgr.DefaultConfig()})
	if err != nil {
		t.Fatal(err)
	}
	manager := host.Manager()
	if err := manager.AddDriver(heldDriver{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(asyncmgr.Registration{ID: "unrelated", Owner: "elsewhere", Kind: "held_work", Lineage: []string{"session_run#0"}}); err != nil {
		t.Fatal(err)
	}
	var registered atomic.Bool
	resolver.runnerReturned = func(ctx context.Context) {
		// The first message's call leaves a handle going under the run's lineage.
		if registered.CompareAndSwap(false, true) {
			if err := manager.Register(asyncmgr.Registration{ID: "background", Owner: "elsewhere", Kind: "held_work", Lineage: native.RunLineageOf(ctx)}); err != nil {
				t.Error(err)
			}
		}
	}
	count := func(path string) int {
		data, _ := os.ReadFile(path)
		return strings.Count(string(data), "\n")
	}
	sandbox := loggingSandbox(wrapped, "caller", logs)

	if outcome := sendNarrowed(t, f, sandbox); outcome.Err != nil {
		t.Fatalf("first message: %+v", outcome)
	}
	if got := confinedManagers(resolver); got != 1 {
		t.Fatalf("%d managers of narrowings after the first message, want 1 (its work still goes)", got)
	}
	if outcome := sendNarrowed(t, f, sandbox); outcome.Err != nil {
		t.Fatalf("second message: %+v", outcome)
	}
	if got, in := count(starts), count(wrapped); got != 1 || in != 1 {
		t.Fatalf("%d starts, %d in the caller's sandbox; want 1, 1 (the second message shares the narrowing's server)", got, in)
	}
	if got := confinedManagers(resolver); got != 1 {
		t.Fatalf("%d managers of narrowings after the second message ended, want 1 (the first's work still holds it)", got)
	}
	if _, err := manager.Complete("unrelated", asyncmgr.Result{State: asyncmgr.StateDone}); err != nil {
		t.Fatal(err)
	}
	if got := confinedManagers(resolver); got != 1 {
		t.Fatalf("%d managers of narrowings after unrelated work ended, want 1", got)
	}
	if _, err := manager.Complete("background", asyncmgr.Result{State: asyncmgr.StateDone}); err != nil {
		t.Fatal(err)
	}
	if got := confinedManagers(resolver); got != 0 {
		t.Fatalf("%d managers of narrowings after the message's work ended, want 0", got)
	}
	if !waitFixtureProcesses(0) {
		t.Fatalf("%d fixture servers still run after the narrowing's last hold went", fixtureProcesses())
	}
	resolver.mcpDrains.mu.Lock()
	pending := len(resolver.mcpDrains.pending)
	resolver.mcpDrains.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d holds still wait for a drain", pending)
	}
}

// A narrowed run is told of the changes its own servers report, not of those of the profile's
// shared servers: the changes the target's own server reported after its last run stay for its
// next run, which is told of them, and the narrowed message run in between is not.
func TestANarrowedRunDoesNotTakeTheResourceChangesOfTheTargetsOwnServers(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh for the command sandbox")
	}
	f := newLayoutFixture(t)
	body, _ := json.Marshal(appStdioMCPServerFile(t, "", func(file map[string]any) {
		file["transport"].(map[string]any)["env"] = map[string]string{appStdioMCPEnvironment: "1", appStdioMCPTouchEnvironment: "1"}
		file["changes_note"] = true
	}))
	writeFile(t, filepath.Join(f.global, "mcp", "m", "stdio.json"), body)
	writeFile(t, filepath.Join(f.global, "tools", "review", "stdio.json"), []byte(`{"native":{},"mcp":{"m/stdio":{}}}`))
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"review/stdio"}`)
	f.captureBodies = true
	ownRun := func() string {
		t.Helper()
		f.mu.Lock()
		from := len(f.bodies)
		f.mu.Unlock()
		if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile=review%2Freviewer", `{"input":"go"}`); status != http.StatusOK || !strings.Contains(body, "event: completed") {
			t.Fatalf("own run: %d %s", status, body)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Join(f.bodies[from:], "\n")
	}

	// The target's own run touches its server, which reports a resource list change.
	f.script = scriptFunctionCall(mcpToolNameFor("stdio", "touch"), map[string]any{})
	ownRun()
	f.script = nil
	shared, err := f.service.resolver.sharedMCPManager(boundRunnerOf(t, f, "review/reviewer").config)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for events, _, _ := shared.ResourceEvents(0); len(events) == 0; events, _, _ = shared.ResourceEvents(0) {
		if time.Now().After(deadline) {
			t.Fatal("the target's server reported no resource change")
		}
		time.Sleep(20 * time.Millisecond)
	}

	f.mu.Lock()
	from := len(f.bodies)
	f.mu.Unlock()
	logs := t.TempDir()
	if outcome := sendNarrowed(t, f, loggingSandbox(filepath.Join(logs, "wrapped"), "caller", logs)); outcome.Err != nil {
		t.Fatalf("message: %+v", outcome)
	}
	f.mu.Lock()
	message := strings.Join(f.bodies[from:], "\n")
	f.mu.Unlock()
	if message == "" {
		t.Fatal("the message run sent no request")
	}
	if strings.Contains(message, "resource list changed") {
		t.Fatalf("the narrowed run was told the target's own server's change: %s", message)
	}
	if next := ownRun(); !strings.Contains(next, "resource list changed") {
		t.Fatalf("the target's next own run was not told its server's change: %s", next)
	}
}
