package native

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

// get returns the run with the given id, or nil when it is unknown or another session's.
func (m *SubagentManager) get(id, owner string) *subagentRun {
	if run := m.byID(id); run != nil && run.owner == owner {
		return run
	}
	return nil
}

var subagentRunIDPattern = regexp.MustCompile(`sub_[0-9a-f]{8}`)

// blockedLauncher holds each child until release closes or its context ends, and
// records whether any child saw its context cancelled.
func blockedLauncher(release chan struct{}, cancelled *atomic.Int64) *fakeLauncher {
	return &fakeLauncher{run: func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
		select {
		case <-release:
			return SubagentResult{Text: "late answer", Rounds: 4, Tokens: 900}, nil
		case <-ctx.Done():
			if cancelled != nil {
				cancelled.Add(1)
			}
			return SubagentResult{Rounds: 1, Tokens: 10}, ctx.Err()
		}
	}}
}

func runIDOf(t *testing.T, output string) string {
	t.Helper()
	id := subagentRunIDPattern.FindString(output)
	if id == "" {
		t.Fatalf("no run id in:\n%s", output)
	}
	return id
}

func TestSubagentSlowChildReturnsARunIdAndKeepsRunning(t *testing.T) {
	release := make(chan struct{})
	var cancelled atomic.Int64
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), blockedLauncher(release, &cancelled), readOnlyTool("read"))

	started := time.Now()
	output := subagentCall(t, catalog, `{"task":"slow search","block_ms":30}`)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("a 30 ms block took %v", elapsed)
	}
	for _, want := range []string{`"status":"ok"`, "still running", "Do not start the task again", "async tool", "state: running"} {
		if !strings.Contains(output, want) {
			t.Errorf("handoff lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "error_code") {
		t.Errorf("a handoff is not an error:\n%s", output)
	}
	id := runIDOf(t, output)
	if cancelled.Load() != 0 {
		t.Fatal("the child was cancelled when the call stopped waiting")
	}

	close(release)
	collected := asyncWait(t, catalog)
	for _, want := range []string{id, "done", "late answer"} {
		if !strings.Contains(collected, want) {
			t.Errorf("collected result lacks %q:\n%s", want, collected)
		}
	}
}

func TestSubagentAsyncWaitNeverStartsASecondChild(t *testing.T) {
	release := make(chan struct{})
	launcher := blockedLauncher(release, nil)
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"))
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":10}`))
	for range 3 {
		asyncCall(t, catalog, `{"action":"status","id":"`+id+`"}`)
	}
	close(release)
	asyncWait(t, catalog)
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if len(launcher.requests) != 1 {
		t.Fatalf("%d children started, want 1", len(launcher.requests))
	}
}

func TestSubagentFastChildStillAnswersInline(t *testing.T) {
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), &fakeLauncher{}, readOnlyTool("read"))
	output := subagentCall(t, catalog, `{"task":"quick"}`)
	if !strings.Contains(output, "Agent explore finished") || strings.Contains(output, "still") {
		t.Fatalf("a fast child did not answer inline:\n%s", output)
	}
	if out := asyncCall(t, catalog, `{"action":"wait","wait_ms":0}`); strings.Contains(out, "sub_") {
		t.Errorf("an answer returned inline is signalled again:\n%s", out)
	}
}

func TestSubagentZeroBlockDetachesAtOnce(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), blockedLauncher(release, nil), readOnlyTool("read"))
	output := subagentCall(t, catalog, `{"task":"x","block_ms":0}`)
	if !strings.Contains(output, "still") || !strings.Contains(output, "sub_") {
		t.Fatalf("block_ms 0 did not return a run id:\n%s", output)
	}
}

func TestSubagentBlockIsClampedToTheAsyncCap(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	config := asyncmgr.DefaultConfig()
	config.DefaultBlockMs, config.MaxBlockMs = 10, 40
	f := newSubagentAsyncFixture(t, config, subagentSettings(agentNamed("explore")), blockedLauncher(release, nil))
	started := time.Now()
	output := f.start(t, "s1", map[string]any{"task": "x", "block_ms": int64(3600000)})
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("an hour-long block was not clamped to 40 ms: %v", elapsed)
	}
	if !strings.Contains(output, "still running") {
		t.Fatalf("output:\n%s", output)
	}
}

func TestSubagentAsyncStopCancelsADetachedChild(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var cancelled atomic.Int64
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), blockedLauncher(release, &cancelled), readOnlyTool("read"))
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":10}`))

	output := asyncCall(t, catalog, `{"action":"stop","id":"`+id+`"}`)
	if !strings.Contains(output, id) || !strings.Contains(output, "stopped") {
		t.Fatalf("stop result:\n%s", output)
	}
	if cancelled.Load() != 1 {
		t.Fatalf("the child saw %d cancellations, want 1", cancelled.Load())
	}
	// Stopping again is harmless.
	if again := asyncCall(t, catalog, `{"action":"stop","id":"`+id+`"}`); strings.Contains(again, "error_code: internal") {
		t.Fatalf("second stop:\n%s", again)
	}
}

func TestSubagentTimeoutAfterDetachingIsSignalledAsTimedOut(t *testing.T) {
	agent := agentNamed("slow")
	agent.TimeoutMS = 60
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agent), blockedLauncher(make(chan struct{}), nil), readOnlyTool("read"))
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":5}`))
	output := asyncWait(t, catalog)
	for _, want := range []string{id, "timed_out"} {
		if !strings.Contains(output, want) {
			t.Errorf("signal lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, `"ok":true`) {
		t.Errorf("a timed out run is ok:\n%s", output)
	}
}

func TestSubagentCancellingTheStartCallDoesNotStopTheRun(t *testing.T) {
	release := make(chan struct{})
	var cancelled atomic.Int64
	launcher := blockedLauncher(release, &cancelled)
	catalog, tool := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tool.Execute(ctx, map[string]any{"task": "x", "block_ms": int64(5000)}, tools.ExecutionContext{CallID: "c", SessionKey: testSessionKey})
		done <- err
	}()
	eventually(t, "the child to start", func() bool { return launcher.count() == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if cancelled.Load() != 0 {
		t.Fatal("cancelling the start call cancelled the run")
	}
	close(release)
	if out := asyncWait(t, catalog); !strings.Contains(out, "late answer") {
		t.Fatalf("the run did not survive:\n%s", out)
	}
}

func TestSubagentRunWithoutAHandleStillDiesWithItsCall(t *testing.T) {
	var cancelled atomic.Int64
	launcher := blockedLauncher(make(chan struct{}), &cancelled)
	_, tool := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tool.Execute(ctx, map[string]any{"task": "x"}, tools.ExecutionContext{CallID: "c"})
		done <- err
	}()
	eventually(t, "the child to start", func() bool { return launcher.count() == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if cancelled.Load() != 1 {
		t.Fatalf("the child saw %d cancellations, want 1 (and it must be over when the call returns)", cancelled.Load())
	}
}

func TestSubagentRemovedArgumentsAreRejected(t *testing.T) {
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), &fakeLauncher{}, readOnlyTool("read"))
	for name, arguments := range map[string]string{
		"id":      `{"id":"sub_00000000"}`,
		"stop":    `{"id":"sub_00000000","stop":true}`,
		"wait_ms": `{"task":"x","wait_ms":5}`,
	} {
		out := subagentCall(t, catalog, arguments)
		if !strings.Contains(out, "error_code: invalid_input") && !strings.Contains(out, "additional") && !strings.Contains(out, "not allowed") {
			t.Errorf("%s was not rejected:\n%s", name, out)
		}
	}
	// A blank task lists the agents.
	if out := subagentCall(t, catalog, `{"task":"  "}`); !strings.Contains(out, "1 agent available") {
		t.Errorf("blank task:\n%s", out)
	}
	// A negative block means no block: the run is handed off at once.
	release := make(chan struct{})
	defer close(release)
	slow, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), blockedLauncher(release, nil), readOnlyTool("read"))
	if out := subagentCall(t, slow, `{"task":"x","block_ms":-5}`); !strings.Contains(out, "still running") {
		t.Errorf("negative block:\n%s", out)
	}
}

func TestSubagentRefusesARunWhenTheQueueIsFull(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	settings := subagentSettings(agentNamed("explore"))
	settings.MaxConcurrent = 1
	settings.MaxQueued = 1
	catalog, _ := buildSubagentCatalog(t, settings, blockedLauncher(release, nil), readOnlyTool("read"))

	first := subagentCall(t, catalog, `{"task":"a","block_ms":0}`)
	second := subagentCall(t, catalog, `{"task":"b","block_ms":0}`)
	third := subagentCall(t, catalog, `{"task":"c","block_ms":0}`)
	if !strings.Contains(first, "state: running") {
		t.Errorf("first run:\n%s", first)
	}
	if !strings.Contains(second, "state: queued") || !strings.Contains(second, "still queued") {
		t.Errorf("second run should wait for the slot:\n%s", second)
	}
	if !strings.Contains(third, "error_code: subagent_busy") {
		t.Errorf("third run should be refused:\n%s", third)
	}
	if list := asyncCall(t, catalog, `{"action":"list"}`); strings.Count(list, `"kind":"subagent","state"`) != 2 {
		t.Errorf("the refused run left a handle:\n%s", list)
	}
}

func TestSubagentQueuedRunStartsWhenASlotFrees(t *testing.T) {
	release := make(chan struct{})
	settings := subagentSettings(agentNamed("explore"))
	settings.MaxConcurrent = 1
	catalog, _ := buildSubagentCatalog(t, settings, blockedLauncher(release, nil), readOnlyTool("read"))
	subagentCall(t, catalog, `{"task":"a","block_ms":0}`)
	queued := runIDOf(t, subagentCall(t, catalog, `{"task":"b","block_ms":0}`))
	if out := asyncCall(t, catalog, `{"action":"status","id":"`+queued+`"}`); !strings.Contains(out, "queued") {
		t.Errorf("the waiting run is not queued:\n%s", out)
	}
	close(release)
	eventually(t, "the queued run to finish", func() bool {
		return strings.Contains(asyncCall(t, catalog, `{"action":"status","id":"`+queued+`"}`), "late answer")
	})
}

func TestSubagentCountsRunningChildrenAcrossToolsSharingAManager(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	manager := NewSubagentManager()
	defer manager.Close()
	host := newTestAsyncHost(t)
	settings := subagentSettings(agentNamed("explore"))
	settings.MaxConcurrent = 1
	settings.MaxQueued = 0
	launcher := blockedLauncher(release, nil)
	var calls [2]*tools.Catalog
	for index := range calls {
		parent := withExecutors(t, readOnlyTool("read"))
		tool, err := newSubagentToolAsync(settings, parent, nil, launcher, nil, manager, nil, host)
		if err != nil {
			t.Fatal(err)
		}
		calls[index] = catalogWithAsync(t, host, parent, tool)
	}
	if out := subagentCall(t, calls[0], `{"task":"a","block_ms":0}`); !strings.Contains(out, "state: running") {
		t.Fatalf("first:\n%s", out)
	}
	if out := subagentCall(t, calls[1], `{"task":"b","block_ms":0}`); !strings.Contains(out, "error_code: subagent_busy") {
		t.Fatalf("a second request did not see the first request's child:\n%s", out)
	}
}

func TestSubagentKeepsOnlyTheNewestFinishedRuns(t *testing.T) {
	settings := subagentSettings(agentNamed("explore"))
	settings.KeepFinished = 2
	manager := NewSubagentManager()
	defer manager.Close()
	host := newTestAsyncHost(t)
	// Each child waits for a token, so the call that starts it always hands off: with an
	// instant child, a zero block could return the answer and no id.
	proceed := make(chan struct{})
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentToolAsync(settings, parent, nil, &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		<-proceed
		return SubagentResult{Text: "found it", Rounds: 2, Tokens: 1234}, nil
	}}, nil, manager, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog := catalogWithAsync(t, host, parent, tool)
	var ids []string
	for range 4 {
		ids = append(ids, runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":0}`)))
		proceed <- struct{}{}
		waitUntilListed(t, catalog, ids[len(ids)-1], "done")
	}
	for index, id := range ids {
		dropped := manager.get(id, testSessionKey) == nil
		if wantDropped := index < 2; dropped != wantDropped {
			t.Errorf("run %d dropped = %v, want %v", index, dropped, wantDropped)
		}
	}
}

func waitUntilListed(t *testing.T, catalog *tools.Catalog, id, state string) {
	t.Helper()
	eventually(t, fmt.Sprintf("run %s to reach %s", id, state), func() bool {
		return strings.Contains(asyncCall(t, catalog, `{"action":"status","id":"`+id+`"}`), state)
	})
}

func TestSubagentNeverDropsARunThatIsStillGoing(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	settings := subagentSettings(agentNamed("explore"))
	settings.KeepFinished = 1
	settings.MaxConcurrent = 8
	catalog, _ := buildSubagentCatalog(t, settings, blockedLauncher(release, nil), readOnlyTool("read"))
	var ids []string
	for range 4 {
		ids = append(ids, runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":0}`)))
	}
	for _, id := range ids {
		if out := asyncCall(t, catalog, `{"action":"status","id":"`+id+`"}`); strings.Contains(out, "unknown_handle") {
			t.Fatalf("running run %s was dropped:\n%s", id, out)
		}
	}
}

func TestSubagentAsyncListShowsTheRunsWithTheirState(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), blockedLauncher(release, nil), readOnlyTool("read"))
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"find the retry delay in the provider","block_ms":0}`))
	list := asyncCall(t, catalog, `{"action":"list"}`)
	for _, want := range []string{id, "subagent", "running", "find the retry delay in the provider"} {
		if !strings.Contains(list, want) {
			t.Errorf("list lacks %q:\n%s", want, list)
		}
	}
	if agents := subagentCall(t, catalog, `{}`); strings.Contains(agents, "Runs:") || strings.Contains(agents, id) {
		t.Errorf("the agent list shows runs:\n%s", agents)
	}
}

func TestSubagentTheHandleKeepsOnlyAShortTaskLabel(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), blockedLauncher(release, nil), readOnlyTool("read"))
	long := strings.Repeat("secret-detail ", 50)
	subagentCall(t, catalog, `{"task":"`+long+`","block_ms":0}`)
	list := asyncCall(t, catalog, `{"action":"list"}`)
	if strings.Contains(list, long) || !strings.Contains(list, "explore: secret-detail") {
		t.Fatalf("the handle holds the whole task:\n%s", list)
	}
}

func TestSubagentCutsALateAnswerToTheResultLimit(t *testing.T) {
	long := strings.Repeat("é", 2000)
	proceed := make(chan struct{})
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		<-proceed
		return SubagentResult{Text: long, Rounds: 1}, nil
	}}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"))
	runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":0}`))
	close(proceed)
	out := asyncWait(t, catalog)
	if !strings.Contains(out, `"cut":true`) {
		t.Fatalf("a cut answer is not marked:\n%s", out)
	}
	if strings.Contains(out, strings.Repeat("é", 600)) {
		t.Fatalf("the signal carries more than the summary cap")
	}
}

func TestSubagentLauncherPanicBecomesAFailedRun(t *testing.T) {
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) { panic("boom") }}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"))
	out := subagentCall(t, catalog, `{"task":"x"}`)
	if !strings.Contains(out, "error_code: subagent_failed") || !strings.Contains(out, "panicked") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestSubagentManagerCloseStopsDetachedChildrenAndWaitsForThem(t *testing.T) {
	var cancelled atomic.Int64
	manager := NewSubagentManager()
	host := newTestAsyncHost(t)
	settings := subagentSettings(agentNamed("explore"))
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentToolAsync(settings, parent, nil, blockedLauncher(make(chan struct{}), &cancelled), nil, manager, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog := catalogWithAsync(t, host, parent, tool)
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":0}`))

	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if cancelled.Load() != 1 {
		t.Fatalf("Close left the child running: %d cancellations", cancelled.Load())
	}
	if out := asyncCall(t, catalog, `{"action":"status","id":"`+id+`"}`); !strings.Contains(out, "stopped") {
		t.Fatalf("run after Close:\n%s", out)
	}
	if out := subagentCall(t, catalog, `{"task":"y"}`); !strings.Contains(out, "error_code: subagent_unavailable") {
		t.Fatalf("a start after Close:\n%s", out)
	}
	if counts := host.Manager().Counts(testSessionKey); counts.Active != 0 {
		t.Fatalf("a refused start left a live handle: %+v", counts)
	}
}

type panickingProcesses struct{ ProcessManager }

func (panickingProcesses) ownerOf(string) (string, bool) { return "", false }
func (panickingProcesses) stopOwned(string, string) int  { panic("sweep exploded") }

type lockedLog struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

func TestSubagentPanicAfterTheLauncherFailsTheRunInsteadOfEndingTheServer(t *testing.T) {
	logged := &lockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), parent, nil, &fakeLauncher{}, nil, manager, panickingProcesses{}, newTestAsyncHost(t))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	out, returned := callReturnsWithin(t, catalog, `{"task":"x"}`, 5*time.Second)
	if !returned {
		t.Fatal("the call never returned: the run was left going")
	}
	if !strings.Contains(out, "error_code: subagent_failed") || strings.Contains(out, "sweep exploded") {
		t.Fatalf("output:\n%s", out)
	}
	if views := manager.list(testSessionKey); len(views) != 1 || views[0].state != subagentRunFailed {
		t.Fatalf("runs after the panic: %+v", views)
	}
	if text := logged.String(); !strings.Contains(text, "sweep exploded") || !strings.Contains(text, "stack=") {
		t.Fatalf("the panic left no log line with its value and stack:\n%s", text)
	}
}

func TestSubagentLauncherPanicTextStaysOutOfTheRunReason(t *testing.T) {
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) { panic("secret /srv/path") }}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"))
	if out := subagentCall(t, catalog, `{"task":"x"}`); strings.Contains(out, "secret /srv/path") {
		t.Fatalf("the panic value reached the result:\n%s", out)
	}
}

// A run keep_finished drops is reported to the hook once, with its id, after the manager's lock is
// released, so the hook may call back into the manager.
func TestDroppedRunsAreReportedToTheHook(t *testing.T) {
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	var dropped []string
	manager.SetOnDropped(func(identity any) {
		manager.mu.Lock()
		manager.mu.Unlock()
		dropped = append(dropped, identity.(*subagentRun).id)
	})
	manager.mu.Lock()
	for _, id := range []string{"sub_00000001", "sub_00000002"} {
		manager.runs[id] = &subagentRun{id: id}
		manager.finished = append(manager.finished, id)
	}
	manager.mu.Unlock()
	manager.removeDropped([]droppedRun{{id: "sub_00000001", run: manager.runs["sub_00000001"]}})
	if len(dropped) != 1 || dropped[0] != "sub_00000001" {
		t.Fatalf("the hook was told %v, want the dropped run only", dropped)
	}
}
