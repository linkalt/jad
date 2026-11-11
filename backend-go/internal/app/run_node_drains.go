package app

import (
	"sync"

	"jad/backend-go/internal/tools/native"
)

// runNodeDrains are the run tree nodes of message runs that ended while work they left was still
// live under their lineage key, by that key. A target run's handles are owned by the target's
// session, so they outlive its run, and a start one of them makes (a tool_smith script's
// profile_message) names the run's node as its parent: the node stays in the tree as an ended
// parent until that work is gone (releaseNodeAfter), as a narrowing's MCP manager does
// (releaseMCPScopeAfter).
type runNodeDrains struct {
	mu      sync.Mutex
	pending map[string]*native.RunNode
}

// releaseNodeAfter ends node now, its run having ended and released its transcript, and releases
// it once nothing is live under the run's lineage key: at once when nothing is, else when the last
// handle or sub-agent run under it ends (lineageWorkEnded). An empty key releases at once. The node
// is recorded before the look, so an end between the two finds it.
func (r *Resolver) releaseNodeAfter(node *native.RunNode, key string) {
	if node == nil {
		return
	}
	node.End()
	if key == "" {
		node.Release()
		return
	}
	r.nodeDrains.mu.Lock()
	if r.nodeDrains.pending == nil {
		r.nodeDrains.pending = make(map[string]*native.RunNode)
	}
	r.nodeDrains.pending[key] = node
	r.nodeDrains.mu.Unlock()
	r.drainNode(key)
}

// checkNodeDrains releases the pending nodes whose lineage has nothing live any more; with nothing
// pending it costs one lock.
func (r *Resolver) checkNodeDrains() {
	r.nodeDrains.mu.Lock()
	if len(r.nodeDrains.pending) == 0 {
		r.nodeDrains.mu.Unlock()
		return
	}
	keys := make([]string, 0, len(r.nodeDrains.pending))
	for key := range r.nodeDrains.pending {
		keys = append(keys, key)
	}
	r.nodeDrains.mu.Unlock()
	for _, key := range keys {
		r.drainNode(key)
	}
}

// drainNode releases the node pending under key when nothing is live under it. Work under a key is
// started only by the run of the key or by live work under it, so once none is live no start can
// name the node any more, and the release cannot come too early.
func (r *Resolver) drainNode(key string) {
	if r.lineageLive(key) {
		return
	}
	r.nodeDrains.mu.Lock()
	node := r.nodeDrains.pending[key]
	delete(r.nodeDrains.pending, key)
	r.nodeDrains.mu.Unlock()
	node.Release()
}

// lineageWorkEnded is the end callback of every async and sub-agent manager of the resolver
// (SetLineageEnd, SetRunEnd), called outside their locks each time work that carries a lineage
// ended: what waits for a run's lineage to drain looks again.
func (r *Resolver) lineageWorkEnded() {
	r.checkMCPDrains()
	r.checkNodeDrains()
}
