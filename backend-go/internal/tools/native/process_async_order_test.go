package native

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/tools"
)

// gatedAsync holds back the end signal of a process until release is called.
type gatedAsync struct {
	ProcessAsync
	gate chan struct{}
}

func (g gatedAsync) End(snapshot ProcessSnapshot) {
	<-g.gate
	g.ProcessAsync.End(snapshot)
}

// gatedProcessManager starts processes whose manifest is already finished while their end
// signal is still held back, and releases it when the tool waits for the process.
type gatedProcessManager struct {
	*ManagedProcessManager
	gate chan struct{}
	once sync.Once
}

func (g *gatedProcessManager) release() { g.once.Do(func() { close(g.gate) }) }

func (g *gatedProcessManager) Start(request ProcessStartRequest) (ProcessSnapshot, error) {
	request.Async = gatedAsync{ProcessAsync: request.Async, gate: g.gate}
	snapshot, err := g.ManagedProcessManager.Start(request)
	if err != nil {
		return snapshot, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if current, ok := g.ManagedProcessManager.SnapshotWithLogs(snapshot.ID, ProcessStreamLimits{}, ProcessStreamLimits{}, 0, false); ok && processTerminal(current.State) {
			return *current, nil
		}
		time.Sleep(time.Millisecond)
	}
	return snapshot, nil
}

func (g *gatedProcessManager) WaitContext(ctx context.Context, id string, timeoutMs int64) ProcessWaitResult {
	g.release()
	return g.ManagedProcessManager.WaitContext(ctx, id, timeoutMs)
}

func TestFinishedProcessResultIsConsumedEvenWhenItsSignalIsQueuedAfterTheManifest(t *testing.T) {
	f := newProcessAsyncFixture(t)
	gated := &gatedProcessManager{ManagedProcessManager: f.manager, gate: make(chan struct{})}
	t.Cleanup(gated.release)
	settings := processTestRequest(f.dir, "", 1<<20).Policy
	settings.StorageDir = filepath.Join(f.dir, "processes")
	settings.Shell = "/bin/sh"
	process, err := NewProcessToolWithAsync(settings, gated, "KEY", f.host)
	if err != nil {
		t.Fatal(err)
	}
	out, err := process.Execute(context.Background(), map[string]any{"command": "echo quick", "block_ms": json.Number("5000")}, tools.ExecutionContext{CallID: "w", WorkspaceRoot: f.dir, SessionKey: "s1"})
	if err != nil || !strings.Contains(out, "exit_code: 0") {
		t.Fatalf("start = %s, err = %v", out, err)
	}
	gated.release()
	f.manager.Wait(processIDOf(t, out), 5000)
	if later := f.wait(t, "s1", 0); strings.Contains(later, processIDOf(t, out)) {
		t.Fatalf("the signal was delivered twice:\n%s", later)
	}
}
