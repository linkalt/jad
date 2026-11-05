package native

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

// A read-only sub-agent started with a block shorter than its runtime returns a handle;
// the handle outlives the start call and the run's context; async wait delivers the
// completion; async stop ends a second one; nothing is left running afterwards.
func TestSubagentEndToEndHandleWaitStopAndNoLeak(t *testing.T) {
	before := baselineCount()
	{
		host, err := NewAsyncHost(filepath.Join(t.TempDir(), ".async"), asyncmgr.DefaultConfig(), nil)
		if err != nil {
			t.Fatal(err)
		}
		manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: t.TempDir(), Keep: 10})
		release := make(chan struct{})
		launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
			if strings.Contains(request.Task, "stop me") {
				<-ctx.Done()
				return SubagentResult{Text: "partial"}, ctx.Err()
			}
			select {
			case <-release:
				return SubagentResult{Text: "the answer is 42", Rounds: 3, Tokens: 77}, nil
			case <-ctx.Done():
				return SubagentResult{}, ctx.Err()
			}
		}}
		parent := withExecutors(t, readOnlyTool("read"))
		tool, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), parent, nil, launcher, nil, manager, host)
		if err != nil {
			t.Fatal(err)
		}
		catalog := catalogWithAsync(t, host, parent, tool)

		// The start call runs under a context that ends with the run, as a model turn does.
		turn, endTurn := context.WithCancel(context.Background())
		started := time.Now()
		out, err := tool.Execute(turn, map[string]any{"task": "long search", "block_ms": int64(50)}, tools.ExecutionContext{CallID: "c1", SessionKey: "s1"})
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(started) > 3*time.Second {
			t.Fatalf("a 50 ms block took %v", time.Since(started))
		}
		id := runIDOf(t, out)
		if !strings.Contains(out, "still running") || !strings.Contains(out, "async tool") {
			t.Fatalf("no handle returned:\n%s", out)
		}
		endTurn()

		// The handle survives the end of the turn; the run is still going.
		if status := asyncCallAs(t, catalog, "s1", `{"action":"status","id":"`+id+`"}`); !strings.Contains(status, "running") {
			t.Fatalf("the handle did not survive the run end:\n%s", status)
		}
		close(release)
		waited := asyncCallAs(t, catalog, "s1", `{"action":"wait","wait_ms":5000}`)
		for _, want := range []string{id, "\"state\":\"done\"", "\"ok\":true", "the answer is 42", "\"kind\":\"subagent\""} {
			if !strings.Contains(waited, want) {
				t.Errorf("wait lacks %q:\n%s", want, waited)
			}
		}
		if again := asyncCallAs(t, catalog, "s1", `{"action":"wait","wait_ms":0}`); strings.Contains(again, id) {
			t.Errorf("the signal was delivered twice:\n%s", again)
		}

		// Stop a second run through the async tool.
		second := runIDOf(t, subagentCallAs(t, catalog, "s1", `{"task":"stop me","block_ms":0}`))
		stopped := asyncCallAs(t, catalog, "s1", `{"action":"stop","id":"`+second+`"}`)
		if !strings.Contains(stopped, "stopped") || !strings.Contains(stopped, second) {
			t.Errorf("stop result:\n%s", stopped)
		}
		if counts := host.Manager().Counts("s1"); counts.Active != 0 {
			t.Errorf("handles still live: %+v", counts)
		}
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		host.Close()
	}
	requireNoGrowth(t, before, "a sub-agent handle started, awaited and stopped")
}
