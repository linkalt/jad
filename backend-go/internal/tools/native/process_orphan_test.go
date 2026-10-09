package native

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

// crashedProcess is a process that a crashed server left running. It starts a real sleep in its own process group, as a managed
// process is, and writes the manifest and owner record a crashed server would have left.
type crashedProcess struct {
	dir string
	id  string
	cmd *exec.Cmd
	// reaped closes when the process has ended and been waited for, as init would do
	// for a real orphan; until then a signalled process would linger as a zombie.
	reaped chan struct{}
}

// reapInBackground waits for cmd so a signalled process does not stay a zombie.
func reapInBackground(cmd *exec.Cmd) chan struct{} {
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	return reaped
}

func leaveCrashedProcess(t *testing.T, owner *serverIdentity, mutate func(*crashedProcess, *processOwnerRecord)) crashedProcess {
	t.Helper()
	return leaveCrashedProcessIn(t, owner, mutate, true)
}

// leaveCrashedProcessIn is leaveCrashedProcess with a choice: leader false leaves the
// process in this test's own process group, so it does not lead one.
func leaveCrashedProcessIn(t *testing.T, owner *serverIdentity, mutate func(*crashedProcess, *processOwnerRecord), leader bool) crashedProcess {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the start-time check this relies on is Linux only")
	}
	cmd := exec.Command("sleep", "120")
	if leader {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := reapInBackground(cmd)
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		<-reaped
	})
	dir := filepath.Join(t.TempDir(), "processes")
	crashed := crashedProcess{dir: dir, id: "proc_orphan", cmd: cmd, reaped: reaped}
	if err := os.MkdirAll(filepath.Join(dir, crashed.id), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := ProcessManifest{SchemaVersion: 1, ID: crashed.id, State: ProcessRunning, CWD: "/w", StartedAt: "2025-01-01T00:00:00.000Z", PID: cmd.Process.Pid}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, crashed.id, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The server that started the process is gone: a pid that no process holds.
	gone := serverIdentity{PID: 2_000_000_000}
	if owner != nil {
		gone = *owner
	}
	record := processOwnerRecord{Server: gone, Start: processStartTime(cmd.Process.Pid)}
	if mutate != nil {
		mutate(&crashed, &record)
	}
	if record.Server.PID > 0 {
		encoded, _ := json.Marshal(record)
		if err := os.WriteFile(filepath.Join(dir, crashed.id, processOwnerFile), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return crashed
}

func (c crashedProcess) recover(t *testing.T, stop bool) ProcessSnapshot {
	t.Helper()
	manager := NewProcessManager(c.dir)
	manager.SetOrphanPolicy(stop, 300*time.Millisecond)
	if err := manager.Recover(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	snapshot, found := manager.Snapshot(c.id)
	if !found {
		t.Fatal("the recovered process is unknown")
	}
	return *snapshot
}

func (c crashedProcess) alive() bool {
	select {
	case <-c.reaped:
		return false
	case <-time.After(100 * time.Millisecond):
		return true
	}
}

func TestRecoverStopsAProcessACrashedServerLeftRunning(t *testing.T) {
	crashed := leaveCrashedProcess(t, nil, nil)
	snapshot := crashed.recover(t, true)
	if snapshot.State != ProcessLost || snapshot.StopReason != processReasonRestartedStopped {
		t.Errorf("recovered = %s/%s, want lost/%s", snapshot.State, snapshot.StopReason, processReasonRestartedStopped)
	}
	if crashed.alive() {
		t.Error("the orphan is still running")
	}
}

func TestRecoverLeavesTheOrphanWhenStoppingIsOff(t *testing.T) {
	crashed := leaveCrashedProcess(t, nil, nil)
	snapshot := crashed.recover(t, false)
	if snapshot.State != ProcessLost || snapshot.StopReason != processReasonRestarted {
		t.Errorf("recovered = %s/%s, want lost/%s", snapshot.State, snapshot.StopReason, processReasonRestarted)
	}
	if !crashed.alive() {
		t.Error("the process was signalled although stop_orphans is off")
	}
}

func TestRecoverNeverSignalsWhatItCannotProve(t *testing.T) {
	live, _ := otherProcess(t)
	cases := map[string]struct {
		owner  *serverIdentity
		mutate func(*crashedProcess, *processOwnerRecord)
	}{
		"no owner record":                 {mutate: func(_ *crashedProcess, r *processOwnerRecord) { r.Server.PID = 0 }},
		"owner server still running":      {owner: &live},
		"pid reused (start time differs)": {mutate: func(_ *crashedProcess, r *processOwnerRecord) { r.Start++ }},
		"no start time recorded":          {mutate: func(_ *crashedProcess, r *processOwnerRecord) { r.Start = 0 }},
		"recorded by this very process":   {owner: &serverIdentity{PID: os.Getpid(), Start: processStartTime(os.Getpid())}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			crashed := leaveCrashedProcess(t, tc.owner, tc.mutate)
			checkNotSignalled(t, crashed)
		})
	}
}

func checkNotSignalled(t *testing.T, crashed crashedProcess) {
	t.Helper()
	{
		{
			snapshot := crashed.recover(t, true)
			if snapshot.StopReason == processReasonRestartedStopped {
				t.Errorf("reported stopped: %s", snapshot.StopReason)
			}
			if snapshot.State != ProcessLost {
				t.Errorf("state = %s, want lost", snapshot.State)
			}
			if !crashed.alive() {
				t.Error("a process that could not be proven an orphan was signalled")
			}
		}
	}
}

// A process that does not lead its own group is never signalled, even when its pid, its
// start time and its owner record all match: a group signal would reach others. It is
// the one case the table above cannot build, because the group is chosen at start.
func TestRecoverNeverSignalsAProcessThatDoesNotLeadItsGroup(t *testing.T) {
	checkNotSignalled(t, leaveCrashedProcessIn(t, nil, nil, false))
}

func TestRecoverForcesAnOrphanThatIgnoresTheFirstSignal(t *testing.T) {
	crashed := leaveShellOrphan(t, "proc_stubborn", stubbornScript)
	snapshot := crashed.recover(t, true)
	if snapshot.StopReason != processReasonRestartedStopped {
		t.Errorf("stop reason = %s, want %s (the forced signal)", snapshot.StopReason, processReasonRestartedStopped)
	}
	if crashed.alive() {
		t.Error("the orphan that ignores SIGTERM survived")
	}
}

func TestStartedProcessLeavesAnOwnerRecordOutsideTheManifest(t *testing.T) {
	f := newProcessFixture(t)
	id := f.start(t, "", "sleep 30")
	data, err := os.ReadFile(filepath.Join(f.dir, "processes", id, processOwnerFile))
	if err != nil {
		t.Fatalf("no owner record: %v", err)
	}
	var record processOwnerRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Server.PID != os.Getpid() || record.Start == 0 {
		t.Errorf("record = %+v, want this server's pid and the process start time", record)
	}
	manifest, err := os.ReadFile(filepath.Join(f.dir, "processes", id, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifest), "owner") || strings.Contains(string(manifest), "server") {
		t.Errorf("the owner record leaked into the manifest the model sees:\n%s", manifest)
	}
}

func TestOwnerLimitRefusesAStartBeyondThePerChildCap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX only")
	}
	dir := t.TempDir()
	manager := NewProcessManager(filepath.Join(dir, "processes"))
	t.Cleanup(func() { _ = manager.Close() })
	start := func(owner string, limit int) error {
		request := processTestRequest(dir, "sleep 30", 1024)
		request.Owner, request.OwnerLimit = owner, limit
		request.Policy.MaxRunning = 10
		_, err := manager.Start(request)
		return err
	}
	for i := 0; i < 2; i++ {
		if err := start("sub_a", 2); err != nil {
			t.Fatalf("start %d within the limit: %v", i, err)
		}
	}
	if err := start("sub_a", 2); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("third start by the same child = %v, want a refusal naming the limit", err)
	}
	if err := start("sub_b", 2); err != nil {
		t.Errorf("another child is limited by the first one's processes: %v", err)
	}
	if err := start("", 2); err != nil {
		t.Errorf("the session's own start is limited: %v", err)
	}
	if err := start("sub_a", 0); err != nil {
		t.Errorf("a zero limit must mean no per-child limit: %v", err)
	}
}

func TestOwnerLimitFreesASlotWhenAProcessEnds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX only")
	}
	dir := t.TempDir()
	manager := NewProcessManager(filepath.Join(dir, "processes"))
	t.Cleanup(func() { _ = manager.Close() })
	request := processTestRequest(dir, "sleep 30", 1024)
	request.Owner, request.OwnerLimit = "sub_a", 1
	request.Policy.MaxRunning = 10
	first, err := manager.Start(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(request); err == nil {
		t.Fatal("the second start was not refused")
	}
	manager.Stop(first.ID, "test")
	if _, err := manager.Start(request); err != nil {
		t.Errorf("start after the first ended: %v", err)
	}
}

func TestSessionCapacityStillAppliesToChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX only")
	}
	dir := t.TempDir()
	manager := NewProcessManager(filepath.Join(dir, "processes"))
	t.Cleanup(func() { _ = manager.Close() })
	request := processTestRequest(dir, "sleep 30", 1024)
	request.Policy.MaxRunning = 1
	request.Owner, request.OwnerLimit = "sub_a", 5
	if _, err := manager.Start(request); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(request); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Errorf("second start = %v, want the session capacity refusal", err)
	}
}

// A child's tool call carries the cap from tools.native.subagent.process_max_running to
// the process manager: the real settings, sub-agent tool and process tool together.
func TestSubagentProcessCapReachesTheChildsProcessTool(t *testing.T) {
	f := newProcessFixture(t)
	var refusal string
	var started int
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		execution := request.Execution
		execution.WorkspaceRoot = f.dir
		for i := 0; i < 3; i++ {
			out, err := f.tool.Execute(ctx, map[string]any{"action": "start", "command": "sleep 30"}, execution)
			if err != nil {
				return SubagentResult{}, err
			}
			if strings.Contains(out, "process_id: ") {
				started++
				continue
			}
			refusal = out
		}
		return SubagentResult{Text: "done", Rounds: 1, Tokens: 1}, nil
	}}
	settings := writeSettings(writerAgent("runner", "read", "process"))
	settings.ProcessMaxRunning = 2
	process := f.tool
	process.ReadOnly = false
	parent := withExecutors(t, readOnlyTool("read"))
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	tool, err := NewSubagentToolWithProcesses(settings, append(parent, process), launcher, nil, manager, f.manager)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(append(parent, process), tool))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"serve"}`)
	if started != 2 || !strings.Contains(refusal, "limit") {
		t.Errorf("started %d processes, refusal = %q; want 2 started and the third refused naming the limit", started, refusal)
	}
}

func TestRecoverGivesTheOrphanTimeToHandleTheFirstSignal(t *testing.T) {
	crashed := leaveShellOrphan(t, "proc_graceful", gracefulScript)
	snapshot := crashed.recover(t, true)
	if snapshot.StopReason != processReasonRestartedStopped {
		t.Errorf("stop reason = %s, want %s", snapshot.StopReason, processReasonRestartedStopped)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(crashed.dir), "cleaned-up")); err != nil {
		t.Errorf("the orphan was not given the polite signal first: %v", err)
	}
}

// kill_grace_ms of the settings is the time an orphan gets between the two signals.
func TestConfiguredProcessManagerGivesTheOrphanTheKillGrace(t *testing.T) {
	crashed := leaveShellOrphan(t, "proc_graceful_cfg", gracefulScript)
	manager := NewConfiguredProcessManager(sessionfs.NativeProcessSettings{StorageDir: crashed.dir, StopOrphans: true, KillGraceMs: 1000})
	if err := manager.Recover(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := os.Stat(filepath.Join(filepath.Dir(crashed.dir), "cleaned-up")); err != nil {
		t.Errorf("with a kill grace of 1000 ms the orphan must get time to clean up: %v", err)
	}
}

// A process that is signalled but is still there afterwards is not reported as stopped.
// A zombie is that case: it exists until its parent waits for it, whatever is sent to it.
func TestRecoverDoesNotReportAnOrphanThatIsStillThereAsStopped(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	cmd := exec.Command("sleep", "120")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	start := processStartTime(pid)
	_ = syscall.Kill(pid, syscall.SIGKILL) // dies, but is not waited for: a zombie
	t.Cleanup(func() { _ = cmd.Wait() })
	dir := filepath.Join(t.TempDir(), "processes")
	_ = os.MkdirAll(filepath.Join(dir, "proc_zombie"), 0o755)
	manifest := ProcessManifest{SchemaVersion: 1, ID: "proc_zombie", State: ProcessRunning, CWD: "/w", StartedAt: "2025-01-01T00:00:00.000Z", PID: pid}
	data, _ := json.Marshal(manifest)
	_ = os.WriteFile(filepath.Join(dir, "proc_zombie", "manifest.json"), data, 0o600)
	record, _ := json.Marshal(processOwnerRecord{Server: serverIdentity{PID: 2_000_000_000}, Start: start})
	_ = os.WriteFile(filepath.Join(dir, "proc_zombie", processOwnerFile), record, 0o600)
	crashed := crashedProcess{dir: dir, id: "proc_zombie"}
	snapshot := crashed.recover(t, true)
	if snapshot.State != ProcessLost || snapshot.StopReason != processReasonRestarted {
		t.Errorf("recovered = %s/%s, want lost/%s: the process is still there", snapshot.State, snapshot.StopReason, processReasonRestarted)
	}
}

// leaveShellOrphan starts a shell running script in its own process group, waits until
// it has created the ready file, and leaves the manifest and owner record of a crashed
// server for it.
func leaveShellOrphan(t *testing.T, id, script string) crashedProcess {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	cmd := exec.Command("/bin/sh", "-c", ": > "+ready+"; "+strings.ReplaceAll(script, "{dir}", dir))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := reapInBackground(cmd)
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-reaped })
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the shell never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	crashed := crashedProcess{dir: filepath.Join(dir, "processes"), id: id, cmd: cmd, reaped: reaped}
	_ = os.MkdirAll(filepath.Join(crashed.dir, id), 0o755)
	manifest := ProcessManifest{SchemaVersion: 1, ID: id, State: ProcessRunning, CWD: "/w", StartedAt: "2025-01-01T00:00:00.000Z", PID: cmd.Process.Pid}
	data, _ := json.Marshal(manifest)
	_ = os.WriteFile(filepath.Join(crashed.dir, id, "manifest.json"), data, 0o600)
	record, _ := json.Marshal(processOwnerRecord{Server: serverIdentity{PID: 2_000_000_000}, Start: processStartTime(cmd.Process.Pid)})
	_ = os.WriteFile(filepath.Join(crashed.dir, id, processOwnerFile), record, 0o600)
	return crashed
}

const (
	// stubbornScript ignores the polite signal, so only the forced one ends it.
	stubbornScript = "trap '' TERM; while :; do sleep 1; done"
	// gracefulScript cleans up when asked politely, and writes a marker to show it.
	gracefulScript = "trap 'echo done > {dir}/cleaned-up; exit 0' TERM; while :; do sleep 0.05; done"
)

// The manager the resolver builds from the settings carries the orphan policy.
func TestConfiguredProcessManagerAppliesTheOrphanPolicy(t *testing.T) {
	for _, stop := range []bool{true, false} {
		crashed := leaveCrashedProcess(t, nil, nil)
		manager := NewConfiguredProcessManager(sessionfs.NativeProcessSettings{StorageDir: crashed.dir, StopOrphans: stop, KillGraceMs: 300})
		if err := manager.Recover(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = manager.Close() })
		if got := !crashed.alive(); got != stop {
			t.Errorf("stop_orphans=%v: the orphan was stopped = %v", stop, got)
		}
	}
}
