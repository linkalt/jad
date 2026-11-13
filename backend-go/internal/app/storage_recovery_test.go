package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

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
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: layout.Workspace, GlobalDir: layout.GlobalDir, Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	return recoveryFixture{workspace: workspace, layout: layout, roots: config.Roots}
}

// newTwoProfileFixture is one session, demo, whose profiles dev/green and
// dev/blue share a toolset, so each keeps its tool storage in its own folder.
func newTwoProfileFixture(t *testing.T, native map[string]any) recoveryFixture {
	t.Helper()
	workspace := t.TempDir()
	profile := map[string]any{"models": []string{"openai/default"}, "model_default": "openai/default", "toolset": "coding/default"}
	layout := layouttest.Write(t, layouttest.Spec{
		SessionID: "demo", CWD: workspace, Native: native,
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

// brokenSession adds a session whose file is not JSON.
func (f recoveryFixture) brokenSession(t *testing.T, name string) {
	t.Helper()
	if err := os.MkdirAll(f.layout.SessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.layout.SessionsDir, name+".json"), []byte(`{ this is not json`), 0o600); err != nil {
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

// subagentTool is the native subagent settings with one agent and a storage directory
// (none when empty).
func subagentTool(storage string, extra map[string]any) map[string]any {
	tool := map[string]any{"agents": map[string]any{"explore": map[string]any{"description": "d", "model": "openai/default", "instructions": "p", "tools": []string{"read_image"}}}}
	if storage != "" {
		tool["storage_dir"] = storage
	}
	for key, value := range extra {
		tool[key] = value
	}
	return tool
}

// serviceConfig loads the fixture through LoadConfig, so the root setting key reaches
// the service the way it does in production. server is the JSON body of "server".
func serviceConfig(t *testing.T, f recoveryFixture, server string) Config {
	t.Helper()
	var root map[string]json.RawMessage
	data, err := os.ReadFile(f.layout.GlobalSetting)
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
	if err := os.WriteFile(f.layout.GlobalSetting, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir:  f.layout.Workspace,
		GlobalDir:   f.layout.GlobalDir,
		Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken},
	})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	return config
}

func TestLoadConfigReadsTheRecoverSubagentRunsSwitch(t *testing.T) {
	f := newRecoveryFixture(t)
	for _, c := range []struct {
		server   string
		wantRuns bool
	}{
		{`{}`, true},
		{`{"recover_subagent_runs_at_start":null}`, true},
		{`{"recover_subagent_runs_at_start":false}`, false},
	} {
		if config := serviceConfig(t, f, c.server); config.RecoverSubagentRunsAtStart != c.wantRuns {
			t.Errorf("server %s: runs=%v, want %v", c.server, config.RecoverSubagentRunsAtStart, c.wantRuns)
		}
	}
}

// recover_processes_at_start went with the process tool: a root setting that still has it
// is refused by name, not ignored.
func TestLoadConfigRefusesTheRemovedRecoverProcessesSwitch(t *testing.T) {
	f := newRecoveryFixture(t)
	serviceConfig(t, f, `{}`)
	data, err := os.ReadFile(f.layout.GlobalSetting)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	root["server"] = json.RawMessage(`{"recover_processes_at_start":false}`)
	data, _ = json.Marshal(root)
	if err := os.WriteFile(f.layout.GlobalSetting, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadConfig(sessionfs.LoadOptions{WorkingDir: f.layout.Workspace, GlobalDir: f.layout.GlobalDir, Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken}})
	if err == nil || !strings.Contains(err.Error(), "recover_processes_at_start was removed") {
		t.Fatalf("LoadConfig() error = %v, want the removed key named", err)
	}
}

// Close must not hang on, or race with, a scan that is still going.
func TestServiceCloseEndsTheStartScan(t *testing.T) {
	f := newRecoveryFixture(t)
	for i := 0; i < 20; i++ {
		f.session(t, fmt.Sprintf("s%02d", i), f.subagentSetting(filepath.Join(f.workspace, fmt.Sprintf("r%02d", i)), nil))
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

// Cancelling ends the scan before the next session is even loaded: with no session
// having the subagent tool, the only effect of the cancel is the error it returns.
func TestRecoverStoragesChecksTheContextBeforeEachSession(t *testing.T) {
	f := newRecoveryFixture(t)
	f.session(t, "plain", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.resolver(t).RecoverStorages(ctx, StorageRecoveryOptions{SubagentRuns: true}); err == nil {
		t.Fatal("a cancelled scan over a session without the tool returned no error")
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
	if report.RunStorages != 1 {
		t.Fatalf("report = %+v, want 1 run storage", report)
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

func TestServiceStartsNoScanWhenTheSwitchIsOff(t *testing.T) {
	f := newRecoveryFixture(t)
	storage := filepath.Join(f.workspace, "runs")
	f.session(t, "demo", f.subagentSetting(storage, nil))
	run := leaveRun(t, storage, "sub_0000aaaa", "running", time.Time{})
	service, err := NewService(serviceConfig(t, f, `{"recover_subagent_runs_at_start":false}`))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if service.recovering != nil {
		t.Fatal("a scan was started with the switch off")
	}
	if state, _, _ := runState(t, run); state != "running" {
		t.Fatalf("run is %q although recover_subagent_runs_at_start is off", state)
	}
}
