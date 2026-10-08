package native

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/tools"
)

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
	output := subagentCall(t, catalog, `{"task":"slow search","wait_ms":30}`)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("a 30 ms wait took %v", elapsed)
	}
	for _, want := range []string{`"status":"ok"`, "still running", "Do not start the task again", "state: running"} {
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
	collected := subagentCall(t, catalog, `{"id":"`+id+`","wait_ms":2000}`)
	for _, want := range []string{"Agent explore finished", "late answer", "rounds: 4", "tokens: 900"} {
		if !strings.Contains(collected, want) {
			t.Errorf("collected result lacks %q:\n%s", want, collected)
		}
	}
}

func TestSubagentFollowUpNeverStartsASecondChild(t *testing.T) {
	release := make(chan struct{})
	launcher := blockedLauncher(release, nil)
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"))
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"x","wait_ms":10}`))
	for range 3 {
		subagentCall(t, catalog, `{"id":"`+id+`","wait_ms":0}`)
	}
	close(release)
	subagentCall(t, catalog, `{"id":"`+id+`","wait_ms":2000}`)
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
}

func TestSubagentZeroWaitDetachesAtOnce(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	settings := subagentSettings(agentNamed("explore"))
	settings.WaitMS = 0
	catalog, _ := buildSubagentCatalog(t, settings, blockedLauncher(release, nil), readOnlyTool("read"))
	output := subagentCall(t, catalog, `{"task":"x"}`)
	if !strings.Contains(output, "still") || !strings.Contains(output, "sub_") {
		t.Fatalf("wait_ms 0 did not return a run id:\n%s", output)
	}
}

func TestSubagentWaitIsClampedToTheCap(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	settings := subagentSettings(agentNamed("explore"))
	settings.MaxWaitMS = 40
	catalog, _ := buildSubagentCatalog(t, settings, blockedLauncher(release, nil), readOnlyTool("read"))
	started := time.Now()
	output := subagentCall(t, catalog, `{"task":"x","wait_ms":3600000}`)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("an hour-long wait was not clamped to 40 ms: %v", elapsed)
	}
	if !strings.Contains(output, "still running") {
		t.Fatalf("output:\n%s", output)
	}
}

func TestSubagentStopCancelsADetachedChild(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var cancelled atomic.Int64
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), blockedLauncher(release, &cancelled), readOnlyTool("read"))
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"x","wait_ms":10}`))

	output := subagentCall(t, catalog, `{"id":"`+id+`","stop":true,"wait_ms":2000}`)
	if !strings.Contains(output, "was stopped") || !strings.Contains(output, "rounds: 1") {
		t.Fatalf("stop result:\n%s", output)
	}
	if cancelled.Load() != 1 {
		t.Fatalf("the child saw %d cancellations, want 1", cancelled.Load())
	}
	// Stopping again is harmless and reports the same end.
	again := subagentCall(t, catalog, `{"id":"`+id+`","stop":true}`)
	if !strings.Contains(again, "was stopped") {
		t.Fatalf("second stop:\n%s", again)
	}
}

func TestSubagentTimeoutAfterDetachingIsReportedByTheFollowUp(t *testing.T) {
	agent := agentNamed("slow")
	agent.TimeoutMS = 60
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agent), blockedLauncher(make(chan struct{}), nil), readOnlyTool("read"))
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"x","wait_ms":5}`))
	output := subagentCall(t, catalog, `{"id":"`+id+`","wait_ms":2000}`)
	for _, want := range []string{"error_code: subagent_timeout", "did not finish within 60 ms", "rounds: 1", "tokens: 10"} {
		if !strings.Contains(output, want) {
			t.Errorf("follow-up lacks %q:\n%s", want, output)
		}
	}
}

func TestSubagentCancellingAFollowUpDoesNotStopTheRun(t *testing.T) {
	release := make(chan struct{})
	var cancelled atomic.Int64
	catalog, tool := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), blockedLauncher(release, &cancelled), readOnlyTool("read"))
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"x","wait_ms":5}`))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tool.Execute(ctx, map[string]any{"id": id, "wait_ms": int64(5000)}, tools.ExecutionContext{CallID: "c"})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if cancelled.Load() != 0 {
		t.Fatal("cancelling a follow-up call cancelled the run")
	}
	close(release)
	if out := subagentCall(t, catalog, `{"id":"`+id+`","wait_ms":2000}`); !strings.Contains(out, "late answer") {
		t.Fatalf("the run did not survive:\n%s", out)
	}
}

func TestSubagentArgumentProblemsAreCompactErrors(t *testing.T) {
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), &fakeLauncher{}, readOnlyTool("read"))
	for name, test := range map[string]struct{ arguments, want string }{
		"unknown id":          {`{"id":"sub_00000000"}`, "error_code: unknown_run"},
		"id and task":         {`{"id":"sub_00000000","task":"x"}`, "error_code: conflicting_arguments"},
		"id and agent":        {`{"id":"sub_00000000","agent":"explore"}`, "error_code: conflicting_arguments"},
		"stop no id":          {`{"stop":true}`, "error_code: missing_id"},
		"stop no id yet task": {`{"stop":true,"task":"x"}`, "error_code: missing_id"},
	} {
		if out := subagentCall(t, catalog, test.arguments); !strings.Contains(out, test.want) {
			t.Errorf("%s: output lacks %q:\n%s", name, test.want, out)
		}
	}
	// A blank id is no id.
	if out := subagentCall(t, catalog, `{"id":"  ","task":"x"}`); !strings.Contains(out, "Agent explore finished") {
		t.Errorf("blank id with task:\n%s", out)
	}
	// A negative wait means no wait: the run is handed off at once.
	release := make(chan struct{})
	defer close(release)
	slow, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), blockedLauncher(release, nil), readOnlyTool("read"))
	if out := subagentCall(t, slow, `{"task":"x","wait_ms":-5}`); !strings.Contains(out, "still running") {
		t.Errorf("negative wait:\n%s", out)
	}
}

func TestSubagentRefusesARunWhenTheQueueIsFull(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	settings := subagentSettings(agentNamed("explore"))
	settings.MaxConcurrent = 1
	settings.MaxQueued = 1
	catalog, _ := buildSubagentCatalog(t, settings, blockedLauncher(release, nil), readOnlyTool("read"))

	first := subagentCall(t, catalog, `{"task":"a","wait_ms":0}`)
	second := subagentCall(t, catalog, `{"task":"b","wait_ms":0}`)
	third := subagentCall(t, catalog, `{"task":"c","wait_ms":0}`)
	if !strings.Contains(first, "state: running") {
		t.Errorf("first run:\n%s", first)
	}
	if !strings.Contains(second, "state: queued") || !strings.Contains(second, "still queued") {
		t.Errorf("second run should wait for the slot:\n%s", second)
	}
	if !strings.Contains(third, "error_code: subagent_busy") {
		t.Errorf("third run should be refused:\n%s", third)
	}
}

func TestSubagentQueuedRunStartsWhenASlotFrees(t *testing.T) {
	release := make(chan struct{})
	settings := subagentSettings(agentNamed("explore"))
	settings.MaxConcurrent = 1
	catalog, _ := buildSubagentCatalog(t, settings, blockedLauncher(release, nil), readOnlyTool("read"))
	subagentCall(t, catalog, `{"task":"a","wait_ms":0}`)
	queued := runIDOf(t, subagentCall(t, catalog, `{"task":"b","wait_ms":0}`))
	close(release)
	out := subagentCall(t, catalog, `{"id":"`+queued+`","wait_ms":3000}`)
	if !strings.Contains(out, "late answer") {
		t.Fatalf("the queued run never ran:\n%s", out)
	}
}

func TestSubagentCountsRunningChildrenAcrossToolsSharingAManager(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	manager := NewSubagentManager()
	defer manager.Close()
	settings := subagentSettings(agentNamed("explore"))
	settings.MaxConcurrent = 1
	settings.MaxQueued = 0
	launcher := blockedLauncher(release, nil)
	var calls [2]*tools.Catalog
	for index := range calls {
		parent := withExecutors(t, readOnlyTool("read"))
		tool, err := NewSubagentToolWithManager(settings, parent, launcher, nil, manager)
		if err != nil {
			t.Fatal(err)
		}
		calls[index], err = tools.NewCatalog(append(parent, tool))
		if err != nil {
			t.Fatal(err)
		}
	}
	if out := subagentCall(t, calls[0], `{"task":"a","wait_ms":0}`); !strings.Contains(out, "state: running") {
		t.Fatalf("first:\n%s", out)
	}
	if out := subagentCall(t, calls[1], `{"task":"b","wait_ms":0}`); !strings.Contains(out, "error_code: subagent_busy") {
		t.Fatalf("a second request did not see the first request's child:\n%s", out)
	}
}

func TestSubagentKeepsOnlyTheNewestFinishedRuns(t *testing.T) {
	settings := subagentSettings(agentNamed("explore"))
	settings.KeepFinished = 2
	catalog, _ := buildSubagentCatalog(t, settings, &fakeLauncher{}, readOnlyTool("read"))
	var ids []string
	for range 4 {
		// A zero wait detaches, so the id exists even though the child is quick.
		ids = append(ids, runIDOf(t, subagentCall(t, catalog, `{"task":"x","wait_ms":0}`)))
		waitUntilListed(t, catalog, ids[len(ids)-1], "done")
	}
	for index, id := range ids {
		out := subagentCall(t, catalog, `{"id":"`+id+`"}`)
		dropped := strings.Contains(out, "error_code: unknown_run")
		if wantDropped := index < 2; dropped != wantDropped {
			t.Errorf("run %d dropped = %v, want %v:\n%s", index, dropped, wantDropped, out)
		}
	}
}

func waitUntilListed(t *testing.T, catalog *tools.Catalog, id, state string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if list := subagentCall(t, catalog, `{}`); strings.Contains(list, id+" explore "+state) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %s", id, state)
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
		ids = append(ids, runIDOf(t, subagentCall(t, catalog, `{"task":"x","wait_ms":0}`)))
	}
	for _, id := range ids {
		if out := subagentCall(t, catalog, `{"id":"`+id+`","wait_ms":0}`); strings.Contains(out, "unknown_run") {
			t.Fatalf("running run %s was dropped:\n%s", id, out)
		}
	}
}

func TestSubagentListShowsTheRunsWithTheirState(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), blockedLauncher(release, nil), readOnlyTool("read"))
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"find the retry delay in the provider","wait_ms":0}`))
	list := subagentCall(t, catalog, `{}`)
	for _, want := range []string{"Runs:", id + " explore running", "find the retry delay in the provider", "runs: 1"} {
		if !strings.Contains(list, want) {
			t.Errorf("list lacks %q:\n%s", want, list)
		}
	}
}

func TestSubagentTheRunListKeepsOnlyAShortTaskPreview(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), blockedLauncher(release, nil), readOnlyTool("read"))
	long := strings.Repeat("secret-detail ", 50)
	subagentCall(t, catalog, `{"task":"`+long+`","wait_ms":0}`)
	list := subagentCall(t, catalog, `{}`)
	if strings.Contains(list, long) || !strings.Contains(list, "...") {
		t.Fatalf("the run list holds the whole task:\n%s", list)
	}
}

func TestSubagentCutsALateAnswerToTheResultLimit(t *testing.T) {
	long := strings.Repeat("é", 2000)
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{Text: long, Rounds: 1}, nil
	}}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"))
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"x","wait_ms":0}`))
	waitUntilListed(t, catalog, id, "done")
	out := subagentCall(t, catalog, `{"id":"`+id+`"}`)
	if !strings.Contains(out, "truncated: true") {
		t.Fatalf("a cut answer is not marked:\n%s", out)
	}
	if strings.Contains(out, strings.Repeat("é", 600)) {
		t.Fatalf("the answer was not cut to 1024 bytes")
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
	settings := subagentSettings(agentNamed("explore"))
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := NewSubagentToolWithManager(settings, parent, blockedLauncher(make(chan struct{}), &cancelled), nil, manager)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"x","wait_ms":0}`))

	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if cancelled.Load() != 1 {
		t.Fatalf("Close left the child running: %d cancellations", cancelled.Load())
	}
	if out := subagentCall(t, catalog, `{"id":"`+id+`"}`); !strings.Contains(out, "was stopped") {
		t.Fatalf("run after Close:\n%s", out)
	}
	if out := subagentCall(t, catalog, `{"task":"y"}`); !strings.Contains(out, "error_code: subagent_unavailable") {
		t.Fatalf("a start after Close:\n%s", out)
	}
}

func TestSubagentParentStoppingBeforeTheHandoffStillCancelsTheChild(t *testing.T) {
	var cancelled atomic.Int64
	catalog, tool := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), blockedLauncher(make(chan struct{}), &cancelled), readOnlyTool("read"))
	_ = catalog
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tool.Execute(ctx, map[string]any{"task": "x"}, tools.ExecutionContext{CallID: "c"})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if cancelled.Load() != 1 {
		t.Fatalf("the child saw %d cancellations, want 1 (and it must be over when the call returns)", cancelled.Load())
	}
}
