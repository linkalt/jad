package native

import (
	"cmp"
	"errors"
	"slices"
	"strconv"
	"sync"
)

// RunTree is the registry of every run a caller started that is going now, of either kind and in
// every manager of the process (design section 3.7, T-007): a local sub-agent run, the place a
// nested remote sub-agent run holds, and a profile_message run, queued or running. Each node knows
// its parent and its root, so max_runs_per_root and max_runs_per_session count the whole tree
// across a profile hop and across the managers of different storage folders. A user's own run is
// not a node: a run it starts is a root.
//
// Node ids are minted here and are unique in the process; they are not the external run ids
// (sub_…, a profile run's turn), which only one manager or one transcript make unique.
//
// A node lives from its admission (Reserve) until Release. A run whose work can outlive it (a
// profile_message target's handles, owned by its session) may End first: it then gives back its
// places but stays a parent, so a start that work makes later joins the run's tree under its root
// and ceiling instead of failing closed; the owner Releases it once that work is gone.
//
// Lock order: RunTree.mu is a leaf. It is taken under SubagentManager.mu (admission) and under
// the app's profile queue lock, and nothing is called while it is held.
//
// The zero value is ready to use.
type RunTree struct {
	mu  sync.Mutex
	seq uint64
	// nodes holds every node in the tree by id: the live ones and the ended ones that are still
	// parents (End before Release).
	nodes map[string]*RunNode
	// below counts the live nodes of each root, the root itself not counted: the runs below one
	// top-level run, which max_runs_per_root bounds.
	below map[string]int
	// bySession counts the live nodes of each session key, the session that executes the run.
	bySession map[string]int
	// writers counts the background writer places of each owner (session key), whichever manager
	// the writer runs in: max_background_writers is the session's cap, and one owner's writers can
	// be in two managers (its storage_dir changed while one ran). See ReserveWriter.
	writers map[string]int
}

// RunNodeKind says what a node is.
type RunNodeKind string

const (
	// RunNodeSubagent is a local sub-agent run.
	RunNodeSubagent RunNodeKind = "subagent"
	// RunNodeRemoteSubagent is the place a nested remote sub-agent run holds on this host. The run
	// itself is on the remote; the node is counted in the calling session.
	RunNodeRemoteSubagent RunNodeKind = "remote_subagent"
	// RunNodeProfileMessage is a run profile_message started, from the moment it is admitted
	// (queued for its target or running) until the target's transcript is released.
	RunNodeProfileMessage RunNodeKind = "profile_message"
)

// RunNodeState is where a node's run stands.
type RunNodeState string

const (
	RunNodeQueued  RunNodeState = "queued"
	RunNodeRunning RunNodeState = "running"
	// RunNodeEnded is a run that ended (End) while work it left may still start runs: it is
	// counted nowhere, and stays a parent until Release.
	RunNodeEnded RunNodeState = "ended"
)

var (
	// ErrRunTreeLimit refuses a nested start when its tree or the session that would execute it
	// already has the most runs it may have going.
	ErrRunTreeLimit = errors.New("this sub-agent tree or session already has the most runs it may have going")
	// ErrRunTreeParentGone refuses a start whose calling run is not, or no longer, in the tree. A
	// call from inside a run always carries that run's node while the run goes, so this fails
	// closed on a call that carries none (or a stale one) instead of starting an uncounted tree.
	ErrRunTreeParentGone = errors.New("the run this call belongs to is not going any more, so it cannot start another")
)

// RunReservation is one admission into the tree.
type RunReservation struct {
	// Parent is the node of the calling run; empty for a call of a user's own run, whose start is
	// a root and is not checked against the caps (it is counted).
	Parent string
	// Session is the session key that executes the new run: the caller's for a sub-agent (local
	// or remote), the target's for a profile_message run.
	Session string
	Kind    RunNodeKind
	State   RunNodeState
	// MaxPerRoot is the caller's max_runs_per_root and MaxPerSession the max_runs_per_session of
	// Session, the executing session's own (for a sub-agent the caller's, for a profile_message
	// run the target's); zero is no limit. A root keeps its MaxPerRoot as the tree's ceiling: a
	// nested start is bounded by the lowest ceiling on its path (each caller can narrow it, none
	// can widen it). MaxPerSession is checked on the count of Session.
	MaxPerRoot    int
	MaxPerSession int
}

// RunNode is one live run of the tree. Release takes it out once.
type RunNode struct {
	tree                   *RunTree
	seq                    uint64
	id, parent, root, sess string
	kind                   RunNodeKind
	// state, ended and released are guarded by tree.mu. ended: the places are given back (End);
	// released: the node is out of the tree too (Release).
	state           RunNodeState
	ended, released bool
	// rootCap is the max_runs_per_root ceiling the node's own nested starts are bounded by.
	rootCap int
}

// RunNodeInfo is a copy of a node, for checks and listings.
type RunNodeInfo struct {
	ID, Parent, Root, Session string
	Kind                      RunNodeKind
	State                     RunNodeState
	RootCap                   int
}

// narrowCap is the lower of two caps where zero is none.
func narrowCap(ceiling, limit int) int {
	switch {
	case ceiling <= 0:
		return max(limit, 0)
	case limit <= 0:
		return ceiling
	}
	return min(ceiling, limit)
}

// Reserve admits a run into the tree and returns its node. A nested start (Parent set) is refused
// with ErrRunTreeLimit when its root already has its ceiling of runs below it, or Session has
// MaxPerSession runs going, and with ErrRunTreeParentGone when Parent is not in the tree (never
// admitted, or released; a parent that only ended is one). The check and
// the insertion are one step, so two starts in different managers never both take the last place.
func (t *RunTree) Reserve(r RunReservation) (*RunNode, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.nodes == nil {
		t.nodes, t.below, t.bySession = map[string]*RunNode{}, map[string]int{}, map[string]int{}
	}
	t.seq++
	node := &RunNode{tree: t, seq: t.seq, id: "run_" + strconv.FormatUint(t.seq, 10), parent: r.Parent, sess: r.Session, kind: r.Kind, state: r.State}
	if r.Parent == "" {
		node.root, node.rootCap = node.id, max(r.MaxPerRoot, 0)
	} else {
		parent := t.nodes[r.Parent]
		if parent == nil {
			return nil, ErrRunTreeParentGone
		}
		node.root, node.rootCap = parent.root, narrowCap(parent.rootCap, r.MaxPerRoot)
		if (node.rootCap > 0 && t.below[node.root] >= node.rootCap) || (r.MaxPerSession > 0 && t.bySession[r.Session] >= r.MaxPerSession) {
			return nil, ErrRunTreeLimit
		}
		t.below[node.root]++
	}
	t.bySession[r.Session]++
	t.nodes[node.id] = node
	return node, nil
}

// ID is the node's id, which the run's calls carry as ExecutionContext.RunNode. Empty for nil.
func (n *RunNode) ID() string {
	if n == nil {
		return ""
	}
	return n.id
}

// SetState records where the node's run stands; an ended node stays ended.
func (n *RunNode) SetState(state RunNodeState) {
	if n == nil {
		return
	}
	n.tree.mu.Lock()
	defer n.tree.mu.Unlock()
	if !n.ended {
		n.state = state
	}
}

// End gives back the node's places, its run having ended, and keeps it in the tree as the parent of
// a start that work the run left going makes later (Reserve). It is safe to call more than once,
// and on nil; Release still takes the node out.
func (n *RunNode) End() {
	if n == nil {
		return
	}
	n.tree.mu.Lock()
	defer n.tree.mu.Unlock()
	n.endLocked()
}

func (n *RunNode) endLocked() {
	if n.ended {
		return
	}
	n.ended, n.state = true, RunNodeEnded
	t := n.tree
	if n.id != n.root {
		countDown(t.below, n.root)
	}
	countDown(t.bySession, n.sess)
}

// Release takes the node out of the tree and gives back its places, if End has not. It is safe to
// call more than once and on nil: only the first call counts. A node's children stay; they keep
// their root.
func (n *RunNode) Release() {
	if n == nil {
		return
	}
	t := n.tree
	t.mu.Lock()
	defer t.mu.Unlock()
	n.endLocked()
	if !n.released {
		n.released = true
		delete(t.nodes, n.id)
	}
}

// Nodes lists the nodes in the tree, ordered by id number: the live ones and those that ended but
// are still parents (RunNodeEnded).
func (t *RunTree) Nodes() []RunNodeInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	nodes := make([]*RunNode, 0, len(t.nodes))
	for _, n := range t.nodes {
		nodes = append(nodes, n)
	}
	slices.SortFunc(nodes, func(a, b *RunNode) int { return cmp.Compare(a.seq, b.seq) })
	infos := make([]RunNodeInfo, len(nodes))
	for i, n := range nodes {
		infos[i] = RunNodeInfo{ID: n.id, Parent: n.parent, Root: n.root, Session: n.sess, Kind: n.kind, State: n.state, RootCap: n.rootCap}
	}
	return infos
}

// SessionRuns is how many live nodes session executes.
func (t *RunTree) SessionRuns(session string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bySession[session]
}

// ReserveWriter takes one of owner's background writer places and reports whether there was one:
// a limit of zero or less means no limit, and force takes the place whatever the count (a closed
// manager does, so its start reports the closure and gives the place back). The check and the
// count are one step, so two starts in different managers never both take the last place.
func (t *RunTree) ReserveWriter(owner string, limit int, force bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !force && limit > 0 && t.writers[owner] >= limit {
		return false
	}
	if t.writers == nil {
		t.writers = map[string]int{}
	}
	t.writers[owner]++
	return true
}

// ReleaseWriter gives back one of owner's background writer places.
func (t *RunTree) ReleaseWriter(owner string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	countDown(t.writers, owner)
}

// Writers is how many background writer places owner holds, in every manager sharing the tree.
func (t *RunTree) Writers(owner string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writers[owner]
}
