package app

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/tools/native"
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

// A manager that closes takes its remote entries with it: its rr_ ids are unknown and the index holds
// nothing for it. A run of another manager stays resumable. The index is checked directly as well as
// through the API.
func TestClosingAManagerForgetsItsRemoteRuns(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))
	started := func(request string) string {
		status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
			"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "first", "request_id": request})
		if status != 202 {
			t.Fatalf("start %s: %d %v", request, status, body)
		}
		server.waitDone(t, body["id"].(string))
		return body["id"].(string)
	}
	closed := started("req-close-1")
	index := server.service.resolver.remoteRuns
	manager := index.managerOf(t, closed)
	if got := index.size(); got != 1 {
		t.Fatalf("the index holds %d entries before the close, want 1", got)
	}
	manager.Close()
	if got := index.size(); got != 0 {
		t.Fatalf("the index holds %d entries after the close, want 0", got)
	}
	status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "more", "resume": closed, "request_id": "req-close-2"})
	if status != 409 {
		t.Fatalf("resume of a closed manager's run: status %d %v, want 409", status, body)
	}
	if code, _ := body["code"].(string); code != "unknown_run" {
		t.Fatalf("resume of a closed manager's run: code %q, want unknown_run %v", code, body)
	}
}

// A start that races with its manager's close leaves no entry: the start is refused as closed, or it
// completes before the close and is forgotten with it. Either way the index is empty afterwards.
func TestAStartRacingACloseLeavesNoEntry(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))
	// The start runs in its own goroutine and is closed out from here. Its outcome is not checked: the
	// start may be refused as closed or complete first. Either way no entry may outlive the close.
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
			"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "race", "request_id": "req-race-1"})
	}()
	index := server.service.resolver.remoteRuns
	manager := onlySubagentManagerWhenBuilt(t, server.service.resolver)
	manager.Close()
	receive(t, "the racing start to return", done)
	if got := index.size(); got != 0 {
		t.Fatalf("the index holds %d entries after a racing start and close, want 0", got)
	}
}

// A manager's close forgets its own entries and no other manager's: the other manager's run stays
// in the index and resumable.
func TestCloseOfOneManagerKeepsAnotherManagersRuns(t *testing.T) {
	index := newRemoteRunIndex()
	closing, other := native.NewSubagentManager(), native.NewSubagentManager()
	defer func() { _ = other.Close() }()
	index.record("rr_0000000000000001", remoteRunRef{local: "sub_00000001", session: "ref\x00dev/coder", identity: new(int), manager: closing})
	index.record("rr_0000000000000002", remoteRunRef{local: "sub_00000002", session: "ref\x00dev/coder", identity: new(int), manager: other})
	index.forgetManager(closing)
	if got := index.size(); got != 1 {
		t.Fatalf("the index holds %d entries, want the other manager's one", got)
	}
	if _, ok := index.lookup("rr_0000000000000002", "ref\x00dev/coder"); !ok {
		t.Fatal("another manager's run is not resumable after the close")
	}
}

// managerOf is the manager that holds the run a remote id names. The test fails when the id is unknown.
func (i *remoteRunIndex) managerOf(t *testing.T, remoteID string) *native.SubagentManager {
	t.Helper()
	i.mu.Lock()
	defer i.mu.Unlock()
	ref, ok := i.runs[remoteID]
	if !ok {
		t.Fatalf("no run for remote id %s", remoteID)
	}
	return ref.manager
}

// onlySubagentManager is the one sub-agent manager the resolver holds; the test fails when there is not
// exactly one.
func onlySubagentManager(t *testing.T, r *Resolver) *native.SubagentManager {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.subagentManagers) != 1 {
		t.Fatalf("the resolver holds %d sub-agent managers, want 1", len(r.subagentManagers))
	}
	for _, manager := range r.subagentManagers {
		return manager
	}
	return nil
}

// onlySubagentManagerWhenBuilt waits until the resolver holds its sub-agent manager, then returns it.
// A start builds the manager, so a test that races a start with the close calls this first.
func onlySubagentManagerWhenBuilt(t *testing.T, r *Resolver) *native.SubagentManager {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		count := len(r.subagentManagers)
		r.mu.Unlock()
		if count == 1 {
			return onlySubagentManager(t, r)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the resolver never built its sub-agent manager")
	return nil
}

// A start whose manager closes after the run is observed and before it is recorded leaves no entry.
// The seam pauses the start in that window and the test closes the manager there. The close finds no
// entry to forget, so the record that follows must be refused by the retention check: a closed manager
// keeps nothing.
func TestRunRecordedAfterItsManagerClosesLeavesNoEntry(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
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
	started := make(chan reply, 1)
	go func() {
		status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
			"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "closing", "request_id": "req-closing-1"})
		started <- reply{status, body}
	}()
	receive(t, "the start to reach its window", entered)
	index := server.service.resolver.remoteRuns
	if got := index.size(); got != 0 {
		t.Fatalf("the index holds %d entries in the window, want none yet", got)
	}
	onlySubagentManager(t, server.service.resolver).Close()

	releasePause()
	got := receive(t, "the start to finish", started)
	if got.status != 202 {
		t.Fatalf("start: %d %v", got.status, got.body)
	}
	remoteRun := got.body["id"].(string)
	if size := index.size(); size != 0 {
		t.Fatalf("the index holds %d entries after the start in the closed manager, want 0", size)
	}
	status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "more", "resume": remoteRun, "request_id": "req-closing-2"})
	if status != 409 {
		t.Fatalf("resume of the run recorded across the close: status %d %v, want 409", status, body)
	}
	if code, _ := body["code"].(string); code != "unknown_run" {
		t.Fatalf("resume of the run recorded across the close: code %q, want unknown_run %v", code, body)
	}
}
