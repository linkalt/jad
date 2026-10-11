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
	"example.com/agent-harness/backend-go/internal/sessionfs/layouttest"
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

// recoveryFixture is a workspace with a catalog-layout root. Each session is added with
// its own native tool settings.
type recoveryFixture struct {
	workspace string
	layout    layouttest.Fixture
	roots     []sessionfs.Root
}

func newRecoveryFixture(t *testing.T) recoveryFixture {
	t.Helper()
	workspace := t.TempDir()
	layout := layouttest.Write(t, layouttest.Spec{CWD: workspace, NoSession: true})
	return recoveryFixtureFrom(t, workspace, layout)
}

func recoveryFixtureFrom(t *testing.T, workspace string, layout layouttest.Fixture) recoveryFixture {
	t.Helper()
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: layout.Root, RootSettingPath: layout.RootSetting, Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	return recoveryFixture{workspace: workspace, layout: layout, roots: config.Roots}
}

// newTwoProfileFixture is one session, demo, whose profiles dev/green (active) and
// dev/blue share a toolset, so each keeps its tool storage in its own folder.
func newTwoProfileFixture(t *testing.T, native map[string]any) recoveryFixture {
	t.Helper()
	workspace := t.TempDir()
	profile := map[string]any{"models": []string{"openai/default"}, "model_default": "openai/default", "toolset": "coding/default"}
	layout := layouttest.Write(t, layouttest.Spec{
		SessionID: "demo", CWD: workspace, Native: native, ActiveProfile: "dev/green",
		Profiles: map[string]map[string]any{"dev/green": profile, "dev/blue": profile},
	})
	return recoveryFixtureFrom(t, workspace, layout)
}

// profileTools is where the named dev profile of session demo keeps its tool storage.
func (f recoveryFixture) profileTools(name string) string {
	return filepath.Join(f.workspace, ".jad", "state", "demo", "dev", name, "tools")
}

// session adds a session that uses the given native tool settings (none: no toolset).
func (f recoveryFixture) session(t *testing.T, name string, native map[string]any) {
	t.Helper()
	f.layout.AddSession(t, name, f.workspace, native, nil)
}

// brokenSession adds a session whose session.json is not JSON.
func (f recoveryFixture) brokenSession(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join(f.layout.SessionsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{ this is not json`), 0o600); err != nil {
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

// processTool is the native process settings with a storage directory (none when empty).
func processTool(storage string, extra map[string]any) map[string]any {
	tool := map[string]any{"shell": "/bin/sh", "kill_grace_ms": 200}
	if storage != "" {
		tool["storage_dir"] = storage
	}
	for key, value := range extra {
		tool[key] = value
	}
	return tool
}

// subagentTool is the native subagent settings with one agent and a storage directory
// (none when empty).
func subagentTool(storage string, extra map[string]any) map[string]any {
	tool := map[string]any{"agents": map[string]any{"explore": map[string]any{"description": "d", "model": "openai/default", "instructions": "p", "tools": []string{"read"}}}}
	if storage != "" {
		tool["storage_dir"] = storage
	}
	for key, value := range extra {
		tool[key] = value
	}
	return tool
}

func (f recoveryFixture) processSetting(storage string) map[string]any {
	return map[string]any{"process": processTool(storage, nil)}
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
	f.session(t, "plain", nil)
	// A directory with no session.json is not a session.
	if err := os.MkdirAll(filepath.Join(f.layout.SessionsDir, "not-a-session"), 0o755); err != nil {
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
	f.brokenSession(t, "a-broken")
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
	f := newTwoProfileFixture(t, map[string]any{"process": map[string]any{"shell": "/bin/sh", "kill_grace_ms": 200}})
	blue := filepath.Join(f.profileTools("blue"), ".processes")
	green := filepath.Join(f.profileTools("green"), ".processes")
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
	f.session(t, "demo", map[string]any{"process": map[string]any{"shell": "/bin/sh", "storage_dir": storage, "stop_orphans": false}})
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
	var root map[string]json.RawMessage
	data, err := os.ReadFile(f.layout.RootSetting)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	root["server"] = json.RawMessage(server)
	data, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.layout.RootSetting, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir:      f.layout.Root,
		RootSettingPath: f.layout.RootSetting,
		Environment:     map[string]string{"AGENT_API_TOKEN": integrationAPIToken},
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

func TestLoadConfigReadsTheRecoverSubagentRunsSwitch(t *testing.T) {
	f := newRecoveryFixture(t)
	for _, c := range []struct {
		server        string
		wantRuns      bool
		wantProcesses bool
	}{
		{`{}`, true, true},
		{`{"recover_subagent_runs_at_start":null}`, true, true},
		{`{"recover_subagent_runs_at_start":false}`, false, true},
		{`{"recover_subagent_runs_at_start":false,"recover_processes_at_start":false}`, false, false},
		{`{"recover_processes_at_start":false}`, true, false},
	} {
		config := serviceConfig(t, f, c.server)
		if config.RecoverSubagentRunsAtStart != c.wantRuns || config.RecoverProcessesAtStart != c.wantProcesses {
			t.Errorf("server %s: runs=%v processes=%v, want runs=%v processes=%v", c.server, config.RecoverSubagentRunsAtStart, config.RecoverProcessesAtStart, c.wantRuns, c.wantProcesses)
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
	f.session(t, "demo", map[string]any{"process": map[string]any{"shell": "/bin/sh", "storage_dir": storage, "kill_grace_ms": 1500}})
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
	f.session(t, "plain", nil)
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
	f := newTwoProfileFixture(t, map[string]any{"process": map[string]any{"shell": "/bin/sh", "kill_grace_ms": 1500}})
	green := filepath.Join(f.profileTools("green"), ".processes")
	blue := filepath.Join(f.profileTools("blue"), ".processes")
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
	f.session(t, "a-slow", map[string]any{"process": map[string]any{"shell": "/bin/sh", "storage_dir": slow, "kill_grace_ms": 1500}})
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
	f.session(t, "demo", map[string]any{"process": map[string]any{"shell": "/bin/sh", "storage_dir": slow, "kill_grace_ms": 1500}})
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

// ---- sub-agent run manifests ----

func (f recoveryFixture) subagentSetting(storage string, extra map[string]any) map[string]any {
	return map[string]any{"subagent": subagentTool(storage, extra)}
}

// leaveRun writes the manifest of a run a crashed server left in state, written by a
// server that no longer exists.
func leaveRun(t *testing.T, storage, id, state string, ended time.Time) string {
	t.Helper()
	dir := filepath.Join(storage, ".runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"version": 1, "id": id, "owner": "local:demo\x00", "agent": "explore", "task": "t", "timeout_ms": 1000,
		"state": state, "started_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		"server": map[string]any{"pid": 2000000000},
	}
	if !ended.IsZero() {
		manifest["ended_at"] = ended.UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runState(t *testing.T, path string) (state, reason string, exists bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct{ State, Reason string }
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.State, manifest.Reason, true
}

// waitForState polls the manifest until it reads want or the limit passes.
func waitForState(t *testing.T, path, want string, limit time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		state, _, _ := runState(t, path)
		if state == want || time.Now().After(deadline) {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRecoverStoragesMarksAnInterruptedRunLostWithoutAnyRequest(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "runs")
	f.session(t, "demo", f.subagentSetting(storage, nil))
	running := leaveRun(t, storage, "sub_0000aaaa", "running", time.Time{})
	queued := leaveRun(t, storage, "sub_0000bbbb", "queued", time.Time{})
	done := leaveRun(t, storage, "sub_0000cccc", "done", time.Now().Add(-time.Minute))

	report, err := f.resolver(t).RecoverStorages(context.Background(), StorageRecoveryOptions{SubagentRuns: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{running, queued} {
		state, reason, _ := runState(t, path)
		if state != "lost" || !strings.Contains(reason, "server stopped") {
			t.Errorf("%s: state %q reason %q, want lost / the server stopped", filepath.Base(path), state, reason)
		}
	}
	if state, _, _ := runState(t, done); state != "done" {
		t.Errorf("an ended run changed to %q", state)
	}
	if report.RunStorages != 1 || report.Storages != 0 {
		t.Fatalf("report = %+v, want 1 run storage and 0 process storages", report)
	}
}

func TestRecoverStoragesPrunesRunsBeyondKeepFinished(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "runs")
	f.session(t, "demo", f.subagentSetting(storage, map[string]any{"keep_finished": 1}))
	oldest := leaveRun(t, storage, "sub_0000aaaa", "done", time.Now().Add(-3*time.Hour))
	middle := leaveRun(t, storage, "sub_0000bbbb", "done", time.Now().Add(-2*time.Hour))
	newest := leaveRun(t, storage, "sub_0000cccc", "done", time.Now().Add(-time.Hour))

	if _, err := f.resolver(t).RecoverStorages(context.Background(), StorageRecoveryOptions{SubagentRuns: true}); err != nil {
		t.Fatal(err)
	}
	_, _, oldestLeft := runState(t, oldest)
	_, _, middleLeft := runState(t, middle)
	_, _, newestLeft := runState(t, newest)
	if oldestLeft || middleLeft || !newestLeft {
		t.Fatalf("manifests left: oldest=%v middle=%v newest=%v; want only the newest", oldestLeft, middleLeft, newestLeft)
	}
}

func TestRecoverStoragesLeavesTheRunsOfAnotherLiveServerAlone(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "runs")
	f.session(t, "demo", f.subagentSetting(storage, nil))
	path := leaveRun(t, storage, "sub_0000aaaa", "running", time.Time{})
	// Name this very test process's parent as the owner: another live server.
	data, _ := os.ReadFile(path)
	var manifest map[string]any
	_ = json.Unmarshal(data, &manifest)
	manifest["server"] = map[string]any{"pid": os.Getppid(), "start": linuxStartTime(t, os.Getppid())}
	data, _ = json.Marshal(manifest)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := f.resolver(t).RecoverStorages(context.Background(), StorageRecoveryOptions{SubagentRuns: true}); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := runState(t, path); state != "running" {
		t.Fatalf("a run of another live server was changed to %q", state)
	}
}

func TestRecoverStoragesCreatesNoRunStorageForASessionThatNeverStartedAChild(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "never-created")
	f.session(t, "demo", f.subagentSetting(storage, nil))

	report, err := f.resolver(t).RecoverStorages(context.Background(), StorageRecoveryOptions{SubagentRuns: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(storage); !os.IsNotExist(err) {
		t.Fatalf("stat(%s) error = %v; the scan must not create a storage directory", storage, err)
	}
	if report.RunStorages != 0 {
		t.Fatalf("report = %+v, want 0 run storages", report)
	}
}

func TestRecoverStoragesSkipsASessionThatDoesNotPersistRuns(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "runs")
	f.session(t, "demo", f.subagentSetting(storage, map[string]any{"persist_runs": false}))
	path := leaveRun(t, storage, "sub_0000aaaa", "running", time.Time{})

	report, err := f.resolver(t).RecoverStorages(context.Background(), StorageRecoveryOptions{SubagentRuns: true})
	if err != nil {
		t.Fatal(err)
	}
	if state, _, _ := runState(t, path); state != "running" || report.RunStorages != 0 {
		t.Fatalf("state %q, report %+v; a session with persist_runs off has nothing to recover", state, report)
	}
}

func TestRecoverStoragesVisitsEveryProfilesRunStorage(t *testing.T) {
	f := newTwoProfileFixture(t, map[string]any{"subagent": subagentTool("", nil)})
	blue := leaveRun(t, filepath.Join(f.profileTools("blue"), ".subagents"), "sub_0000aaaa", "running", time.Time{})
	green := leaveRun(t, filepath.Join(f.profileTools("green"), ".subagents"), "sub_0000bbbb", "running", time.Time{})

	report, err := f.resolver(t).RecoverStorages(context.Background(), StorageRecoveryOptions{SubagentRuns: true})
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"blue": blue, "green": green} {
		if state, _, _ := runState(t, path); state != "lost" {
			t.Errorf("%s profile run is %q, want lost", name, state)
		}
	}
	if report.RunStorages != 2 {
		t.Fatalf("report = %+v, want 2 run storages", report)
	}
}

// One scan with both options recovers a session's processes and its runs, and neither
// option touches the other's storage.
func TestRecoverStoragesOptionsAreIndependent(t *testing.T) {
	f := newRecoveryFixture(t)
	procs := filepath.Join(f.workspace, "procs")
	runs := filepath.Join(f.workspace, "runs")
	f.session(t, "demo", map[string]any{"process": processTool(procs, nil), "subagent": subagentTool(runs, nil)})
	crashed := leaveOrphan(t, procs, "proc_a")
	run := leaveRun(t, runs, "sub_0000aaaa", "running", time.Time{})

	// Processes only: the orphan goes, the run stays as the crashed server left it.
	if _, err := f.resolver(t).RecoverStorages(context.Background(), StorageRecoveryOptions{Processes: true}); err != nil {
		t.Fatal(err)
	}
	if !crashed.waitGone(t, 3*time.Second) {
		t.Fatal("a processes scan did not stop the orphan")
	}
	if state, _, _ := runState(t, run); state != "running" {
		t.Fatalf("run is %q after a processes-only scan, want running (untouched)", state)
	}

	// Runs only: the run is marked lost; a second orphan is not signalled.
	second := leaveOrphan(t, procs, "proc_b")
	if _, err := f.resolver(t).RecoverStorages(context.Background(), StorageRecoveryOptions{SubagentRuns: true}); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := runState(t, run); state != "lost" {
		t.Fatalf("run is %q after a runs-only scan, want lost", state)
	}
	if second.waitGone(t, 300*time.Millisecond) {
		t.Fatal("a runs-only scan stopped a process")
	}
}

func TestServiceRecoversInterruptedRunsAtStartInTheBackground(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "runs")
	f.session(t, "demo", f.subagentSetting(storage, nil))
	path := leaveRun(t, storage, "sub_0000aaaa", "running", time.Time{})

	service, err := NewService(serviceConfig(t, f, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	// No request was sent: only the start-time scan can change the file.
	if got := waitForState(t, path, "lost", 5*time.Second); got != "lost" {
		t.Fatalf("run is %q after the service started, want lost", got)
	}
}

func TestServiceLeavesRunsAloneWhenOnlyTheRunSwitchIsOff(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "runs")
	procs := filepath.Join(f.workspace, "procs")
	f.session(t, "demo", map[string]any{"process": processTool(procs, nil), "subagent": subagentTool(storage, nil)})
	run := leaveRun(t, storage, "sub_0000aaaa", "running", time.Time{})
	crashed := leaveOrphan(t, procs, "proc_a")

	service, err := NewService(serviceConfig(t, f, `{"recover_subagent_runs_at_start":false}`))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if !crashed.waitGone(t, 5*time.Second) {
		t.Fatal("the process scan did not run although only the run switch is off")
	}
	// The scan stops a process before it reaches the run branch, so look only once it ended.
	select {
	case <-service.recovering:
	case <-time.After(5 * time.Second):
		t.Fatal("the start scan did not end")
	}
	if state, _, _ := runState(t, run); state != "running" {
		t.Fatalf("run is %q although recover_subagent_runs_at_start is off", state)
	}
}

func TestServiceStartsNoScanWhenBothSwitchesAreOff(t *testing.T) {
	f := newRecoveryFixture(t)
	service, err := NewService(serviceConfig(t, f, `{"recover_subagent_runs_at_start":false,"recover_processes_at_start":false}`))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if service.recovering != nil {
		t.Fatal("a scan was started with both switches off")
	}
}

func TestServiceRecoversRunsAtStartWhenOnlyTheProcessSwitchIsOff(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "runs")
	procs := filepath.Join(f.workspace, "procs")
	f.session(t, "demo", map[string]any{"process": processTool(procs, nil), "subagent": subagentTool(storage, nil)})
	run := leaveRun(t, storage, "sub_0000aaaa", "running", time.Time{})
	crashed := leaveOrphan(t, procs, "proc_a")

	service, err := NewService(serviceConfig(t, f, `{"recover_processes_at_start":false}`))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if got := waitForState(t, run, "lost", 5*time.Second); got != "lost" {
		t.Fatalf("run is %q, want lost: the run scan must start although the process switch is off", got)
	}
	select {
	case <-service.recovering:
	case <-time.After(5 * time.Second):
		t.Fatal("the start scan did not end")
	}
	if crashed.waitGone(t, 300*time.Millisecond) {
		t.Fatal("the orphan was stopped although recover_processes_at_start is off")
	}
}
