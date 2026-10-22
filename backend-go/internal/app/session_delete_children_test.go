package app

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

func deleteDemoSession(t *testing.T, f *layoutFixture) {
	t.Helper()
	path := catalogSessionsPath + "/demo"
	current := f.catalogCall(t, "GET", path, "", nil)
	if reply := f.catalogCall(t, "DELETE", path, "", map[string]string{"If-Match": current.etag}); reply.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", reply.status, reply.body)
	}
}

func demoSettingPath(t *testing.T, f *layoutFixture) string {
	t.Helper()
	location, err := sessionfs.Resolve(f.service.resolver.roots, "local:demo")
	if err != nil {
		t.Fatal(err)
	}
	return location.File
}

func startSleeper(t *testing.T, manager *native.ManagedProcessManager, workspace string) string {
	t.Helper()
	snapshot, err := manager.Start(native.ProcessStartRequest{
		CWD: workspace, DisplayCWD: workspace, Command: "sleep 60", Environment: os.Environ(),
		Policy: sessionfs.NativeProcessSettings{
			Shell: "/bin/sh", KillGraceMs: 250, MaxRunning: 4, MaxCompleted: 10,
			LogCapBytes: 1024, MaxLineChars: 100, StdinMaxBytes: 1024, Env: map[string]string{}, EnvDeny: []string{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.ID
}

func TestSessionDeleteClosesItsMCPManagersAndKeepsOthers(t *testing.T) {
	f := newLayoutFixture(t)
	resolver := f.service.resolver
	settingPath := demoSettingPath(t, f)
	own, err := resolver.mcpManagerFor(settingPath + "\x00dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	toolset, err := resolver.mcpManagerFor(settingPath + "\x00dev/coder\x00toolset\x00x")
	if err != nil {
		t.Fatal(err)
	}
	other, err := resolver.mcpManagerFor(filepath.Join(f.root, "sessions", "other.json") + "\x00dev/coder")
	if err != nil {
		t.Fatal(err)
	}

	deleteDemoSession(t, f)

	resolver.mu.Lock()
	remaining := len(resolver.mcpManagers)
	resolver.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("%d MCP managers left cached, want only the other session's", remaining)
	}
	if _, err := own.LoadTools(t.Context(), nil, f.cwd); err == nil {
		t.Error("the deleted session's MCP manager still accepts work")
	}
	if _, err := toolset.LoadTools(t.Context(), nil, f.cwd); err == nil {
		t.Error("the deleted session's toolset MCP manager still accepts work")
	}
	if _, err := other.LoadTools(t.Context(), nil, f.cwd); err != nil {
		t.Errorf("another session's MCP manager was closed: %v", err)
	}
}

func TestSessionDeleteStopsItsManagedProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	f := newLayoutFixture(t)
	resolver := f.service.resolver
	settingPath := demoSettingPath(t, f)
	processes := sessionfs.NativeProcessSettings{StorageDir: filepath.Join(f.cwd, "demo-processes"), KillGraceMs: 250}
	manager := resolver.sharedProcessManager(sessionfs.SessionConfig{SettingPath: settingPath, NativeProcess: &processes}).(*native.ManagedProcessManager)
	id := startSleeper(t, manager, f.cwd)

	// A second session that points at the same storage directory keeps the manager alive.
	resolver.sharedProcessManager(sessionfs.SessionConfig{SettingPath: filepath.Join(f.root, "sessions", "twin.json"), NativeProcess: &processes})
	deleteDemoSession(t, f)
	if snapshot, found := manager.Snapshot(id); !found || snapshot.State != native.ProcessRunning {
		t.Fatalf("a manager the twin session still uses was stopped: %+v", snapshot)
	}

	resolver.releaseSessionChildren(filepath.Join(f.root, "sessions", "twin.json"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, found := manager.Snapshot(id)
		if found && snapshot.State != native.ProcessRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process still running after its last session was released: %+v", snapshot)
		}
		time.Sleep(20 * time.Millisecond)
	}
	resolver.mu.Lock()
	left := len(resolver.processManagers) + len(resolver.processSessions)
	resolver.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d process manager entries left cached", left)
	}
}

func TestSessionDeleteWithStateAlsoClosesItsMCPManagers(t *testing.T) {
	f := newLayoutFixture(t)
	resolver := f.service.resolver
	manager, err := resolver.mcpManagerFor(demoSettingPath(t, f) + "\x00dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	path := catalogSessionsPath + "/demo"
	current := f.catalogCall(t, "GET", path, "", nil)
	if reply := f.catalogCall(t, "DELETE", path+"?state=delete", "", map[string]string{"If-Match": current.etag}); reply.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", reply.status, reply.body)
	}
	if _, err := manager.LoadTools(t.Context(), nil, f.cwd); err == nil {
		t.Error("the MCP manager of a session deleted with its state still accepts work")
	}
}

func TestSessionFileGoneReportsAMissingFileOnly(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "a.json")
	if err := os.WriteFile(present, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if sessionFileGone(present) {
		t.Error("an existing file reported as gone")
	}
	if !sessionFileGone(filepath.Join(dir, "missing.json")) {
		t.Error("a missing file not reported as gone")
	}
}
