package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/tools/native"
)

// A real background sub-agent the narrowed message run started keeps its narrowing's MCP manager
// (mcpScopeHold) alive after the run ended, calls the narrowing's server through it, and the
// manager and its server go once the child ended. releaseInRunnerReturned lets the child go on
// while the run itself ends (Resolver.runnerReturned, before the run's drain look), so the
// child's call and end race the run's release; otherwise the child is let go only after the
// message ended. Either way the child was registered under the run's lineage before the runner
// returned, which the test checks at that point.
func runNarrowedBackgroundChild(t *testing.T, releaseInRunnerReturned bool) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh for the command sandbox")
	}
	f := newLayoutFixture(t)
	logs := t.TempDir()
	starts, wrapped, out := filepath.Join(logs, "starts"), filepath.Join(logs, "wrapped"), filepath.Join(logs, "out.txt")
	writeConfinedMCPTarget(t, f, starts)
	writeFile(t, filepath.Join(f.root, "tools", "review", "stdio.json"), []byte(`{"native":{"async":{},"subagent":{"background_writers":true,"max_background_writers":2,`+
		`"agents":{"finder":{"description":"Uses the server."}}}},"mcp":{"m/stdio":{}}}`))
	resolver := f.service.resolver

	childStarted, release, answered := make(chan struct{}, 1), make(chan struct{}), make(chan string, 1)
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	lookup := mcpToolNameFor("stdio", "lookup")
	f.script = func(model, body string) string {
		last := lastInputItem(t, body)
		output := strings.Contains(last, "function_call_output")
		switch {
		case !output && strings.Contains(last, "use the server"):
			childStarted <- struct{}{}
			<-release
			return layoutCall("call-lookup", lookup, map[string]any{"word": "w", "target": out})
		case output && strings.Contains(last, "call-lookup"):
			answered <- last
			return ""
		case output:
			return ""
		default:
			return layoutCall("call-rsub", "subagent", map[string]any{"agent": "finder", "task": "use the server", "block_ms": 0})
		}
	}
	liveAtReturn := make(chan bool, 1)
	resolver.runnerReturned = func(ctx context.Context) {
		lineage := native.RunLineageOf(ctx)
		if len(lineage) == 0 {
			liveAtReturn <- false
			return
		}
		// The child was started by a call of the run, so it is live under the run's key before
		// the runner returned: the drain look that follows cannot miss it.
		liveAtReturn <- resolver.lineageLive(lineage[len(lineage)-1])
		if releaseInRunnerReturned {
			letGo()
		}
	}

	if outcome := sendNarrowed(t, f, loggingSandbox(wrapped, "caller", logs)); outcome.Err != nil {
		t.Fatalf("message: %+v", outcome)
	}
	awaitSignal(t, childStarted, "the background child's first request")
	if !<-liveAtReturn {
		t.Fatal("the background child was not live under the run's lineage when the runner returned")
	}
	if !releaseInRunnerReturned {
		if got := confinedManagers(resolver); got != 1 {
			t.Fatalf("%d managers of narrowings after the message ended, want 1 (its child still goes)", got)
		}
		if got := pendingDrains(resolver); got != 1 {
			t.Fatalf("%d holds wait for a drain, want 1 (the message run's)", got)
		}
		letGo()
	}
	var got string
	select {
	case got = <-answered:
	case <-time.After(time.Minute):
		t.Fatal("the child's server call was not answered")
	}
	if !strings.Contains(got, "[caller] MCP wrote") {
		t.Fatalf("the child's call after its parent ended answered %s, want the narrowing's server in the caller's sandbox", got)
	}
	count := func(path string) int {
		data, _ := os.ReadFile(path)
		return strings.Count(string(data), "\n")
	}
	if s, in := count(starts), count(wrapped); s != 1 || in != 1 {
		t.Fatalf("%d starts, %d in the caller's sandbox; want 1, 1 (the child calls the server its parent listed)", s, in)
	}
	deadline := time.Now().Add(30 * time.Second)
	for confinedManagers(resolver) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d managers of narrowings after the child ended, want 0", confinedManagers(resolver))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waitFixtureProcesses(0) {
		t.Fatalf("%d fixture servers still run after the child ended", fixtureProcesses())
	}
	resolver.mu.Lock()
	refs := len(resolver.mcpScopeRefs)
	resolver.mu.Unlock()
	if pending := pendingDrains(resolver); pending != 0 || refs != 0 {
		t.Fatalf("%d holds wait for a drain and %d hold counts are left, want 0 and 0", pending, refs)
	}
}

func pendingDrains(resolver *Resolver) int {
	resolver.mcpDrains.mu.Lock()
	defer resolver.mcpDrains.mu.Unlock()
	return len(resolver.mcpDrains.pending)
}

func TestABackgroundSubagentOfANarrowedRunKeepsItsMCPServersUntilItEnds(t *testing.T) {
	runNarrowedBackgroundChild(t, false)
}

func TestABackgroundSubagentEndingWhileItsNarrowedRunEndsReleasesTheServersOnce(t *testing.T) {
	runNarrowedBackgroundChild(t, true)
}
