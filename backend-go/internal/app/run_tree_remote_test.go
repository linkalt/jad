package app

import (
	"testing"
	"time"

	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/tools/native"
)

// T-007 Q3: a sub-agent run started over the remote API is a top-level run on this host: a root of
// its own in the run tree, in the session it runs in, counted there and never refused by a
// caller's caps (the caller's tree is on its own host and does not cross the remote boundary). A
// remote stop ends it and its node goes.
func TestRemoteStartedSubagentIsARootOfThisHostsTree(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	f.provider.childBlocks = true
	f.provider.childEntered = make(chan struct{}, 4)
	f.provider.childRelease = make(chan struct{})
	t.Cleanup(func() { close(f.provider.childRelease) })
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))
	r := server.service.resolver
	status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "find the answer", "request_id": "req-tre-00001"})
	if status != 202 {
		t.Fatalf("start: %d %v", status, body)
	}
	select {
	case <-f.provider.childEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the child never reached the provider")
	}
	reference, err := r.CanonicalSessionReference("local:demo")
	if err != nil {
		t.Fatal(err)
	}
	nodes := r.runTree.Nodes()
	if len(nodes) != 1 || nodes[0].Parent != "" || nodes[0].Root != nodes[0].ID || nodes[0].Kind != native.RunNodeSubagent ||
		nodes[0].Session != sessionKey(reference, "dev/coder") || nodes[0].State != native.RunNodeRunning {
		t.Fatalf("the remote-started run must be one root node in dev/coder's session: %+v", nodes)
	}
	id := body["id"].(string)
	if status, body = server.call(t, "POST", remoteapi.Prefix+"/runs/"+id+"/stop", remoteTestSecret, nil); status != 200 {
		t.Fatalf("stop: %d %v", status, body)
	}
	if done := server.waitDone(t, id); done["state"] != "stopped" {
		t.Fatalf("after stop: %v", done)
	}
	awaitEmptyRunTree(t, r)
}
