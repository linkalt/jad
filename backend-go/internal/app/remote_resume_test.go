package app

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/remoteapi"
)

// A remote resume of a run another token started is refused as unknown_run, the same code a local
// call gets for another owner's run. The source is the run the first token started through the real
// remote API, so the serving side reaches it in its own manager.
func TestRemoteResumeOfAnotherTokensRunIsUnknownLikeALocalOtherOwner(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	server := startRemoteServer(t, f, tokenTable(
		tokenRow("peer", remoteTestSecret, fullGrant),
		tokenRow("other", remoteTestSecret+"-other", fullGrant)))

	status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "first", "request_id": "req-resume-app-1"})
	if status != 202 {
		t.Fatalf("start: %d %v", status, body)
	}
	server.waitDone(t, body["id"].(string))
	remoteRun := body["id"].(string)

	status, body = server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret+"-other", map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "more", "resume": remoteRun, "request_id": "req-resume-app-2"})
	if status != 409 {
		t.Fatalf("another token's resume: status %d %v, want 409", status, body)
	}
	if code, _ := body["code"].(string); code != "unknown_run" {
		t.Fatalf("another token's resume: code %q, want unknown_run (the local other-owner code) %v", code, body)
	}
}

// A remote resume leaves its source run as it was: the source keeps its state and its answer, and
// the new run is a copy with its own id.
func TestRemoteResumeLeavesTheSourceRunUnchanged(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))

	status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "first", "request_id": "req-resume-app-3"})
	if status != 202 {
		t.Fatalf("start: %d %v", status, body)
	}
	source := body["id"].(string)
	before := server.waitDone(t, source)

	status, body = server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "more", "resume": source, "request_id": "req-resume-app-4"})
	if status != 202 {
		t.Fatalf("resume: %d %v", status, body)
	}
	resumed := body["id"].(string)
	if resumed == source {
		t.Fatalf("the resumed run has the source's id %s", source)
	}
	server.waitDone(t, resumed)

	after := server.waitDone(t, source)
	if after["state"] != before["state"] || after["text"] != before["text"] {
		t.Fatalf("the source run changed: before %v, after %v", before, after)
	}
}

// A remote resume finds the run by the id the remote API minted for it. The id is kept with the
// resolver's remote backend, so the same run is found on every later request of that server.
func TestRemoteStartedRunResumesByItsRemoteID(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))

	status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "first", "request_id": "req-map-1"})
	if status != 202 {
		t.Fatalf("start: %d %v", status, body)
	}
	source := body["id"].(string)
	server.waitDone(t, source)

	status, body = server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "more", "resume": source, "request_id": "req-map-2"})
	if status != 202 {
		t.Fatalf("resume by %s: %d %v", source, status, body)
	}
	resumed := server.waitDone(t, body["id"].(string))
	if resumed["state"] != "done" || resumed["resumed_from"] != source {
		t.Fatalf("the resumed run is not done and named after its remote source %s: %v", source, resumed)
	}
}

// A restarted server does not know the remote ids of the runs before it: the old rr_ id is refused as
// unknown_run, the same code another token's run gets. Its run is still in the manifest, so the local
// run exists; only the remote name for it is gone.
func TestRemoteResumeOfAnOldRemoteIDIsUnknownAfterARestart(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))

	status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "first", "request_id": "req-map-3"})
	if status != 202 {
		t.Fatalf("start: %d %v", status, body)
	}
	source := body["id"].(string)
	server.waitDone(t, source)

	restarted := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))
	status, body = restarted.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "more", "resume": source, "request_id": "req-map-4"})
	if status != 409 {
		t.Fatalf("resume after a restart: status %d %v, want 409", status, body)
	}
	if code, _ := body["code"].(string); code != "unknown_run" {
		t.Fatalf("resume after a restart: code %q, want unknown_run %v", code, body)
	}
}

// The index finds a remote run only in the session and profile it started in: the same id under
// another session key is not found, so a resume through another profile is unknown.
func TestRemoteRunIndexFindsARunOnlyInItsSession(t *testing.T) {
	index := newRemoteRunIndex()
	index.record("rr_0123456789abcdef", remoteRunRef{local: "sub_00000001", session: "ref\x00dev/coder", identity: new(int)})
	if ref, ok := index.lookup("rr_0123456789abcdef", "ref\x00dev/coder"); !ok || ref.local != "sub_00000001" {
		t.Fatalf("the run is not found in its own session: %v %v", ref, ok)
	}
	if _, ok := index.lookup("rr_0123456789abcdef", "ref\x00dev/other"); ok {
		t.Fatal("the run is found through another profile")
	}
	if _, ok := index.lookup("rr_fedcba9876543210", "ref\x00dev/coder"); ok {
		t.Fatal("an unknown remote id is found")
	}
}

// The index forgets a local run when its manager drops it, from every map.
func TestRemoteRunIndexForgetsADroppedRun(t *testing.T) {
	index := newRemoteRunIndex()
	identity := new(int)
	index.record("rr_0123456789abcdef", remoteRunRef{local: "sub_00000001", session: "ref\x00dev/coder", identity: identity})
	index.forget(identity)
	if _, ok := index.lookup("rr_0123456789abcdef", "ref\x00dev/coder"); ok {
		t.Fatal("a dropped run is still found by its remote id")
	}
	if got := index.remoteIDOf("sub_00000001"); got != "sub_00000001" {
		t.Fatalf("a dropped run still has a remote id: %q", got)
	}
	if len(index.runs) != 0 || len(index.byLocal) != 0 || len(index.byID) != 0 {
		t.Fatalf("the index keeps entries for a dropped run: %d, %d, %d", len(index.runs), len(index.byLocal), len(index.byID))
	}
}

// A local id can be drawn again once its run is pruned. A forget for the pruned run, which arrives
// after the new run has the same id, must leave the new run's entry: the identity names the run.
func TestForgetOfAPrunedRunKeepsANewRunWithTheSameLocalID(t *testing.T) {
	index := newRemoteRunIndex()
	pruned, reused := new(int), new(int)
	index.record("rr_0000000000000001", remoteRunRef{local: "sub_00000001", session: "ref\x00dev/coder", identity: pruned})
	index.record("rr_0000000000000002", remoteRunRef{local: "sub_00000001", session: "ref\x00dev/other", identity: reused})
	index.forget(pruned)
	if _, ok := index.lookup("rr_0000000000000002", "ref\x00dev/other"); !ok {
		t.Fatal("a forget for the pruned run removed the new run with the same local id")
	}
	if got := index.remoteIDOf("sub_00000001"); got != "rr_0000000000000002" {
		t.Fatalf("the new run lost its remote id: %q", got)
	}
}

// A run pruned between its record and the retention check leaves no entry in the index. The seam
// pauses the first remote start there; a second remote run, which keep_finished allows only with the
// first gone, prunes the first from the test's own goroutine. With the check before the record, the
// paused start would record the pruned run.
func TestRunPrunedInItsWindowLeavesNoEntry(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "__subagent_settings": map[string]any{"keep_finished": 1}})
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))

	entered := make(chan struct{})
	release := make(chan struct{})
	var paused atomic.Bool
	releasePause := sync.OnceFunc(func() { close(release) })
	afterRemoteObserved = func() {
		if paused.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	t.Cleanup(func() { afterRemoteObserved = nil; releasePause() })

	type reply struct {
		status int
		body   map[string]any
	}
	first := make(chan reply, 1)
	go func() {
		status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
			"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "first", "request_id": "req-window-1"})
		first <- reply{status, body}
	}()
	receive(t, "the first start to reach its window", entered)
	local := waitForManifestState(t, f, "done")

	status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "second", "request_id": "req-window-2"})
	if status != 202 {
		t.Fatalf("second start: %d %v", status, body)
	}
	server.waitDone(t, body["id"].(string))
	if _, err := os.Stat(filepath.Join(f.toolDir, ".subagents", ".runs", local+".json")); !os.IsNotExist(err) {
		t.Fatalf("the first run was not pruned in its window: %v", err)
	}

	releasePause()
	got := receive(t, "the first start to finish", first)
	if got.status != 202 {
		t.Fatalf("first start: %d %v", got.status, got.body)
	}
	remoteRun := got.body["id"].(string)

	index := server.service.resolver.remoteRuns
	index.mu.Lock()
	_, stale := index.runs[remoteRun]
	entries, locals := len(index.runs), len(index.byLocal)
	index.mu.Unlock()
	if stale || entries != 1 || locals != 1 {
		t.Fatalf("the index after the pruned start: holds %v for %s, %d remote entries and %d local, want only the second run's", stale, remoteRun, entries, locals)
	}

	status, body = server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "more", "resume": remoteRun, "request_id": "req-window-3"})
	if status != 409 {
		t.Fatalf("resume of the pruned remote id: status %d %v, want 409", status, body)
	}
	if code, _ := body["code"].(string); code != "unknown_run" {
		t.Fatalf("resume of the pruned remote id: code %q, want unknown_run %v", code, body)
	}
}
