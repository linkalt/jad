package native

import (
	"context"
	"testing"
)

// A new run never takes the id of a run the manager still holds: the second start gets a
// fresh id, and the first run keeps its own record.
func TestSubagentRunIDIsNotReusedWhileTheRunIsHeld(t *testing.T) {
	manager := NewSubagentManager()
	defer manager.Close()
	release := make(chan struct{})
	defer close(release)
	launcher := &fakeLauncher{run: func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return SubagentResult{Text: "done", Rounds: 1}, nil
	}}
	ids := []string{"sub_00000001", "sub_00000001", "sub_00000002"}
	manager.newRunID = func() (string, error) {
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}
	start := func() *subagentRun {
		run, err := manager.start(subagentStart{
			launcher: launcher, owner: "o", request: SubagentRequest{Agent: agentNamed("explore"), Task: "t"},
			admission: runAdmission{limit: 4, maxQueued: 4}, keep: 4, resultMax: 1000,
		})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	first := start()
	second := start()
	if first.id == second.id {
		t.Fatalf("two runs share the id %s", first.id)
	}
	if manager.byID(first.id) != first {
		t.Fatalf("the first run %s was replaced in the manager", first.id)
	}
}
