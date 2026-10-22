package app

import (
	"net/http"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

// The tests below document what the server does today and are meant to be flipped when
// the async manager makes a user stop, a reset and a session delete reach processes.

func processOfDemoSession(t *testing.T, f *layoutFixture, storage string) (*native.ManagedProcessManager, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	processes := sessionfs.NativeProcessSettings{StorageDir: filepath.Join(f.cwd, storage), KillGraceMs: 250}
	manager := f.service.resolver.sharedProcessManager(sessionfs.SessionConfig{SettingPath: demoSettingPath(t, f), NativeProcess: &processes}).(*native.ManagedProcessManager)
	return manager, startSleeper(t, manager, f.cwd)
}

func requireStillRunningAfter(t *testing.T, manager *native.ManagedProcessManager, id, what string) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	if snapshot, found := manager.Snapshot(id); !found || snapshot.State != native.ProcessRunning {
		t.Fatalf("process after %s = %+v found=%v, want running (current behaviour)", what, snapshot, found)
	}
}

func TestCurrentBehaviourUserStopDoesNotStopProcessesAndWP4Flips(t *testing.T) {
	f := newLayoutFixture(t)
	manager, id := processOfDemoSession(t, f, "stop-processes")
	defer manager.Stop(id, "test_cleanup")
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop", ""); status != http.StatusOK {
		t.Fatalf("stop: %d %s", status, body)
	}
	requireStillRunningAfter(t, manager, id, "a user stop")
}

func TestCurrentBehaviourUserResetDoesNotStopProcessesAndWP4Flips(t *testing.T) {
	f := newLayoutFixture(t)
	manager, id := processOfDemoSession(t, f, "reset-processes")
	defer manager.Stop(id, "test_cleanup")
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/reset", ""); status != http.StatusOK {
		t.Fatalf("reset: %d %s", status, body)
	}
	requireStillRunningAfter(t, manager, id, "a reset")
}

func TestCurrentBehaviourSharedStorageSessionDeleteLeavesItsProcessesRunningAndWP4Flips(t *testing.T) {
	f := newLayoutFixture(t)
	manager, id := processOfDemoSession(t, f, "shared-processes")
	defer manager.Stop(id, "test_cleanup")
	processes := sessionfs.NativeProcessSettings{StorageDir: filepath.Join(f.cwd, "shared-processes"), KillGraceMs: 250}
	f.service.resolver.sharedProcessManager(sessionfs.SessionConfig{SettingPath: filepath.Join(f.root, "sessions", "twin.json"), NativeProcess: &processes})
	deleteDemoSession(t, f)
	requireStillRunningAfter(t, manager, id, "deleting a session whose storage another session shares")
}
