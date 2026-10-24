package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// controlRun is a message the test ends by hand.
type controlRun struct {
	queued  bool
	started chan struct{}
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	outcome ProfileMessageOutcome
	stops   []string
}

func (r *controlRun) Queued() bool             { return r.queued }
func (r *controlRun) Started() <-chan struct{} { return r.started }
func (r *controlRun) Done() <-chan struct{}    { return r.done }
func (r *controlRun) Status() string           { return "control run" }
func (r *controlRun) Outcome() ProfileMessageOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcome
}

func (r *controlRun) begin() {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
}

func (r *controlRun) finish(outcome ProfileMessageOutcome) {
	r.once.Do(func() {
		r.mu.Lock()
		r.outcome = outcome
		r.mu.Unlock()
		close(r.done)
	})
}

func (r *controlRun) Stop(reason string) {
	r.mu.Lock()
	r.stops = append(r.stops, reason)
	r.mu.Unlock()
	r.finish(ProfileMessageOutcome{Err: ErrProfileStopped, Reason: reason, Text: "partial answer"})
}

func (r *controlRun) stopped() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stops...)
}

// controlMessenger hands out controlRuns and records what was prepared.
type controlMessenger struct {
	mu         sync.Mutex
	queued     bool
	prepareErr error
	startErr   error
	prepared   int
	runs       []*controlRun
}

func (m *controlMessenger) Prepare(context.Context, ProfileMessageRequest) (ProfileMessagePlan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prepared++
	if m.prepareErr != nil {
		return nil, m.prepareErr
	}
	return controlPlan{m}, nil
}

type controlPlan struct{ m *controlMessenger }

func (p controlPlan) Start(context.Context) (ProfileMessageRun, error) {
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	if p.m.startErr != nil {
		return nil, p.m.startErr
	}
	run := &controlRun{queued: p.m.queued, started: make(chan struct{}), done: make(chan struct{})}
	if !p.m.queued {
		run.begin()
	}
	p.m.runs = append(p.m.runs, run)
	return run, nil
}

func (m *controlMessenger) run(t *testing.T, index int) *controlRun {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if index >= len(m.runs) {
		t.Fatalf("only %d runs started, want run %d", len(m.runs), index)
	}
	return m.runs[index]
}

type profileMessageHandleFixture struct {
	host      *AsyncHost
	messenger *controlMessenger
	message   tools.Tool
	async     tools.Tool
	dir       string
}

func newProfileMessageHandleFixture(t *testing.T, mutate func(*sessionfs.NativeProfileMessageSettings), config func(*asyncmgr.Config)) *profileMessageHandleFixture {
	t.Helper()
	cfg := asyncmgr.DefaultConfig()
	if config != nil {
		config(&cfg)
	}
	dir := t.TempDir()
	host, err := NewAsyncHost(filepath.Join(dir, ".async"), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	settings := sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024, MaxQueued: 4}
	if mutate != nil {
		mutate(&settings)
	}
	messenger := &controlMessenger{}
	message, err := NewProfileMessageTool(settings, profileMessageTargets, messenger, host)
	if err != nil {
		t.Fatal(err)
	}
	async, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	return &profileMessageHandleFixture{host: host, messenger: messenger, message: message, async: async, dir: dir}
}

var handleIDPattern = regexp.MustCompile(`pm_[0-9a-f]{8}`)

func (f *profileMessageHandleFixture) send(t *testing.T, execution tools.ExecutionContext, block int64) string {
	t.Helper()
	if execution.CallID == "" {
		execution.CallID = "call"
	}
	out, err := f.message.Execute(context.Background(), map[string]any{"profile": "reviewer", "message": "check it", "block_ms": json.Number(fmt.Sprint(block))}, execution)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *profileMessageHandleFixture) asyncCall(t *testing.T, execution tools.ExecutionContext, arguments map[string]any) string {
	t.Helper()
	execution.CallID = "async"
	out, err := f.async.Execute(context.Background(), arguments, execution)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var sessionCaller = tools.ExecutionContext{SessionKey: "s1:main"}

func TestProfileMessageAnsweringWithinBlockMsReturnsTheAnswerAndConsumesTheSignal(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	go func() {
		for {
			f.messenger.mu.Lock()
			n := len(f.messenger.runs)
			f.messenger.mu.Unlock()
			if n > 0 {
				f.messenger.run(t, 0).finish(ProfileMessageOutcome{Text: "looks fine"})
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	out := f.send(t, sessionCaller, 5000)
	if !strings.Contains(out, "looks fine") || !strings.Contains(out, "answered") || strings.Contains(out, "Do not send") {
		t.Fatalf("output = %s", out)
	}
	if wait := f.asyncCall(t, sessionCaller, map[string]any{"action": "wait", "wait_ms": json.Number("0")}); !strings.Contains(wait, "0 signal(s), 0 pending") {
		t.Fatalf("the answer was returned inline, so no signal may remain:\n%s", wait)
	}
}

func TestProfileMessageOutlastingBlockMsBecomesAHandleTheAsyncToolCollects(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	out := f.send(t, sessionCaller, 0)
	id := handleIDPattern.FindString(out)
	if id == "" || !strings.Contains(out, "Do not send the message again") || !strings.Contains(out, "state: running") {
		t.Fatalf("no handle in %s", out)
	}
	if list := f.asyncCall(t, sessionCaller, map[string]any{"action": "list"}); !strings.Contains(list, id) || !strings.Contains(list, "profile_message") {
		t.Fatalf("list lacks the handle:\n%s", list)
	}
	if status := f.asyncCall(t, sessionCaller, map[string]any{"action": "status", "id": id}); !strings.Contains(status, "control run") {
		t.Fatalf("status of a live message lacks the driver's detail:\n%s", status)
	}
	f.messenger.run(t, 0).finish(ProfileMessageOutcome{Text: "the answer", Trace: "demo/review/reviewer#turn_1"})
	wait := f.asyncCall(t, sessionCaller, map[string]any{"action": "wait", "wait_ms": json.Number("5000")})
	if !strings.Contains(wait, id) || !strings.Contains(wait, "the answer") || !strings.Contains(wait, "done") {
		t.Fatalf("wait did not deliver the answer:\n%s", wait)
	}
	if strings.Contains(wait, "turn_1") {
		t.Fatalf("the trace reference was returned to the model:\n%s", wait)
	}
	if again := f.asyncCall(t, sessionCaller, map[string]any{"action": "wait", "wait_ms": json.Number("0")}); strings.Contains(again, "the answer") {
		t.Fatalf("the answer was delivered twice:\n%s", again)
	}
	record, err := f.host.Manager().Status("s1:main", id)
	if err != nil || !strings.Contains(record.Summary, "the answer") {
		t.Fatalf("status after the end = %+v, %v; want the stored answer", record, err)
	}
	files, _ := filepath.Glob(filepath.Join(f.dir, ".async", id+".json"))
	if len(files) != 1 {
		t.Fatalf("record files = %v", files)
	}
}

func TestProfileMessageQueuedHandleIsQueuedUntilTheTargetIsFree(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	f.messenger.queued = true
	out := f.send(t, sessionCaller, 0)
	id := handleIDPattern.FindString(out)
	if id == "" || !strings.Contains(out, "state: queued") || !strings.Contains(out, "waits its turn") {
		t.Fatalf("output = %s", out)
	}
	if list := f.asyncCall(t, sessionCaller, map[string]any{"action": "list", "state": "active"}); !strings.Contains(list, `"state":"queued"`) {
		t.Fatalf("list does not show the queued state:\n%s", list)
	}
	f.messenger.run(t, 0).begin()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list := f.asyncCall(t, sessionCaller, map[string]any{"action": "list", "state": "active"})
		if strings.Contains(list, `"state":"running"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the handle never became running:\n%s", list)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestProfileMessageStopOfAQueuedAndOfARunningHandle(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	f.messenger.queued = true
	queued := handleIDPattern.FindString(f.send(t, sessionCaller, 0))
	f.messenger.queued = false
	running := handleIDPattern.FindString(f.send(t, sessionCaller, 0))
	for _, id := range []string{queued, running} {
		out := f.asyncCall(t, sessionCaller, map[string]any{"action": "stop", "id": id})
		if !strings.Contains(out, "stopped") {
			t.Fatalf("stop of %s:\n%s", id, out)
		}
	}
	if reasons := f.messenger.run(t, 0).stopped(); len(reasons) != 1 {
		t.Fatalf("the queued message was asked to stop %v", reasons)
	}
	if reasons := f.messenger.run(t, 1).stopped(); len(reasons) != 1 {
		t.Fatalf("the running message was asked to stop %v", reasons)
	}
	if status := f.asyncCall(t, sessionCaller, map[string]any{"action": "status", "id": running}); !strings.Contains(status, "partial answer") || !strings.Contains(status, "profile_stopped") {
		t.Fatalf("a stopped message must keep the partial text:\n%s", status)
	}
}

func TestProfileMessageHandlesBelongToTheirOwner(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	id := handleIDPattern.FindString(f.send(t, sessionCaller, 0))
	other := tools.ExecutionContext{SessionKey: "s2:main"}
	for _, action := range []string{"status", "stop"} {
		if out := f.asyncCall(t, other, map[string]any{"action": action, "id": id}); !strings.Contains(out, "unknown_handle") {
			t.Fatalf("%s from another session:\n%s", action, out)
		}
	}
	if reasons := f.messenger.run(t, 0).stopped(); len(reasons) != 0 {
		t.Fatalf("another session stopped the message: %v", reasons)
	}
}

func TestProfileMessageFailureKeepsThePartialTextAndNamesTheCode(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	id := handleIDPattern.FindString(f.send(t, sessionCaller, 0))
	f.messenger.run(t, 0).finish(ProfileMessageOutcome{Text: "half an answer", Err: &ProfileFailedError{Message: "The provider request failed (HTTP 500)."}})
	wait := f.asyncCall(t, sessionCaller, map[string]any{"action": "wait", "wait_ms": json.Number("5000")})
	for _, want := range []string{id, "failed", "half an answer", "profile_failed", "HTTP 500"} {
		if !strings.Contains(wait, want) {
			t.Errorf("wait lacks %q:\n%s", want, wait)
		}
	}
	timed := handleIDPattern.FindString(f.send(t, sessionCaller, 0))
	f.messenger.run(t, 1).finish(ProfileMessageOutcome{Err: ErrProfileTimeout})
	if wait := f.asyncCall(t, sessionCaller, map[string]any{"action": "wait", "wait_ms": json.Number("5000")}); !strings.Contains(wait, timed) || !strings.Contains(wait, "timed_out") || !strings.Contains(wait, "did not answer within") {
		t.Fatalf("a timeout is not reported:\n%s", wait)
	}
}

func TestProfileMessageRefusedBeforeAnyHandleExists(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	f.messenger.prepareErr = ErrProfileCycle
	if out := f.send(t, sessionCaller, 0); !strings.Contains(out, "profile_cycle") {
		t.Fatalf("output = %s", out)
	}
	f.messenger.prepareErr = &ProfileBusyError{QueueFull: 4}
	if out := f.send(t, sessionCaller, 0); !strings.Contains(out, "profile_busy") || !strings.Contains(out, "already waiting") {
		t.Fatalf("output = %s", out)
	}
	f.messenger.prepareErr = nil
	f.messenger.startErr = &ProfileBusyError{QueueFull: 4}
	if out := f.send(t, sessionCaller, 0); !strings.Contains(out, "profile_busy") {
		t.Fatalf("output = %s", out)
	}
	list := f.asyncCall(t, sessionCaller, map[string]any{"action": "list", "state": "all"})
	if strings.Contains(list, "pm_") && strings.Contains(list, `"state":"running"`) {
		t.Fatalf("a refused message left a live handle:\n%s", list)
	}
}

func TestProfileMessageAtTheHandleCapStartsNothing(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, func(c *asyncmgr.Config) { c.MaxHandles = 1 })
	f.send(t, sessionCaller, 0)
	if out := f.send(t, sessionCaller, 0); !strings.Contains(out, "async_capacity_reached") {
		t.Fatalf("output = %s", out)
	}
	f.messenger.mu.Lock()
	started := len(f.messenger.runs)
	f.messenger.mu.Unlock()
	if started != 1 {
		t.Fatalf("%d messages were started, want only the first", started)
	}
}

func TestProfileMessageWithoutTheAsyncHostWaitsInlineAndCancelStopsIt(t *testing.T) {
	messenger := &controlMessenger{}
	tool, err := NewProfileMessageTool(sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024}, profileMessageTargets, messenger, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() {
		out, _ := tool.Execute(ctx, map[string]any{"profile": "reviewer", "message": "hi", "block_ms": json.Number("0")}, tools.ExecutionContext{CallID: "c", SessionKey: "s1:main"})
		done <- out
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		messenger.mu.Lock()
		n := len(messenger.runs)
		messenger.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the message never started")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case out := <-done:
		if !strings.Contains(out, "cancelled") {
			t.Fatalf("output = %s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an inline call did not end with its context")
	}
	if reasons := messenger.run(t, 0).stopped(); len(reasons) != 1 {
		t.Fatalf("the cancelled call left its message running: %v", reasons)
	}
}

func TestProfileMessageFromAChildIsAHandleOnlyWhenTheChildHasTheAsyncTool(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	withAsync := tools.ExecutionContext{SessionKey: "s1:main", ProcessOwner: "sub_00000001", ProcessAsync: true, AgentDepth: 1}
	id := handleIDPattern.FindString(f.send(t, withAsync, 0))
	if id == "" {
		t.Fatal("a child with the async tool got no handle")
	}
	if list := f.asyncCall(t, sessionCaller, map[string]any{"action": "list", "state": "all"}); strings.Contains(list, id) {
		t.Fatalf("the session sees its child's handle:\n%s", list)
	}
	if list := f.asyncCall(t, withAsync, map[string]any{"action": "list", "state": "all"}); !strings.Contains(list, id) {
		t.Fatalf("the child does not see its own handle:\n%s", list)
	}
	// A child without the async tool cannot collect a handle, so the call waits for the end.
	without := tools.ExecutionContext{CallID: "c2", SessionKey: "s1:main", ProcessOwner: "sub_00000002", AgentDepth: 1}
	go func() {
		for {
			f.messenger.mu.Lock()
			n := len(f.messenger.runs)
			f.messenger.mu.Unlock()
			if n >= 2 {
				f.messenger.run(t, 1).finish(ProfileMessageOutcome{Text: "inline answer"})
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	out, err := f.message.Execute(context.Background(), map[string]any{"profile": "reviewer", "message": "hi", "block_ms": json.Number("0")}, without)
	if err != nil || !strings.Contains(out, "inline answer") {
		t.Fatalf("output = %s, err %v", out, err)
	}
}

func TestProfileMessageHandleFoundRunningAfterARestartIsLost(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	id := handleIDPattern.FindString(f.send(t, sessionCaller, 0))
	second, err := NewAsyncHost(filepath.Join(f.dir, ".async"), asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.Manager().AddDriver(&profileMessageDriver{host: second}); err != nil {
		t.Fatal(err)
	}
	entry, err := second.Manager().Status("s1:main", id)
	if err != nil || entry.State != asyncmgr.StateLost || entry.Reason != asyncmgr.ReasonRestarted {
		t.Fatalf("after a restart the handle = %+v, %v; want lost", entry, err)
	}
}

func TestProfileMessageHandleCyclesLeaveNoGoroutines(t *testing.T) {
	baseline := baselineCount()
	for range 5 {
		f := newProfileMessageHandleFixture(t, nil, nil)
		f.messenger.queued = true
		queued := handleIDPattern.FindString(f.send(t, sessionCaller, 0))
		f.messenger.queued = false
		running := handleIDPattern.FindString(f.send(t, sessionCaller, 0))
		finished := handleIDPattern.FindString(f.send(t, sessionCaller, 0))
		f.messenger.run(t, 2).finish(ProfileMessageOutcome{Text: "done"})
		f.asyncCall(t, sessionCaller, map[string]any{"action": "stop", "id": queued})
		f.asyncCall(t, sessionCaller, map[string]any{"action": "stop", "id": running})
		f.asyncCall(t, sessionCaller, map[string]any{"action": "wait", "ids": []any{finished}, "wait_ms": json.Number("5000")})
		f.host.Close()
	}
	requireNoGrowth(t, baseline, "profile_message handles that were finished, stopped queued and stopped running")
}

var _ = errors.Is
