package native

import (
	"context"
	"testing"
)

// runOwnedOne runs one fast child for owner, with that owner's keep, and returns its folder.
func runOwnedOne(t *testing.T, root, name, owner string, manager *SubagentManager, keep int) string {
	t.Helper()
	dir := writeTranscript(t, root, name, "answer "+name)
	launcher := &fakeLauncher{run: func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript(dir)
		return SubagentResult{Text: "answer " + name, Rounds: 1, Tokens: 1}, nil
	}}
	run, err := manager.start(subagentStart{
		launcher: launcher, owner: owner, request: SubagentRequest{Agent: agentNamed("explore"), Task: "t " + name},
		admission: runAdmission{limit: 4, maxQueued: 4}, keep: keep, resultMax: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-run.done
	return dir
}

// Sessions in one workspace share a storage directory and so one manager. keep_finished is a
// session setting: a session with a small one used to trim the shared list and drop (with
// delete_transcripts, delete) the other session's runs (audit D2).
func TestKeepFinishedOfOneSessionNeverDropsAnotherSessionsRuns(t *testing.T) {
	root := t.TempDir()
	manager := deletingManager(t, root, 50, true)
	var big []string
	for _, name := range []string{"big-1", "big-2", "big-3"} {
		big = append(big, runOwnedOne(t, root, name, "big", manager, 3))
	}
	small1 := runOwnedOne(t, root, "small-1", "small", manager, 1)
	small2 := runOwnedOne(t, root, "small-2", "small", manager, 1)

	if got := len(manager.list("big")); got != 3 {
		t.Errorf("session big keeps %d runs, want its keep_finished 3", got)
	}
	for _, dir := range big {
		if !exists(dir) {
			t.Errorf("another session's keep_finished deleted %s", dir)
		}
	}
	if got := len(manager.list("small")); got != 1 {
		t.Errorf("session small keeps %d runs, want its keep_finished 1", got)
	}
	if exists(small1) || !exists(small2) {
		t.Errorf("session small: oldest folder kept = %v, newest kept = %v; want only the newest", exists(small1), exists(small2))
	}
}

// At startup only the building session's keep is known; it applies per session, so a
// restart does not cut every other session down to it in total.
func TestRecoverTrimsEachSessionSeparately(t *testing.T) {
	root := t.TempDir()
	first := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 50})
	for _, name := range []string{"a-1", "a-2", "a-3"} {
		runOwnedOne(t, root, name, "a", first, 50)
	}
	for _, name := range []string{"b-1", "b-2"} {
		runOwnedOne(t, root, name, "b", first, 50)
	}
	_ = first.Close()

	restarted := deletingManager(t, root, 2, false)
	if a, b := len(restarted.list("a")), len(restarted.list("b")); a != 2 || b != 2 {
		t.Fatalf("after a restart with keep 2: session a keeps %d, b keeps %d; want 2 each", a, b)
	}
}
