package native

import (
	"path/filepath"
	"testing"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

// Start, finish, stop and close detached runs: the run goroutine, its context and the
// waiters of every run must be gone afterwards.
func TestSubagentRunCyclesLeaveNoGoroutines(t *testing.T) {
	manager := NewSubagentManager()
	release := make(chan struct{})
	catalog, _ := buildSubagentCatalogOver(t, manager, newTestAsyncHost(t), blockedLauncher(release, nil))
	cycle := func() {
		stopped := runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":0}`))
		asyncCall(t, catalog, `{"action":"stop","id":"`+stopped+`"}`)
		asyncWait(t, catalog)
		runIDOf(t, subagentCall(t, catalog, `{"task":"y","block_ms":0}`))
		release <- struct{}{}
		asyncWait(t, catalog)
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
		host, err := NewAsyncHost(filepath.Join(t.TempDir(), ".async"), asyncmgr.DefaultConfig(), nil)
		if err != nil {
			t.Fatal(err)
		}
		manager := NewSubagentManager()
		catalog, _ := buildSubagentCatalogOver(t, manager, host, blockedLauncher(make(chan struct{}), nil))
		for range 2 {
			subagentCall(t, catalog, `{"task":"x","block_ms":0}`)
		}
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		host.Close()
	}
	requireNoGrowth(t, before, "20 managers closed with detached runs")
}

func buildSubagentCatalogOver(t *testing.T, manager *SubagentManager, host *AsyncHost, launcher SubagentLauncher) (*tools.Catalog, tools.Tool) {
	t.Helper()
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), parent, nil, launcher, nil, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	return catalogWithAsync(t, host, parent, tool), tool
}
