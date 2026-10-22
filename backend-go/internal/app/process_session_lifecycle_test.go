package app

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// A user stop, a reset and a session delete reach the session's process handles through
// the async manager, also on a storage other sessions share.

type processHandleFixture struct {
	f       *layoutFixture
	manager *native.ManagedProcessManager
	host    *native.AsyncHost
	process tools.Tool
	async   tools.Tool
	storage string
}

func newProcessHandleFixture(t *testing.T, f *layoutFixture, storage string) processHandleFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	dir := filepath.Join(f.cwd, storage)
	settings := sessionfs.NativeProcessSettings{StorageDir: dir, KillGraceMs: 250, Shell: "/bin/sh", MaxRunning: 8, MaxCompleted: 10,
		LogCapBytes: 1 << 16, MaxLineChars: 1000, StdinMaxBytes: 1024, Env: map[string]string{}, EnvDeny: []string{}}
	manager := f.service.resolver.sharedProcessManager(sessionfs.SessionConfig{SettingPath: demoSettingPath(t, f), NativeProcess: &settings}).(*native.ManagedProcessManager)
	host, err := f.service.resolver.AsyncHost(native.AsyncHostConfig{Dir: native.AsyncDir(dir), Config: asyncmgr.DefaultConfig()})
	if err != nil {
		t.Fatal(err)
	}
	process, err := native.NewProcessToolWithAsync(settings, manager, "KEY", host)
	if err != nil {
		t.Fatal(err)
	}
	async, err := native.NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	return processHandleFixture{f: f, manager: manager, host: host, process: process, async: async, storage: dir}
}

func (p processHandleFixture) start(t *testing.T, sessionKey, command string) string {
	t.Helper()
	out, err := p.process.Execute(context.Background(), map[string]any{"command": command, "block_ms": json.Number("0")},
		tools.ExecutionContext{CallID: "c", WorkspaceRoot: p.f.cwd, SessionKey: sessionKey})
	if err != nil {
		t.Fatal(err)
	}
	id := regexp.MustCompile(`proc_[0-9a-f-]+`).FindString(out)
	if id == "" {
		t.Fatalf("no process id in %s", out)
	}
	return id
}

func (p processHandleFixture) key(t *testing.T) string {
	t.Helper()
	profile, err := p.f.service.resolver.CanonicalSessionProfile("local:demo", "")
	if err != nil {
		t.Fatal(err)
	}
	reference, err := p.f.service.resolver.CanonicalSessionReference("local:demo")
	if err != nil {
		t.Fatal(err)
	}
	return sessionKey(reference, profile)
}

func (p processHandleFixture) requireState(t *testing.T, id string, want native.ProcessState, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, found := p.manager.Snapshot(id)
		if found && snapshot.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process after %s = %+v found=%v, want %s", what, snapshot, found, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p processHandleFixture) requireStillRunning(t *testing.T, id, what string) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	if snapshot, found := p.manager.Snapshot(id); !found || snapshot.State != native.ProcessRunning {
		t.Fatalf("process after %s = %+v found=%v, want running", what, snapshot, found)
	}
}

func (p processHandleFixture) wait(t *testing.T, owner string) string {
	t.Helper()
	out, err := p.async.Execute(context.Background(), map[string]any{"action": "wait", "block_ms": json.Number("0")},
		tools.ExecutionContext{CallID: "w", WorkspaceRoot: p.f.cwd, SessionKey: owner})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUserStopStopsTheSessionsProcessHandlesAndQueuesTheSignal(t *testing.T) {
	f := newLayoutFixture(t)
	p := newProcessHandleFixture(t, f, "stop-processes")
	owner := p.key(t)
	mine := p.start(t, owner, "sleep 60 & sleep 60; wait")
	other := p.start(t, "another|session", "sleep 60")
	defer p.manager.Stop(other, "test_cleanup")
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop", ""); status != http.StatusOK {
		t.Fatalf("stop: %d %s", status, body)
	}
	p.requireState(t, mine, native.ProcessStopped, "a user stop")
	p.requireStillRunning(t, other, "a user stop of another session")
	if got := p.wait(t, owner); !strings.Contains(got, mine) || !strings.Contains(got, "stopped") || !strings.Contains(got, "user_stop") {
		t.Fatalf("the next wait does not explain the stop:\n%s", got)
	}
}

func TestUserResetStopsTheSessionsProcessHandlesAndDropsItsUnreadSignals(t *testing.T) {
	f := newLayoutFixture(t)
	p := newProcessHandleFixture(t, f, "reset-processes")
	owner := p.key(t)
	finished := p.start(t, owner, "exit 0")
	p.requireState(t, finished, native.ProcessExited, "a quick exit")
	running := p.start(t, owner, "sleep 60")
	other := p.start(t, "another|session", "sleep 60")
	defer p.manager.Stop(other, "test_cleanup")
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/reset", ""); status != http.StatusOK {
		t.Fatalf("reset: %d %s", status, body)
	}
	p.requireState(t, running, native.ProcessStopped, "a reset")
	p.requireStillRunning(t, other, "a reset of another session")
	got := p.wait(t, owner)
	if strings.Contains(got, finished) || strings.Contains(got, running) {
		t.Fatalf("a reset left unread signals behind:\n%s", got)
	}
}

func TestSharedStorageSessionDeleteStopsItsProcessesAndLeavesOtherSessionsAlone(t *testing.T) {
	f := newLayoutFixture(t)
	p := newProcessHandleFixture(t, f, "shared-processes")
	owner := p.key(t)
	mine := p.start(t, owner, "sleep 60")
	twin := p.start(t, "twin|session", "sleep 60")
	defer p.manager.Stop(twin, "test_cleanup")
	finished := p.start(t, owner, "exit 0")
	p.requireState(t, finished, native.ProcessExited, "a quick exit")
	settings := sessionfs.NativeProcessSettings{StorageDir: p.storage, KillGraceMs: 250}
	f.service.resolver.sharedProcessManager(sessionfs.SessionConfig{SettingPath: filepath.Join(f.root, "sessions", "twin.json"), NativeProcess: &settings})
	deleteDemoSession(t, f)
	p.requireState(t, mine, native.ProcessStopped, "deleting the session")
	p.requireStillRunning(t, twin, "deleting another session on the same storage")
	if got := p.wait(t, owner); strings.Contains(got, mine) || strings.Contains(got, finished) {
		t.Fatalf("the deleted session's handles are still known:\n%s", got)
	}
	if got := p.wait(t, "twin|session"); strings.Contains(got, "unknown") {
		t.Fatalf("the other session lost its handle:\n%s", got)
	}
}

func TestEndingARunLeavesProcessHandlesRunning(t *testing.T) {
	f := newLayoutFixture(t)
	p := newProcessHandleFixture(t, f, "run-end-processes")
	owner := p.key(t)
	id := p.start(t, owner, "sleep 60")
	defer p.manager.Stop(id, "test_cleanup")
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", `{"input":"hello"}`); status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("run: %d %s", status, body)
	}
	p.requireStillRunning(t, id, "a run that ended normally")
}

// The run stream's async_state reads the same manager: counts and the newest live ids of
// one session, bounded by sse_max_ids, never another session's handles.
func TestWatchSessionAsyncReportsCountsAndBoundedIDsOfOneSession(t *testing.T) {
	f := newLayoutFixture(t)
	p := newProcessHandleFixture(t, f, "watch-processes")
	owner := p.key(t)
	reference, profile, _ := strings.Cut(owner, "\x00")

	changed := make(chan struct{}, 64)
	read, stop := f.service.resolver.WatchSessionAsync(reference, profile, func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	if read == nil {
		t.Fatal("no watcher for a resolver with an async host")
	}
	defer stop()
	if state := read(); state.Active != 0 || state.Unread != 0 || len(state.IDs) != 0 {
		t.Fatalf("initial state = %+v", state)
	}

	var mine []string
	for i := 0; i < 3; i++ {
		mine = append(mine, p.start(t, owner, "sleep 60"))
	}
	other := p.start(t, "another|session", "sleep 60")
	defer p.manager.Stop(other, "test_cleanup")
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("starting handles did not notify the watcher")
	}
	state := read()
	if state.Active != 3 || len(state.IDs) != 3 {
		t.Fatalf("state = %+v, want 3 active and their ids", state)
	}
	for _, id := range state.IDs {
		if id == other {
			t.Fatal("another session's handle id reached this session's state")
		}
	}

	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop", ""); status != http.StatusOK {
		t.Fatalf("stop: %d %s", status, body)
	}
	p.requireState(t, mine[0], native.ProcessStopped, "a user stop")
	deadline := time.Now().Add(5 * time.Second)
	for read().Active != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if state := read(); state.Active != 0 || state.Unread == 0 || len(state.IDs) != 0 {
		t.Fatalf("after the stop: %+v, want no live ids and the stopped handles unread", state)
	}
}

func TestSessionAsyncStateContextListsPendingHandlesAndCountsTheRest(t *testing.T) {
	f := newLayoutFixture(t)
	p := newProcessHandleFixture(t, f, "notice-processes")
	owner := p.key(t)
	for i := 0; i < 3; i++ {
		p.start(t, owner, "sleep 60")
	}
	other := p.start(t, "another|session", "sleep 60")
	defer p.manager.Stop(other, "test_cleanup")

	section, err := native.CollectAsyncStateContext(p.host.Manager(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if section.Active != 3 || len(section.Items) != 3 || section.HasMore {
		t.Fatalf("section = %+v", section)
	}
	for _, item := range section.Items {
		if item.Kind != "process" || item.State == "" || item.ID == other {
			t.Fatalf("item = %+v", item)
		}
	}
	encoded, _ := json.Marshal(section)
	if strings.Contains(string(encoded), "summary") || strings.Contains(string(encoded), "exit_code") {
		t.Fatalf("the notice carries a summary or a result: %s", encoded)
	}

	for section.StateContextItemCount() > 0 {
		before := section.StateContextItemCount()
		if !section.TrimStateContextItem() || section.StateContextItemCount() != before-1 {
			t.Fatal("trimming does not drop one item at a time")
		}
	}
	if section.OmittedBySnapshot != 3 || section.TrimStateContextItem() {
		t.Fatalf("after trimming: %+v", section)
	}
	if _, err := native.CollectAsyncStateContext(nil, owner); err == nil {
		t.Fatal("a missing manager must be an error, not an empty notice")
	}
}
