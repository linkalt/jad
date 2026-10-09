package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools/native"
)

// orphan is a real sleep that a "crashed server" left running, with the manifest and
// owner record that server would have written.
type orphan struct {
	id     string
	dir    string
	cmd    *exec.Cmd
	reaped chan struct{}
}

func linuxStartTime(t *testing.T, pid int) uint64 {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	rest := strings.Fields(text[strings.LastIndex(text, ")")+2:])
	// Field 22 of stat; rest starts at field 3.
	value, err := strconv.ParseUint(rest[19], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// leaveOrphan starts a sleep in its own process group and writes its manifest and owner
// record into storage/<id>.
func leaveOrphan(t *testing.T, storage, id string) orphan {
	t.Helper()
	return leaveOrphanRunning(t, storage, id, "sleep", "120")
}

// leaveOrphanRunning is leaveOrphan for any command.
func leaveOrphanRunning(t *testing.T, storage, id, name string, args ...string) orphan {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the start-time proof the orphan stop relies on is Linux only")
	}
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		<-reaped
	})
	dir := filepath.Join(storage, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := native.ProcessManifest{SchemaVersion: 1, ID: id, State: native.ProcessRunning, CWD: "/w", StartedAt: "2025-01-01T00:00:00.000Z", PID: cmd.Process.Pid}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The server that started it is gone: a pid no process holds.
	owner := fmt.Sprintf(`{"server":{"pid":2000000000},"start":%d}`, linuxStartTime(t, cmd.Process.Pid))
	if err := os.WriteFile(filepath.Join(dir, "owner.json"), []byte(owner), 0o600); err != nil {
		t.Fatal(err)
	}
	return orphan{id: id, dir: dir, cmd: cmd, reaped: reaped}
}

func (o orphan) alive() bool {
	select {
	case <-o.reaped:
		return false
	default:
		return true
	}
}

// waitGone waits until the process has ended.
func (o orphan) waitGone(t *testing.T, limit time.Duration) bool {
	t.Helper()
	select {
	case <-o.reaped:
		return true
	case <-time.After(limit):
		return false
	}
}

// settledManifest waits for the manifest to leave the running state: Recover signals the
// process first and writes the manifest after, so the process can be gone a moment
// before the file says so.
func (o orphan) settledManifest(t *testing.T, limit time.Duration) native.ProcessManifest {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		manifest := o.manifest(t)
		if manifest.State != native.ProcessRunning || time.Now().After(deadline) {
			return manifest
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (o orphan) manifest(t *testing.T) native.ProcessManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(o.dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest native.ProcessManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

// recoveryFixture is a workspace with one session root. Each session is given a
// setting.json by the test.
type recoveryFixture struct {
	workspace string
	root      string
	roots     []sessionfs.Root
}

func newRecoveryFixture(t *testing.T) recoveryFixture {
	t.Helper()
	workspace := t.TempDir()
	root := filepath.Join(workspace, "sessions")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return recoveryFixture{workspace: workspace, root: root, roots: []sessionfs.Root{{ID: "local", Label: "Local", Directory: root}}}
}

func (f recoveryFixture) session(t *testing.T, name, setting string) {
	t.Helper()
	dir := filepath.Join(f.root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f recoveryFixture) resolver(t *testing.T) *Resolver {
	t.Helper()
	resolver, err := NewResolver(f.roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)
	return resolver
}

func (f recoveryFixture) processSetting(storage string) string {
	return fmt.Sprintf(`{"cwd":%q,"tools":{"native":{"process":{"shell":"/bin/sh","storage_dir":%q,"kill_grace_ms":200}}}}`, f.workspace, storage)
}

func TestRecoverProcessesStopsAnOrphanWithoutAnyRequest(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "procs")
	f.session(t, "demo", f.processSetting(storage))
	crashed := leaveOrphan(t, storage, "proc_a")

	report, err := f.resolver(t).RecoverProcesses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !crashed.waitGone(t, 3*time.Second) {
		t.Fatal("the orphan is still running after the start-time recovery")
	}
	manifest := crashed.settledManifest(t, 3*time.Second)
	if manifest.State != native.ProcessLost || manifest.StopReason != "backend_restarted_stopped" {
		t.Fatalf("manifest = state %q, stop_reason %q; want lost / backend_restarted_stopped", manifest.State, manifest.StopReason)
	}
	if report.Sessions != 1 || report.Storages != 1 || report.Unreadable != 0 || report.Failed != 0 {
		t.Fatalf("report = %+v, want 1 session and 1 storage", report)
	}
}

func TestRecoverProcessesLeavesNoNewDirectoryForASessionThatNeverStartedAProcess(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "never-created")
	f.session(t, "demo", f.processSetting(storage))

	report, err := f.resolver(t).RecoverProcesses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(storage); !os.IsNotExist(err) {
		t.Fatalf("stat(%s) error = %v; the scan must not create a storage directory", storage, err)
	}
	if report.Sessions != 1 || report.Storages != 0 {
		t.Fatalf("report = %+v, want 1 session and 0 storages", report)
	}
}

func TestRecoverProcessesSkipsSessionsWithoutTheProcessTool(t *testing.T) {
	f := newRecoveryFixture(t)
	f.session(t, "plain", fmt.Sprintf(`{"cwd":%q}`, f.workspace))
	// A directory with no setting.json is not a session.
	if err := os.MkdirAll(filepath.Join(f.root, "not-a-session"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := f.resolver(t).RecoverProcesses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Sessions != 1 || report.Storages != 0 || report.Unreadable != 0 {
		t.Fatalf("report = %+v, want 1 session, 0 storages, 0 unreadable", report)
	}
}

func TestRecoverProcessesSkipsABrokenSessionAndKeepsGoing(t *testing.T) {
	f := newRecoveryFixture(t)
	good := filepath.Join(f.workspace, "good-procs")
	f.session(t, "a-broken", `{ this is not json`)
	f.session(t, "b-good", f.processSetting(good))
	crashed := leaveOrphan(t, good, "proc_good")

	report, err := f.resolver(t).RecoverProcesses(context.Background())
	if err != nil {
		t.Fatalf("a broken session must not fail the scan: %v", err)
	}
	if !crashed.waitGone(t, 3*time.Second) {
		t.Fatal("the orphan of the good session was not stopped after a broken one")
	}
	if report.Unreadable != 1 || report.Storages != 1 {
		t.Fatalf("report = %+v, want 1 unreadable and 1 storage", report)
	}
}

// A profile that keeps its tools in its own tool_storage keeps its processes there too,
// so a scan of the active profile alone would miss them.
func TestRecoverProcessesVisitsEveryProfilesStorage(t *testing.T) {
	f := newRecoveryFixture(t)
	f.session(t, "demo", fmt.Sprintf(`{"cwd":%q,"tools":{"native":{"process":{"shell":"/bin/sh","kill_grace_ms":200}}},"paths":{"active_profile":"green","profiles":{"green":{"models":["model.json"]},"blue":{"models":["model.json"]}}}}`, f.workspace))
	blue := filepath.Join(f.workspace, ".jad", "profiles", "blue", "tools", ".processes")
	green := filepath.Join(f.workspace, ".jad", "profiles", "green", "tools", ".processes")
	inBlue := leaveOrphan(t, blue, "proc_blue")
	inGreen := leaveOrphan(t, green, "proc_green")

	report, err := f.resolver(t).RecoverProcesses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !inBlue.waitGone(t, 3*time.Second) || !inGreen.waitGone(t, 3*time.Second) {
		t.Fatalf("orphans left: blue alive=%v green alive=%v", inBlue.alive(), inGreen.alive())
	}
	if report.Storages != 2 {
		t.Fatalf("report = %+v, want 2 storages", report)
	}
}

// Two sessions that name one storage directory share one manager: it is recovered once.
func TestRecoverProcessesRecoversASharedStorageOnce(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "shared")
	f.session(t, "one", f.processSetting(storage))
	f.session(t, "two", f.processSetting(storage))
	crashed := leaveOrphan(t, storage, "proc_shared")

	report, err := f.resolver(t).RecoverProcesses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !crashed.waitGone(t, 3*time.Second) {
		t.Fatal("the shared orphan was not stopped")
	}
	if report.Storages != 1 {
		t.Fatalf("report = %+v, want the shared storage counted once", report)
	}
}

func TestRecoverProcessesHonoursStopOrphansOff(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "procs")
	f.session(t, "demo", fmt.Sprintf(`{"cwd":%q,"tools":{"native":{"process":{"shell":"/bin/sh","storage_dir":%q,"stop_orphans":false}}}}`, f.workspace, storage))
	crashed := leaveOrphan(t, storage, "proc_keep")

	if _, err := f.resolver(t).RecoverProcesses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if crashed.waitGone(t, 500*time.Millisecond) {
		t.Fatal("the orphan was stopped although stop_orphans is false")
	}
	manifest := crashed.settledManifest(t, 3*time.Second)
	if manifest.State != native.ProcessLost || manifest.StopReason != "backend_restarted" {
		t.Fatalf("manifest = state %q, stop_reason %q; want lost / backend_restarted", manifest.State, manifest.StopReason)
	}
}

func TestRecoverProcessesStopsWhenTheContextIsCancelled(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "procs")
	f.session(t, "demo", f.processSetting(storage))
	crashed := leaveOrphan(t, storage, "proc_cancelled")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.resolver(t).RecoverProcesses(ctx); err == nil {
		t.Fatal("RecoverProcesses with a cancelled context returned no error")
	}
	if crashed.waitGone(t, 300*time.Millisecond) {
		t.Fatal("a cancelled scan still stopped the orphan")
	}
}

func TestRecoverProcessesOnAClosedResolverFailsInsteadOfPanicking(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "procs")
	f.session(t, "demo", f.processSetting(storage))
	leaveOrphan(t, storage, "proc_closed")
	resolver := f.resolver(t)
	resolver.Close()
	if _, err := resolver.RecoverProcesses(context.Background()); err == nil {
		t.Fatal("RecoverProcesses on a closed resolver returned no error")
	}
}

// serviceConfig loads the fixture through LoadConfig, so the root setting key reaches
// the service the way it does in production. server is the JSON body of "server".
func serviceConfig(t *testing.T, f recoveryFixture, server string) Config {
	t.Helper()
	root := fmt.Sprintf(`{"session_roots":[{"id":"local","path":"sessions"}],"server":%s}`, server)
	if err := os.WriteFile(filepath.Join(f.workspace, "setting.json"), []byte(root), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir:  f.workspace,
		Environment: map[string]string{"AGENT_API_TOKEN": integrationAPIToken},
	})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	return config
}

func TestLoadConfigReadsTheRecoverProcessesSwitch(t *testing.T) {
	f := newRecoveryFixture(t)
	for _, c := range []struct {
		server string
		want   bool
	}{
		{`{}`, true},
		{`{"recover_processes_at_start":null}`, true},
		{`{"recover_processes_at_start":true}`, true},
		{`{"recover_processes_at_start":false}`, false},
	} {
		if got := serviceConfig(t, f, c.server).RecoverProcessesAtStart; got != c.want {
			t.Errorf("server %s: RecoverProcessesAtStart = %v, want %v", c.server, got, c.want)
		}
	}
}

func TestServiceRecoversOrphansAtStartInTheBackground(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "procs")
	f.session(t, "demo", f.processSetting(storage))
	crashed := leaveOrphan(t, storage, "proc_service")

	service, err := NewService(serviceConfig(t, f, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	// No request was sent: only the start-time scan can stop it.
	if !crashed.waitGone(t, 5*time.Second) {
		t.Fatal("the service did not stop the orphan at start")
	}
	if manifest := crashed.settledManifest(t, 3*time.Second); manifest.StopReason != "backend_restarted_stopped" {
		t.Fatalf("stop_reason = %q, want backend_restarted_stopped", manifest.StopReason)
	}
}

func TestServiceLeavesOrphansAloneWhenTheSwitchIsOff(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "procs")
	f.session(t, "demo", f.processSetting(storage))
	crashed := leaveOrphan(t, storage, "proc_off")

	service, err := NewService(serviceConfig(t, f, `{"recover_processes_at_start":false}`))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if crashed.waitGone(t, time.Second) {
		t.Fatal("the orphan was stopped although recover_processes_at_start is off")
	}
	if manifest := crashed.manifest(t); manifest.State != native.ProcessRunning {
		t.Fatalf("manifest state = %q, want running (untouched)", manifest.State)
	}
}

// Close must not hang on, or race with, a scan that is still going.
func TestServiceCloseEndsTheStartScan(t *testing.T) {
	f := newRecoveryFixture(t)
	for i := 0; i < 20; i++ {
		f.session(t, fmt.Sprintf("s%02d", i), f.processSetting(filepath.Join(f.workspace, fmt.Sprintf("p%02d", i))))
	}
	service, err := NewService(serviceConfig(t, f, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { service.Close(); service.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Service.Close did not return")
	}
}

// The scan must not delay the server. An orphan that ignores the polite signal makes its
// recovery last the whole kill grace; NewService has to return long before that.
func TestServiceStartDoesNotWaitForTheScan(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "procs")
	f.session(t, "demo", fmt.Sprintf(`{"cwd":%q,"tools":{"native":{"process":{"shell":"/bin/sh","storage_dir":%q,"kill_grace_ms":1500}}}}`, f.workspace, storage))
	stubborn := leaveOrphanRunning(t, storage, "proc_stubborn", "sh", "-c", `trap "" TERM; sleep 120`)
	// Let the shell install its trap before the scan signals it.
	time.Sleep(200 * time.Millisecond)

	config := serviceConfig(t, f, `{}`)
	began := time.Now()
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if took := time.Since(began); took > 700*time.Millisecond {
		t.Fatalf("NewService took %v; the scan must run in the background", took)
	}
	if !stubborn.alive() {
		t.Fatal("the stubborn orphan ended at once; the test no longer shows a slow scan")
	}
	if !stubborn.waitGone(t, 6*time.Second) {
		t.Fatal("the stubborn orphan was never stopped")
	}
}

// Cancelling ends the scan before the next session is even loaded: with no session
// having the process tool, the only effect of the cancel is the error it returns.
func TestRecoverProcessesChecksTheContextBeforeEachSession(t *testing.T) {
	f := newRecoveryFixture(t)
	f.session(t, "plain", fmt.Sprintf(`{"cwd":%q}`, f.workspace))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.resolver(t).RecoverProcesses(ctx); err == nil {
		t.Fatal("a cancelled scan over a session without the tool returned no error")
	}
}

// Cancelling while one storage is being recovered must keep the next storage of the same
// session untouched. The active profile (green) is recovered first and holds an orphan
// that ignores the polite signal, so its recovery lasts the kill grace.
func TestRecoverProcessesChecksTheContextBeforeEachStorage(t *testing.T) {
	f := newRecoveryFixture(t)
	f.session(t, "demo", fmt.Sprintf(`{"cwd":%q,"tools":{"native":{"process":{"shell":"/bin/sh","kill_grace_ms":1500}}},"paths":{"active_profile":"green","profiles":{"green":{"models":["model.json"]},"blue":{"models":["model.json"]}}}}`, f.workspace))
	green := filepath.Join(f.workspace, ".jad", "profiles", "green", "tools", ".processes")
	blue := filepath.Join(f.workspace, ".jad", "profiles", "blue", "tools", ".processes")
	stubborn := leaveOrphanRunning(t, green, "proc_green", "sh", "-c", `trap "" TERM; sleep 120`)
	untouched := leaveOrphan(t, blue, "proc_blue")
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(400*time.Millisecond, cancel)
	if _, err := f.resolver(t).RecoverProcesses(ctx); err == nil {
		t.Fatal("a scan cancelled during the first storage returned no error")
	}
	if !stubborn.waitGone(t, 100*time.Millisecond) {
		t.Fatal("the recovery that was already going on should have finished")
	}
	if untouched.waitGone(t, 300*time.Millisecond) {
		t.Fatal("the next storage was recovered after the scan was cancelled")
	}
}

// Closing the service while the scan is in a slow recovery must cancel the rest of the
// scan instead of waiting for it.
func TestServiceCloseCancelsTheRestOfTheScan(t *testing.T) {
	f := newRecoveryFixture(t)
	slow := filepath.Join(f.workspace, "slow")
	rest := filepath.Join(f.workspace, "rest")
	f.session(t, "a-slow", fmt.Sprintf(`{"cwd":%q,"tools":{"native":{"process":{"shell":"/bin/sh","storage_dir":%q,"kill_grace_ms":1500}}}}`, f.workspace, slow))
	f.session(t, "b-rest", f.processSetting(rest))
	leaveOrphanRunning(t, slow, "proc_slow", "sh", "-c", `trap "" TERM; sleep 120`)
	untouched := leaveOrphan(t, rest, "proc_rest")
	time.Sleep(200 * time.Millisecond)

	service, err := NewService(serviceConfig(t, f, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	service.Close()
	if untouched.waitGone(t, 300*time.Millisecond) {
		t.Fatal("Close let the scan go on to the next session")
	}
}

// Service.Close returns only after the scan has ended, so nothing of it (a log line, a
// recovery that signals a process) happens after the resolver and the log are closed.
func TestServiceCloseWaitsForTheScanToEnd(t *testing.T) {
	f := newRecoveryFixture(t)
	slow := filepath.Join(f.workspace, "slow")
	f.session(t, "demo", fmt.Sprintf(`{"cwd":%q,"tools":{"native":{"process":{"shell":"/bin/sh","storage_dir":%q,"kill_grace_ms":1500}}}}`, f.workspace, slow))
	leaveOrphanRunning(t, slow, "proc_slow", "sh", "-c", `trap "" TERM; sleep 120`)
	time.Sleep(200 * time.Millisecond)

	service, err := NewService(serviceConfig(t, f, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the scan is inside the slow recovery
	service.Close()
	select {
	case <-service.recovering:
	default:
		t.Fatal("Service.Close returned while the scan was still going")
	}
}
