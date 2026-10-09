package native

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

// processFixture is a real process manager and a real process tool, as a child that
// lists process would get them.
type processFixture struct {
	manager *ManagedProcessManager
	tool    tools.Tool
	dir     string
}

func newProcessFixture(t *testing.T) processFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	dir := t.TempDir()
	manager := NewProcessManager(dir + "/processes")
	t.Cleanup(func() { _ = manager.Close() })
	settings := processTestRequest(dir, "", 1024).Policy
	settings.StorageDir = dir + "/processes"
	tool, err := NewProcessTool(settings, manager, "KEY")
	if err != nil {
		t.Fatal(err)
	}
	return processFixture{manager: manager, tool: tool, dir: dir}
}

// call runs the process tool as the given owner ("" is the session itself).
func (f processFixture) call(t *testing.T, owner string, arguments map[string]any) string {
	t.Helper()
	out, err := f.tool.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "c1", WorkspaceRoot: f.dir, ProcessOwner: owner})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f processFixture) start(t *testing.T, owner, command string) string {
	t.Helper()
	out := f.call(t, owner, map[string]any{"action": "start", "command": command})
	for _, line := range strings.Split(out, "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), "process_id: "); ok {
			return id
		}
	}
	t.Fatalf("start printed no process id:\n%s", out)
	return ""
}

func (f processFixture) state(t *testing.T, id string) ProcessState {
	t.Helper()
	snapshot, found := f.manager.Snapshot(id)
	if !found {
		t.Fatalf("process %s is unknown", id)
	}
	return snapshot.State
}

func TestProcessToolOwnerSeesOnlyItsOwnProcesses(t *testing.T) {
	f := newProcessFixture(t)
	parent := f.start(t, "", "sleep 30")
	child := f.start(t, "sub_one", "sleep 30")

	list := f.call(t, "sub_one", map[string]any{"action": "list"})
	if !strings.Contains(list, child) || strings.Contains(list, parent) {
		t.Errorf("the child's list must hold its process and not the parent's:\n%s", list)
	}
	whole := f.call(t, "", map[string]any{"action": "list"})
	if !strings.Contains(whole, child) || !strings.Contains(whole, parent) {
		t.Errorf("the session's own list is not limited:\n%s", whole)
	}
	for _, action := range []string{"status", "wait", "stop"} {
		out := f.call(t, "sub_one", map[string]any{"action": action, "id": parent, "timeout_ms": 1})
		if !strings.Contains(out, "process_not_found") {
			t.Errorf("%s of the parent's process by a child = %s", action, out)
		}
	}
	if got := f.state(t, parent); got != ProcessRunning {
		t.Errorf("the parent's process is %s after the child tried to stop it", got)
	}
}

func TestProcessToolOwnerCannotSeeAnotherChildsProcess(t *testing.T) {
	f := newProcessFixture(t)
	first := f.start(t, "sub_one", "sleep 30")
	out := f.call(t, "sub_two", map[string]any{"action": "stop", "id": first})
	if !strings.Contains(out, "process_not_found") {
		t.Errorf("a second child stopped the first one's process:\n%s", out)
	}
	if got := f.state(t, first); got != ProcessRunning {
		t.Errorf("process is %s", got)
	}
}

func TestProcessToolOwnerCanStopItsOwnProcess(t *testing.T) {
	f := newProcessFixture(t)
	id := f.start(t, "sub_one", "sleep 30")
	f.call(t, "sub_one", map[string]any{"action": "stop", "id": id})
	if got := f.state(t, id); got != ProcessStopped {
		t.Errorf("the child's own stop left the process %s", got)
	}
}

func TestProcessToolWithoutAnOwnerStaysUnlimited(t *testing.T) {
	f := newProcessFixture(t)
	child := f.start(t, "sub_one", "sleep 30")
	out := f.call(t, "", map[string]any{"action": "status", "id": child})
	if strings.Contains(out, "process_not_found") {
		t.Errorf("the session cannot see a child's process:\n%s", out)
	}
}

func TestStopOwnedStopsOnlyThatOwnersRunningProcesses(t *testing.T) {
	f := newProcessFixture(t)
	parent := f.start(t, "", "sleep 30")
	mine := f.start(t, "sub_one", "sleep 30")
	other := f.start(t, "sub_two", "sleep 30")
	if got := f.manager.stopOwned("sub_one", "test"); got != 1 {
		t.Fatalf("stopOwned = %d, want 1", got)
	}
	if f.state(t, mine) != ProcessStopped || f.state(t, parent) != ProcessRunning || f.state(t, other) != ProcessRunning {
		t.Errorf("states: mine=%s parent=%s other=%s", f.state(t, mine), f.state(t, parent), f.state(t, other))
	}
	if got := f.manager.stopOwned("", "test"); got != 0 {
		t.Errorf("an empty owner stopped %d processes; the session's own are not a child's to clean up", got)
	}
	if f.state(t, parent) != ProcessRunning {
		t.Error("the session's process was stopped through an empty owner")
	}
}

func TestProcessOwnerIsNotInTheManifestTheModelSees(t *testing.T) {
	f := newProcessFixture(t)
	id := f.start(t, "sub_secret_owner", "sleep 30")
	status := f.call(t, "sub_secret_owner", map[string]any{"action": "status", "id": id})
	if strings.Contains(status, "sub_secret_owner") {
		t.Errorf("the owner leaked into the result:\n%s", status)
	}
	data, err := json.Marshal(mustSnapshot(t, f, id))
	if err != nil || strings.Contains(string(data), "sub_secret_owner") {
		t.Errorf("the owner leaked into the manifest: %s %v", data, err)
	}
}

func mustSnapshot(t *testing.T, f processFixture, id string) *ProcessSnapshot {
	t.Helper()
	snapshot, found := f.manager.Snapshot(id)
	if !found {
		t.Fatal("unknown process")
	}
	return snapshot
}

// processAgentCatalog builds the subagent tool over a real process manager and a stub
// process tool, so a run that lists process is wired as the resolver wires it.
func processAgentCatalog(t *testing.T, f processFixture, launcher SubagentLauncher, agent sessionfs.NativeSubagentAgent) (*tools.Catalog, *SubagentManager) {
	t.Helper()
	process := f.tool
	process.ReadOnly = false
	parent := withExecutors(t, readOnlyTool("read"))
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	tool, err := NewSubagentToolWithProcesses(writeSettings(agent), append(parent, process), launcher, nil, manager, f.manager)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(append(parent, process), tool))
	if err != nil {
		t.Fatal(err)
	}
	return catalog, manager
}

func TestSubagentRunEndStopsTheProcessesTheChildStarted(t *testing.T) {
	f := newProcessFixture(t)
	parent := f.start(t, "", "sleep 30")
	var started string
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		if request.Execution.ProcessOwner == "" {
			t.Error("the child's execution context has no process owner")
		}
		// The child starts a server through the tool, as its model would.
		started = f.start(t, request.Execution.ProcessOwner, "sleep 30")
		return SubagentResult{Text: "done", Rounds: 1, Tokens: 1}, nil
	}}
	catalog, _ := processAgentCatalog(t, f, launcher, writerAgent("runner", "read", "process"))

	out := subagentCall(t, catalog, `{"task":"serve"}`)
	if started == "" {
		t.Fatalf("the child never ran:\n%s", out)
	}
	if got := f.state(t, started); got != ProcessStopped {
		t.Errorf("the child's process is %s after its run ended, want stopped", got)
	}
	if got := f.state(t, parent); got != ProcessRunning {
		t.Errorf("the parent's process is %s: a child's end must not touch it", got)
	}
	if !strings.Contains(out, "stopped_processes: 1") {
		t.Errorf("the result does not say a process was stopped:\n%s", out)
	}
}

func TestSubagentRunEndStopsProcessesAlsoWhenTheChildFails(t *testing.T) {
	f := newProcessFixture(t)
	var started string
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		started = f.start(t, request.Execution.ProcessOwner, "sleep 30")
		return SubagentResult{}, context.DeadlineExceeded
	}}
	catalog, _ := processAgentCatalog(t, f, launcher, writerAgent("runner", "read", "process"))
	subagentCall(t, catalog, `{"task":"serve"}`)
	if got := f.state(t, started); got != ProcessStopped {
		t.Errorf("a failed run left its process %s", got)
	}
}

func TestSubagentRunEndStopsProcessesWhenTheRunIsStopped(t *testing.T) {
	f := newProcessFixture(t)
	startedID := make(chan string, 1)
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		startedID <- f.start(t, request.Execution.ProcessOwner, "sleep 30")
		<-ctx.Done()
		return SubagentResult{}, ctx.Err()
	}}
	catalog, manager := processAgentCatalog(t, f, launcher, writerAgent("runner", "read", "process"))
	done := make(chan string, 1)
	go func() { done <- subagentCallAs(t, catalog, "alpha", `{"task":"serve"}`) }()
	id := <-startedID
	deadline := time.Now().Add(5 * time.Second)
	for manager.CancelOwner("alpha") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not end after the owner was cancelled")
	}
	if got := f.state(t, id); got != ProcessStopped {
		t.Errorf("a stopped run left its process %s", got)
	}
}

func TestSubagentRunWithoutProcessesReportsNoStoppedProcesses(t *testing.T) {
	f := newProcessFixture(t)
	launcher := &fakeLauncher{}
	catalog, _ := processAgentCatalog(t, f, launcher, writerAgent("runner", "read", "process"))
	out := subagentCall(t, catalog, `{"task":"look"}`)
	if strings.Contains(out, "stopped_processes") {
		t.Errorf("a run that started nothing reports stopped processes:\n%s", out)
	}
}

func TestSubagentAgentListingProcessCanChangeFiles(t *testing.T) {
	agent := writerAgent("runner", "read", "process")
	if !agent.ChangesFiles() {
		t.Fatal("an agent that lists process must count as one that changes files: it runs to its end and takes the checkpoint")
	}
	settings := writeSettings(agent)
	settings.AllowWrite = false
	_, err := NewSubagentTool(settings, withExecutors(t, readOnlyTool("read"), writeTool("process")), nil)
	if err == nil {
		t.Fatal("process was accepted for a child without allow_write")
	}
}

func TestOwnedProcessManagerRefusesStopAndWaitOfAnotherOwnersProcess(t *testing.T) {
	f := newProcessFixture(t)
	parent := f.start(t, "", "sleep 30")
	scoped, ok := scopeProcessManager(f.manager, "sub_one")
	if !ok {
		t.Fatal("a real process manager must be scopable")
	}
	if snapshot, found := scoped.Stop(parent, "test"); found || snapshot != nil {
		t.Errorf("Stop reached a process the owner did not start: %v %v", snapshot, found)
	}
	if got := f.state(t, parent); got != ProcessRunning {
		t.Errorf("the process is %s after a refused stop", got)
	}
	if waited := scoped.Wait(parent, 1); waited.Found || waited.Snapshot != nil {
		t.Errorf("Wait reached a process the owner did not start: %+v", waited)
	}
	if snapshot, found := scoped.SnapshotWithLogs(parent, ProcessStreamLimits{}, ProcessStreamLimits{}, 0, false); found || snapshot != nil {
		t.Errorf("SnapshotWithLogs reached a process the owner did not start")
	}
}

func TestStopOwnedCountsOnlyTheProcessesItStopped(t *testing.T) {
	f := newProcessFixture(t)
	ended := f.start(t, "sub_one", "true")
	f.manager.Wait(ended, 3000)
	if got := f.state(t, ended); got == ProcessRunning {
		t.Fatalf("the short process is still %s", got)
	}
	running := f.start(t, "sub_one", "sleep 30")
	if got := f.manager.stopOwned("sub_one", "test"); got != 1 {
		t.Errorf("stopOwned = %d, want 1: a process that had already ended was not stopped", got)
	}
	if got := f.state(t, running); got != ProcessStopped {
		t.Errorf("the running process is %s", got)
	}
}
