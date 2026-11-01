package app

import (
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
	index := &remoteRunIndex{runs: make(map[string]remoteRunRef)}
	index.record("rr_0123456789abcdef", remoteRunRef{local: "sub_00000001", session: "ref\x00dev/coder"})
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
