package native

import (
	"sync"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
)

// T-007 Q3: an owner's background writer places are counted in the run tree, so every manager
// sharing it sees the owner's one count (max_background_writers is the session's cap).
func TestBackgroundWriterPlacesAreTheOwnersInTheSharedTree(t *testing.T) {
	tree := &RunTree{}
	a, b := NewSubagentManager(), NewSubagentManager()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	a.SetRunTree(tree)
	b.SetRunTree(tree)
	if !a.reserveWriter("owner", 1) {
		t.Fatal("the owner's first writer place was refused")
	}
	if b.reserveWriter("owner", 1) {
		t.Fatal("another manager gave the owner a second place past its cap of 1")
	}
	if !b.reserveWriter("other", 1) {
		t.Fatal("another owner's place was refused")
	}
	if a.BackgroundWriters("owner") != 1 || b.BackgroundWriters("owner") != 1 {
		t.Fatalf("both managers must see the owner's one writer: %d, %d", a.BackgroundWriters("owner"), b.BackgroundWriters("owner"))
	}
	a.releaseWriter("owner")
	if !b.reserveWriter("owner", 1) {
		t.Fatal("a place given back in one manager was not free in the other")
	}
	// A closed manager still reserves, so its start reports the closure (and gives it back).
	_ = a.Close()
	if !a.reserveWriter("owner", 1) {
		t.Fatal("a closed manager must reserve so its start reports the closure")
	}
	a.releaseWriter("owner")
	b.releaseWriter("owner")
	b.releaseWriter("other")
	if got := tree.Writers("owner") + tree.Writers("other"); got != 0 {
		t.Fatalf("%d writer places left", got)
	}
}

// Concurrent reservations of one owner in two managers never take more than the cap.
func TestBackgroundWriterPlacesAcrossManagersAreAtomic(t *testing.T) {
	tree := &RunTree{}
	managers := []*SubagentManager{NewSubagentManager(), NewSubagentManager()}
	for _, m := range managers {
		m.SetRunTree(tree)
		t.Cleanup(func() { _ = m.Close() })
	}
	var taken atomic.Int32
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if managers[i%2].reserveWriter("owner", 3) {
				taken.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := taken.Load(); got != 3 || tree.Writers("owner") != 3 {
		t.Fatalf("%d places taken (tree counts %d), want the cap of 3", got, tree.Writers("owner"))
	}
}

// A top-level remote start holds no node on the calling host: the remote run is a root of the
// remote host's tree, counted there. A nested remote start holds a remote_subagent node below the
// caller's node, in the calling session, until the remote run ends.
func TestRemoteStartsTopLevelHoldNoNodeNestedHoldOneBelowTheCaller(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) { s.MaxDepth = 2 })
	tree := f.manager.runTree()
	during := make(chan []RunNodeInfo, 1)
	go func() {
		run := f.peer.backend.run(t, 0)
		during <- tree.Nodes()
		run.finish(remoteapi.RunResult{State: "done", Text: "ok"})
	}()
	f.start(t, remoteAgent, 5000, remoteCaller)
	if nodes := <-during; len(nodes) != 0 {
		t.Fatalf("a top-level remote run holds nodes on the calling host: %+v", nodes)
	}

	nested := remoteCaller
	root := seedRunRoot(t, f.manager)
	nested.AgentDepth, nested.RootRun, nested.RunNode = 1, "sub_00000001", root
	go func() {
		run := f.peer.backend.run(t, 1)
		during <- tree.Nodes()
		run.finish(remoteapi.RunResult{State: "done", Text: "ok"})
	}()
	f.start(t, remoteAgent, 5000, nested)
	nodes := <-during
	if len(nodes) != 2 || nodes[1].Kind != RunNodeRemoteSubagent || nodes[1].Parent != root || nodes[1].Root != root || nodes[1].Session != remoteCaller.SessionKey {
		t.Fatalf("a nested remote run must hold one remote_subagent node below the caller: %+v", nodes)
	}
	if nodes := tree.Nodes(); len(nodes) != 1 || nodes[0].ID != root {
		t.Fatalf("the nested remote run's node outlived its run: %+v", nodes)
	}
}
