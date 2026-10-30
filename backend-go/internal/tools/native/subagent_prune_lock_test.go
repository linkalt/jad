package native

import (
	"context"
	"testing"
	"time"
)

// Dropping a manifest waits on the store, which a save holds across its fsync. The manager
// lock must not be held during that wait, or every other caller of the manager waits too.
func TestSubagentPruneRemovesManifestsOutsideTheManagerLock(t *testing.T) {
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: t.TempDir(), Keep: 1})
	defer manager.Close()
	parked := make(chan struct{})
	proceed := make(chan struct{})
	manager.removeManifest = func(id string) {
		close(parked)
		<-proceed
		manager.store.remove(id)
	}
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{Text: "done", Rounds: 1}, nil
	}}
	start := func() *subagentRun {
		run, err := manager.start(subagentStart{
			launcher: launcher, owner: "o", request: SubagentRequest{Agent: agentNamed("explore"), Task: "t"},
			limit: 4, maxQueued: 4, keep: 1, resultMax: 1000,
		})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	<-start().done
	second := start()
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("the second finish did not prune the first run")
	}
	locked := make(chan bool, 1)
	go func() {
		manager.mu.Lock()
		manager.mu.Unlock()
		locked <- true
	}()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		close(proceed)
		t.Fatal("the manager lock is held while a manifest is removed")
	}
	close(proceed)
	<-second.done
}
