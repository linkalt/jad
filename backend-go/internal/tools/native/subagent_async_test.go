package native

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type subagentAsyncFixture struct {
	host    *AsyncHost
	manager *SubagentManager
	tool    tools.Tool
	async   tools.Tool
	dir     string
}

func newSubagentAsyncFixture(t *testing.T, config asyncmgr.Config, settings sessionfs.NativeSubagentSettings, launcher SubagentLauncher) subagentAsyncFixture {
	t.Helper()
	dir := t.TempDir()
	host, err := NewAsyncHost(filepath.Join(dir, ".async"), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: filepath.Join(dir, "sub"), Keep: 10})
	t.Cleanup(func() { _ = manager.Close() })
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentToolAsync(settings, parent, nil, launcher, nil, manager, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	asyncTool, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	return subagentAsyncFixture{host: host, manager: manager, tool: tool, async: asyncTool, dir: dir}
}

func (f subagentAsyncFixture) start(t *testing.T, owner string, arguments map[string]any) string {
	t.Helper()
	if f.tool.NormalizeArguments != nil {
		arguments = f.tool.NormalizeArguments(arguments)
	}
	out, err := f.tool.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "s1", WorkspaceRoot: f.dir, SessionDir: f.dir, SessionKey: owner})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSubagentRunIsAnAsyncHandleOwnedByTheSession(t *testing.T) {
	release := make(chan struct{})
	f := newSubagentAsyncFixture(t, asyncmgr.DefaultConfig(), subagentSettings(agentNamed("explore")), blockedLauncher(release, nil))
	id := runIDOf(t, f.start(t, "s1", map[string]any{"task": "find the retry delay", "block_ms": int64(5)}))
	entry, err := f.host.Manager().Status("s1", id)
	if err != nil || entry.Kind != "subagent" || entry.State != asyncmgr.StateRunning || !strings.Contains(entry.Label, "explore") {
		t.Fatalf("entry = %+v, err = %v", entry, err)
	}
	if _, err := f.host.Manager().Status("s2", id); err == nil {
		t.Fatal("another session sees the handle")
	}
	if detail := callAsync(t, f.async, "s1", map[string]any{"action": "status", "id": id}); !strings.Contains(detail, "find the retry delay") {
		t.Errorf("status lacks the task:\n%s", detail)
	}
	close(release)
	out := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "wait_ms": int64(5000)})
	for _, want := range []string{id, "done", "late answer", filepath.Join(f.dir, "sub", ".runs", id+".json")} {
		if !strings.Contains(out, want) {
			t.Errorf("wait output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\"ok\":true") {
		t.Errorf("want ok:true:\n%s", out)
	}
}

func TestSubagentAsyncStatesKeepTheirNameAndAreNotOK(t *testing.T) {
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		if strings.Contains(request.Task, "boom") {
			return SubagentResult{}, errors.New("provider exploded")
		}
		<-ctx.Done()
		return SubagentResult{Text: "partial work"}, ctx.Err()
	}}
	settings := subagentSettings(agentNamed("explore"))
	settings.Agents[0].TimeoutMS = 100
	f := newSubagentAsyncFixture(t, asyncmgr.DefaultConfig(), settings, launcher)
	failed := runIDOf(t, f.start(t, "s1", map[string]any{"task": "boom", "block_ms": int64(0)}))
	timedOut := runIDOf(t, f.start(t, "s1", map[string]any{"task": "slow", "block_ms": int64(0)}))
	got := map[string]string{}
	for len(got) < 2 {
		out := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "wait_ms": int64(5000)})
		for _, id := range []string{failed, timedOut} {
			if strings.Contains(out, `"id":"`+id+`","kind":"subagent","state"`) {
				got[id] = out
			}
		}
	}
	if out := got[failed]; !strings.Contains(out, "failed") || !strings.Contains(out, "provider exploded") {
		t.Errorf("failed signal:\n%s", out)
	}
	if out := got[timedOut]; !strings.Contains(out, "timed_out") || !strings.Contains(out, "partial work") {
		t.Errorf("timed_out signal:\n%s", out)
	}
	for _, out := range got {
		if strings.Contains(out, "\"ok\":true") {
			t.Errorf("a non-done state is ok:\n%s", out)
		}
	}
}

func TestSubagentAsyncStopIsNonBlockingAndEndsAsStopped(t *testing.T) {
	var cancelled atomic.Int64
	f := newSubagentAsyncFixture(t, asyncmgr.DefaultConfig(), subagentSettings(agentNamed("explore")), blockedLauncher(make(chan struct{}), &cancelled))
	id := runIDOf(t, f.start(t, "s1", map[string]any{"task": "x", "block_ms": int64(0)}))
	started := time.Now()
	if n := f.host.Manager().CancelOwner("s1", "user_stop"); n != 1 {
		t.Fatalf("CancelOwner = %d", n)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Errorf("CancelOwner blocked for %v", time.Since(started))
	}
	out := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "wait_ms": int64(5000)})
	if !strings.Contains(out, id) || !strings.Contains(out, "stopped") || !strings.Contains(out, "user_stop") {
		t.Errorf("stop signal:\n%s", out)
	}
	if cancelled.Load() != 1 {
		t.Errorf("child saw %d cancellations", cancelled.Load())
	}
}

func TestSubagentStopCountsAnAsyncRunOnce(t *testing.T) {
	var cancelled atomic.Int64
	f := newSubagentAsyncFixture(t, asyncmgr.DefaultConfig(), subagentSettings(agentNamed("explore")), blockedLauncher(make(chan struct{}), &cancelled))
	id := runIDOf(t, f.start(t, "s1", map[string]any{"task": "x", "block_ms": int64(0)}))
	if n := f.manager.CancelUnmanagedOwner("s1", "user_stop"); n != 0 {
		t.Fatalf("sub-agent manager counted a handle the async manager stops: %d", n)
	}
	if n := f.manager.CancelUnmanagedOwnersWithPrefix("s1", "session_delete"); n != 0 {
		t.Fatalf("CancelUnmanagedOwnersWithPrefix counted a handle: %d", n)
	}
	if n := f.host.Manager().CancelOwner("s1", "user_stop"); n != 1 {
		t.Fatalf("async CancelOwner = %d, want 1", n)
	}
	if out := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "wait_ms": int64(5000)}); !strings.Contains(out, id) {
		t.Fatalf("stop signal:\n%s", out)
	}
}

func TestSubagentStartOverTheAsyncCapCreatesNoRun(t *testing.T) {
	config := asyncmgr.DefaultConfig()
	config.MaxHandles = 1
	release := make(chan struct{})
	defer close(release)
	f := newSubagentAsyncFixture(t, config, subagentSettings(agentNamed("explore")), blockedLauncher(release, nil))
	f.start(t, "s1", map[string]any{"task": "first", "block_ms": int64(0)})
	out := f.start(t, "s1", map[string]any{"task": "second", "block_ms": int64(0)})
	if !strings.Contains(out, "async_capacity_reached") {
		t.Fatalf("want a capacity error:\n%s", out)
	}
	if runs := f.manager.list("s1"); len(runs) != 1 {
		t.Errorf("runs = %d, want 1", len(runs))
	}
	if f.manager.queued != 0 || f.manager.running != 1 {
		t.Errorf("slots leaked: running %d queued %d", f.manager.running, f.manager.queued)
	}
}

func TestSubagentWriterIsNeverAHandle(t *testing.T) {
	settings := subagentSettings(sessionfs.NativeSubagentAgent{Name: "writer", MaxToolRounds: 5, TimeoutMS: 5000, HasTools: true, Tools: []string{"write"}})
	dir := t.TempDir()
	host, err := NewAsyncHost(filepath.Join(dir, ".async"), asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	manager := NewSubagentManager()
	defer manager.Close()
	parent := withExecutors(t, readOnlyTool("read"), writeTool("write"))
	tool, err := newSubagentToolAsync(settings, parent, nil, &fakeLauncher{}, nil, manager, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(context.Background(), map[string]any{"task": "edit"}, tools.ExecutionContext{CallID: "w", SessionKey: "s1"})
	if err != nil || !strings.Contains(out, "finished") {
		t.Fatalf("out = %s, err = %v", out, err)
	}
	if counts := host.Manager().Counts("s1"); counts.Total != 0 {
		t.Errorf("a writer run is in the async index: %+v", counts)
	}
}

func TestSubagentReportedInTheCallConsumesItsSignal(t *testing.T) {
	f := newSubagentAsyncFixture(t, asyncmgr.DefaultConfig(), subagentSettings(agentNamed("explore")), &fakeLauncher{})
	out := f.start(t, "s1", map[string]any{"task": "quick", "block_ms": int64(5000)})
	if !strings.Contains(out, "found it") {
		t.Fatalf("out = %s", out)
	}
	if counts := f.host.Manager().Counts("s1"); counts.Unread != 0 || counts.Total != 1 {
		t.Errorf("counts = %+v, want the finished run read", counts)
	}
}

// The files a writer changed are put ahead of its summary, and the whole summary stays
// within the run's result limit: the files line does not add to the cut answer.
func TestSubagentAsyncSummaryIsNotPushedPastTheResultLimit(t *testing.T) {
	const limit = 1024
	var files []subagentFileChange
	for i := range 20 {
		files = append(files, subagentFileChange{Path: fmt.Sprintf("generated/%02d/%s.go", i, strings.Repeat("x", 60)), Action: "created"})
	}
	view := subagentRunView{
		state: subagentRunDone, tracked: true, changes: true,
		files: files, result: SubagentResult{Text: strings.Repeat("a", limit)},
	}
	if got := subagentAsyncResult(view).Summary; len(got) > limit {
		t.Fatalf("the handle's summary is %d bytes, over the result limit %d", len(got), limit)
	}
}

func TestSubagentBlockForWithoutAnAsyncHostIsZero(t *testing.T) {
	if got := (&subagentTool{}).blockFor(map[string]any{"block_ms": int64(5)}); got != 0 {
		t.Fatalf("blockFor without a host = %v, want 0", got)
	}
}
