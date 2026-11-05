package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// A user stop, a reset and a session delete reach the session's exec handles through
// the async manager, also on a storage other sessions share.

type processHandleFixture struct {
	f       *layoutFixture
	host    *native.AsyncHost
	exec    tools.Tool
	async   tools.Tool
	storage string
}

func execHandleSettings(t *testing.T, dir string) sessionfs.NativeExecSettings {
	t.Helper()
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	return sessionfs.NativeExecSettings{
		Child: sessionfs.NativeBashSettings{
			Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}}, Shell: shell,
			TimeoutMs: 120_000, MaxTimeoutMs: 120_000, KillGraceMs: 100, KillProcessGroup: "always", MaxBytes: 2048,
			Stdout: sessionfs.NativeBashStreamBudget{HeadBytes: 512, TailBytes: 512}, Stderr: sessionfs.NativeBashStreamBudget{HeadBytes: 512, TailBytes: 512},
			Spill:        sessionfs.NativeBashSpillSettings{Keep: "truncated_only", CapBytes: 1024, StorageDir: filepath.Join(dir, ".exec-output")},
			MaxLineChars: 2000, StdinMaxBytes: 64 * 1024, Env: map[string]string{}, EnvDeny: []string{},
		},
		LangNames:   []string{"bash"},
		Langs:       map[string]sessionfs.NativeExecLang{"bash": {Argv: []string{"bash", "-c", "{code}", "exec"}, Mode: sessionfs.NativeExecModeArg, Ext: ".sh", Aliases: []string{}, LibEnv: "PATH"}},
		DefaultLang: "bash", CodeMaxBytes: 64 * 1024, InputMaxBytes: 4096, ArgMaxBytes: 64 * 1024, ResultMaxBytes: 1024,
	}
}

func newProcessHandleFixture(t *testing.T, f *layoutFixture, storage string) processHandleFixture {
	t.Helper()
	return newProcessHandleFixtureWith(t, f, storage, asyncmgr.DefaultConfig())
}

func newProcessHandleFixtureWith(t *testing.T, f *layoutFixture, storage string, config asyncmgr.Config) processHandleFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("exec handles need a POSIX shell")
	}
	dir := filepath.Join(f.cwd, storage)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	host, err := f.service.resolver.AsyncHost(native.AsyncHostConfig{Dir: native.AsyncDir(dir), Config: config})
	if err != nil {
		t.Fatal(err)
	}
	execTool, err := native.NewExecTool(execHandleSettings(t, dir), "KEY", host)
	if err != nil {
		t.Fatal(err)
	}
	async, err := native.NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	return processHandleFixture{f: f, host: host, exec: execTool, async: async, storage: dir}
}

// handle is one started exec handle: its id and the pid of the program it runs.
type handle struct {
	id  string
	pid int
}

// start runs code under exec with block_ms 0, so it becomes a handle; the program writes
// its pid first and then becomes the command (exec), so the pid is the command's.
func (p processHandleFixture) start(t *testing.T, sessionKey, command string) handle {
	t.Helper()
	pidFile := filepath.Join(p.storage, fmt.Sprintf("pid-%d", time.Now().UnixNano()))
	code := fmt.Sprintf("echo $$ > %q; exec bash -c %q", pidFile, command)
	arguments := p.exec.NormalizeArguments(map[string]any{"code": code, "block_ms": json.Number("0")})
	out, err := p.exec.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "c", WorkspaceRoot: p.f.cwd, SessionDir: p.storage, SessionKey: sessionKey})
	if err != nil {
		t.Fatal(err)
	}
	id := regexp.MustCompile(`exec_[0-9a-f]+`).FindString(out)
	if id == "" {
		t.Fatalf("no exec handle id in %s", out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				return handle{id: id, pid: pid}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("handle %s wrote no pid", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
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

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// requireGone waits for the handle's program to end.
func (p processHandleFixture) requireGone(t *testing.T, h handle, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(h.pid) {
		if time.Now().After(deadline) {
			t.Fatalf("handle %s (pid %d) still runs after %s", h.id, h.pid, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p processHandleFixture) requireStillRunning(t *testing.T, h handle, what string) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	if !processAlive(h.pid) {
		t.Fatalf("handle %s (pid %d) ended after %s, want running", h.id, h.pid, what)
	}
}

// stop ends a handle the test started for another session.
func (p processHandleFixture) stop(h handle) {
	_ = syscall.Kill(h.pid, syscall.SIGKILL)
}

func (p processHandleFixture) wait(t *testing.T, owner string) string {
	t.Helper()
	out, err := p.async.Execute(context.Background(), map[string]any{"action": "wait", "wait_ms": json.Number("0")},
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
	defer p.stop(other)
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop", ""); status != http.StatusOK {
		t.Fatalf("stop: %d %s", status, body)
	}
	p.requireGone(t, mine, "a user stop")
	p.requireStillRunning(t, other, "a user stop of another session")
	if got := p.wait(t, owner); !strings.Contains(got, mine.id) || !strings.Contains(got, "stopped") || !strings.Contains(got, "user_stop") {
		t.Fatalf("the next wait does not explain the stop:\n%s", got)
	}
}

func TestUserResetStopsTheSessionsProcessHandlesAndDropsItsUnreadSignals(t *testing.T) {
	f := newLayoutFixture(t)
	p := newProcessHandleFixture(t, f, "reset-processes")
	owner := p.key(t)
	finished := p.start(t, owner, "exit 0")
	p.requireGone(t, finished, "a quick exit")
	running := p.start(t, owner, "sleep 60")
	other := p.start(t, "another|session", "sleep 60")
	defer p.stop(other)
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/reset", ""); status != http.StatusOK {
		t.Fatalf("reset: %d %s", status, body)
	}
	p.requireGone(t, running, "a reset")
	p.requireStillRunning(t, other, "a reset of another session")
	got := p.wait(t, owner)
	if strings.Contains(got, finished.id) || strings.Contains(got, running.id) {
		t.Fatalf("a reset left unread signals behind:\n%s", got)
	}
}

func TestSharedStorageSessionDeleteStopsItsProcessesAndLeavesOtherSessionsAlone(t *testing.T) {
	f := newLayoutFixture(t)
	p := newProcessHandleFixture(t, f, "shared-processes")
	owner := p.key(t)
	mine := p.start(t, owner, "sleep 60")
	twin := p.start(t, "twin|session", "sleep 60")
	defer p.stop(twin)
	finished := p.start(t, owner, "exit 0")
	p.requireGone(t, finished, "a quick exit")
	deleteDemoSession(t, f)
	p.requireGone(t, mine, "deleting the session")
	p.requireStillRunning(t, twin, "deleting another session on the same storage")
	if got := p.wait(t, owner); strings.Contains(got, mine.id) || strings.Contains(got, finished.id) {
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
	defer p.stop(id)
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

	var mine []handle
	for i := 0; i < 3; i++ {
		mine = append(mine, p.start(t, owner, "sleep 60"))
	}
	other := p.start(t, "another|session", "sleep 60")
	defer p.stop(other)
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
		if id == other.id {
			t.Fatal("another session's handle id reached this session's state")
		}
	}

	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop", ""); status != http.StatusOK {
		t.Fatalf("stop: %d %s", status, body)
	}
	p.requireGone(t, mine[0], "a user stop")
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
	defer p.stop(other)

	section, err := native.CollectAsyncStateContext(p.host.Manager(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if section.Active != 3 || len(section.Items) != 3 || section.HasMore {
		t.Fatalf("section = %+v", section)
	}
	for _, item := range section.Items {
		if item.Kind != "exec" || item.State == "" || item.ID == other.id {
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

// Several storages feed one run stream: the id list is newest first across managers and the
// cap is the smallest sse_max_ids, whatever order the managers are visited in.
func TestWatchSessionAsyncMergesManagersNewestFirstWithTheSmallestCap(t *testing.T) {
	f := newLayoutFixture(t)
	small := asyncmgr.DefaultConfig()
	small.SSEMaxIDs = 3
	// The first fixture creates its host with the default config; the second is created
	// directly with the small cap, so the two managers disagree.
	a := newProcessHandleFixture(t, f, "merge-a")
	b := newProcessHandleFixtureWith(t, f, "merge-b", small)

	owner := a.key(t)
	reference, profile, _ := strings.Cut(owner, "\x00")
	read, stop := f.service.resolver.WatchSessionAsync(reference, profile, func() {})
	if read == nil {
		t.Fatal("no watcher")
	}
	defer stop()

	var started []string
	for _, p := range []processHandleFixture{a, b, a, b, a} {
		h := p.start(t, owner, "sleep 60")
		defer p.stop(h)
		started = append(started, h.id)
		time.Sleep(2 * time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		state := read()
		if state.Active != 5 || len(state.IDs) != 3 {
			t.Fatalf("state = %+v, want 5 active and the 3 newest ids", state)
		}
		for j, want := range []string{started[4], started[3], started[2]} {
			if state.IDs[j] != want {
				t.Fatalf("ids = %v, want newest first %v", state.IDs, []string{started[4], started[3], started[2]})
			}
		}
	}
}
