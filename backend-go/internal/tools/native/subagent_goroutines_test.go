package native

import (
	"testing"

	"jad/backend-go/internal/tools"
)

// Start, finish, stop and close detached runs: the run goroutine, its context and the
// waiters of every run must be gone afterwards.
func TestSubagentRunCyclesLeaveNoGoroutines(t *testing.T) {
	manager := NewSubagentManager()
	release := make(chan struct{})
	catalog, _ := buildSubagentCatalogOver(t, manager, blockedLauncher(release, nil))
	cycle := func() {
		stopped := runIDOf(t, subagentCall(t, catalog, `{"task":"x","wait_ms":0}`))
		subagentCall(t, catalog, `{"id":"`+stopped+`","stop":true,"wait_ms":2000}`)
		finished := runIDOf(t, subagentCall(t, catalog, `{"task":"y","wait_ms":0}`))
		release <- struct{}{}
		subagentCall(t, catalog, `{"id":"`+finished+`","wait_ms":2000}`)
	}
	cycle()
	before := baselineCount()
	for index := 0; index < 30; index++ {
		cycle()
	}
	requireNoGrowth(t, before, "30 subagent start/finish/stop cycles")
}

func TestSubagentManagerCloseWithDetachedRunsLeavesNoGoroutines(t *testing.T) {
	before := baselineCount()
	for index := 0; index < 20; index++ {
		manager := NewSubagentManager()
		catalog, _ := buildSubagentCatalogOver(t, manager, blockedLauncher(make(chan struct{}), nil))
		for range 2 {
			subagentCall(t, catalog, `{"task":"x","wait_ms":0}`)
		}
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
	}
	requireNoGrowth(t, before, "20 managers closed with detached runs")
}

func buildSubagentCatalogOver(t *testing.T, manager *SubagentManager, launcher SubagentLauncher) (*tools.Catalog, tools.Tool) {
	t.Helper()
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := NewSubagentToolWithManager(subagentSettings(agentNamed("explore")), parent, launcher, nil, manager)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	return catalog, tool
}
