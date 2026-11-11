package native

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/tools"
)

// R5c follow-up: a sub-agent run id is unique only in its manager, so two managers can hold the
// same one. A run's lineage key is unique in the process; a stop cascade and a handle stop reach
// only the work of the run they meant.

// sameIDManager is a manager whose every run draws the same id.
func sameIDManager(t *testing.T, id string) *SubagentManager {
	t.Helper()
	manager := NewSubagentManager()
	manager.newRunID = func() (string, error) { return id, nil }
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func TestRunLineageKeysAreUniqueAcrossManagersWithTheSameRunID(t *testing.T) {
	const id = "sub_0000beef"
	parent := withExecutors(t, readOnlyTool("read"))
	// The worker manager holds the work two same-id runs left going, one under each run's key.
	worker := NewSubagentManager()
	t.Cleanup(func() { _ = worker.Close() })
	workerHost := newTestAsyncHost(t)
	workerTool, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), parent, nil, blockedLauncher(make(chan struct{}), nil), nil, worker, workerHost)
	if err != nil {
		t.Fatal(err)
	}
	workerCatalog := catalogWithAsync(t, workerHost, parent, workerTool)
	// What the resolver's cascade does: stop the work under the key in every manager.
	cascade := func(key, reason string) {
		workerHost.Manager().CancelLineage(keyIs(key), reason)
		worker.CancelUnmanagedLineage(keyIs(key), reason)
	}
	type side struct {
		manager *SubagentManager
		catalog *tools.Catalog
		lineage chan []string
	}
	start := func() side {
		manager := sameIDManager(t, id)
		manager.SetStopCascade(cascade)
		host := newTestAsyncHost(t)
		lineage := make(chan []string, 1)
		launcher := &fakeLauncher{run: func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
			lineage <- RunLineageOf(ctx)
			<-ctx.Done()
			return SubagentResult{}, ctx.Err()
		}}
		tool, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), parent, nil, launcher, nil, manager, host)
		if err != nil {
			t.Fatal(err)
		}
		catalog := catalogWithAsync(t, host, parent, tool)
		if got := runIDOf(t, subagentCall(t, catalog, `{"task":"t","agent":"explore","block_ms":0}`)); got != id {
			t.Fatalf("run id %s, want %s", got, id)
		}
		return side{manager, catalog, lineage}
	}
	a, b := start(), start()
	lineageA, lineageB := <-a.lineage, <-b.lineage
	keyA, keyB := a.manager.byID(id).lineageKey, b.manager.byID(id).lineageKey
	if keyA == keyB || keyA == id || !strings.Contains(keyA, id) {
		t.Fatalf("keys %q and %q: want two keys that name the id and differ", keyA, keyB)
	}
	workA := runIDOf(t, subagentCallWith(t, WithRunLineage(context.Background(), lineageA), workerCatalog, `{"task":"a","agent":"explore","block_ms":0}`))
	workB := runIDOf(t, subagentCallWith(t, WithRunLineage(context.Background(), lineageB), workerCatalog, `{"task":"b","agent":"explore","block_ms":0}`))

	asyncCall(t, a.catalog, `{"action":"stop","id":"`+id+`"}`)
	eventually(t, "A's run to end stopped", func() bool { return a.manager.byID(id).view().state == subagentRunStopped })
	// The cascade ran before A's run was reported ended, so what it reached is decided now.
	eventually(t, "A's work to end", func() bool { return worker.byID(workA).view().state == subagentRunStopped })
	if view := b.manager.byID(id).view(); view.state != subagentRunRunning {
		t.Fatalf("B's run = %+v, want running: A's stop is not B's", view)
	}
	if run := worker.byID(workB); run.view().state != subagentRunRunning || run.stopAsked {
		t.Fatalf("B's work = %+v, want running and never asked: it is under B's key, not A's", run.view())
	}
}

// A handle stop on a host that reaches two managers stops the run that is the handle, never a
// run of the other manager that drew the same id and is not one.
func TestAHandleStopNeverStopsAnotherManagersRunOfTheSameID(t *testing.T) {
	const id = "sub_0000cafe"
	parent := withExecutors(t, readOnlyTool("read"))
	host := newTestAsyncHost(t)
	other := sameIDManager(t, id)
	host.attachSubagent(other) // first, so a lookup by id alone finds its run first
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	otherTool, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), parent, nil, blockedLauncher(release, nil), nil, other, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherCatalog, err := tools.NewCatalog(append(append([]tools.Tool(nil), parent...), otherTool))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_b", "name": "subagent", "arguments": `{"task":"b","agent":"explore"}`})
		_, _ = otherCatalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: testSessionKey})
	}()
	eventually(t, "the other manager's run to start", func() bool {
		run := other.byID(id)
		return run != nil && run.view().state == subagentRunRunning
	})
	manager := sameIDManager(t, id)
	tool, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), parent, nil, blockedLauncher(release, nil), nil, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog := catalogWithAsync(t, host, parent, tool)
	if got := runIDOf(t, subagentCall(t, catalog, `{"task":"a","agent":"explore","block_ms":0}`)); got != id {
		t.Fatalf("run id %s, want %s", got, id)
	}
	asyncCall(t, catalog, `{"action":"stop","id":"`+id+`"}`)
	eventually(t, "the handle's run to end stopped", func() bool { return manager.byID(id).view().state == subagentRunStopped })
	if run := other.byID(id); run.view().state != subagentRunRunning || run.stopAsked {
		t.Fatalf("the other manager's run = %+v, want running and never asked", run.view())
	}
}

// Two managers of one host that start a handle of the same id at once: the second is refused as
// a duplicate (as Register would refuse it) without ending the first one's starting mark, so a
// stop that reaches the first meanwhile is still kept for its run.
func TestASecondStartingHandleOfTheSameIDKeepsTheFirstOnesMark(t *testing.T) {
	host := newTestAsyncHost(t)
	first := &subagentAsyncHandle{host: host, owner: testSessionKey, label: "a"}
	host.beginSubagentStart("sub_0000f00d")
	if err := first.begin("sub_0000f00d", "subagent:sub_0000f00d#2", false); err == nil {
		t.Fatal("a second handle of a starting id was registered")
	}
	(&subagentDriver{host: host}).Stop("sub_0000f00d", "user_stop")
	if reason, stopped := host.endSubagentStart("sub_0000f00d"); !stopped || reason != "user_stop" {
		t.Fatalf("the first handle kept %q %v, want user_stop", reason, stopped)
	}
}
