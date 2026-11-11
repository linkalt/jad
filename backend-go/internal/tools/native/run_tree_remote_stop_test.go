package native

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
)

// remoteNodeHeld reports whether the tree still holds the nested remote run's node below root,
// counted in the calling session.
func remoteNodeHeld(tree *RunTree, root string) bool {
	for _, node := range tree.Nodes() {
		if node.Kind == RunNodeRemoteSubagent && node.Root == root {
			return tree.SessionRuns(remoteCaller.SessionKey) == 1
		}
	}
	return false
}

func onlyRoot(tree *RunTree, root string) bool {
	nodes := tree.Nodes()
	return len(nodes) == 1 && nodes[0].ID == root && tree.SessionRuns(remoteCaller.SessionKey) == 0
}

// A nested remote run that is an async handle keeps its remote_subagent node through a session
// stop, reset or delete while the remote run is still going (the call stays active: the stop is
// only a request to the remote), and gives the node and its session place back once the remote
// run ends. A remote that acts on the stop at once frees them without the test ending it.
func TestNestedRemoteHandleNodeIsReleasedAfterSessionStopResetDelete(t *testing.T) {
	const lineage = "lin_caller"
	stops := map[string]func(f *remoteSubagentFixture) int{
		"stop": func(f *remoteSubagentFixture) int {
			return f.host.Manager().CancelOwner(remoteCaller.SessionKey, "user_stop")
		},
		"reset": func(f *remoteSubagentFixture) int {
			return f.host.Manager().ResetOwner(remoteCaller.SessionKey, "session_reset")
		},
		"delete": func(f *remoteSubagentFixture) int {
			return f.host.Manager().DeleteLineage(func(k string) bool { return k == lineage }, "s1", "session_deleted")
		},
		"run end sweep": func(f *remoteSubagentFixture) int {
			return f.host.Manager().DeleteOwnersWithPrefix(remoteCaller.SessionKey, "parent_ended")
		},
	}
	for name, stop := range stops {
		for _, stubborn := range []bool{true, false} {
			t.Run(name+map[bool]string{true: " remote still going", false: " remote stops at once"}[stubborn], func(t *testing.T) {
				f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) { s.MaxDepth = 2 })
				tree := f.manager.runTree()
				root := seedRunRoot(t, f.manager)
				nested := remoteCaller
				nested.CallID, nested.AgentDepth, nested.RootRun, nested.RunNode = "call", 1, "sub_00000001", root
				ctx := WithRunLineage(context.Background(), []string{lineage})
				out, err := f.tool.Execute(ctx, map[string]any{"task": "find the answer", "agent": remoteAgent, "block_ms": json.Number("0")}, nested)
				if err != nil {
					t.Fatal(err)
				}
				if remoteRunHandle.FindString(out) == "" {
					t.Fatalf("the nested remote call did not become a handle:\n%s", out)
				}
				run := f.peer.backend.run(t, 0)
				if stubborn {
					run.ignoreStops()
				}
				if !remoteNodeHeld(tree, root) {
					t.Fatalf("the running nested remote run holds no node: %+v", tree.Nodes())
				}
				if n := stop(f); n != 1 {
					t.Fatalf("%s reached %d handles, want the remote run's", name, n)
				}
				eventually(t, "the remote run to be asked to stop", func() bool { return len(run.stops()) > 0 })
				if stubborn {
					if !remoteNodeHeld(tree, root) {
						t.Fatalf("the node of a remote run still going was released by the %s: %+v", name, tree.Nodes())
					}
					run.finish(remoteapi.RunResult{State: "stopped", Reason: "remote_stop", Text: "partial"})
				}
				eventually(t, "the remote run's node to be released", func() bool { return onlyRoot(tree, root) })
				waitForRemoteEnds(t, f.manager)
				if !onlyRoot(tree, root) {
					t.Fatalf("nodes after the remote run's end: %+v", tree.Nodes())
				}
			})
		}
	}
}

// A nested remote run whose call blocks (not a handle) is stopped by the cancel of its call, the
// way a session stop reaches a sub-agent run's calls. The call returns at once; the node stays
// counted while the remote run goes and is released by the run's end.
func TestNestedBlockingRemoteNodeIsReleasedAfterItsCallIsCancelled(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) { s.MaxDepth = 2 })
	tree := f.manager.runTree()
	root := seedRunRoot(t, f.manager)
	nested := remoteCaller
	nested.CallID, nested.AgentDepth, nested.RootRun, nested.RunNode = "call", 1, "sub_00000001", root
	nested.ProcessOwner, nested.ProcessAsync = remoteCaller.SessionKey+"/sub_00000001", false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		out, err := f.tool.Execute(ctx, map[string]any{"task": "find the answer", "agent": remoteAgent, "block_ms": json.Number("5000")}, nested)
		if err == nil {
			t.Errorf("a cancelled blocking call answered:\n%s", out)
		}
		returned <- err
	}()
	run := f.peer.backend.run(t, 0)
	run.ignoreStops()
	if !remoteNodeHeld(tree, root) {
		t.Fatalf("the running nested remote run holds no node: %+v", tree.Nodes())
	}
	cancel()
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled call returned %v", err)
	}
	eventually(t, "the remote run to be asked to stop", func() bool { return len(run.stops()) > 0 })
	if !remoteNodeHeld(tree, root) {
		t.Fatalf("the node of a remote run still going was released by the cancel: %+v", tree.Nodes())
	}
	run.finish(remoteapi.RunResult{State: "stopped", Reason: "caller_cancelled", Text: "partial"})
	eventually(t, "the remote run's node to be released", func() bool { return onlyRoot(tree, root) })
	waitForRemoteEnds(t, f.manager)
}
