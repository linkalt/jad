package native

import "jad/backend-go/internal/tools"

// runAdmission is everything admission reads about one start (design section 3.5, phase R5a):
// where the start sits in the run tree, and the limits of the session that starts it. The
// subagent tool builds it once per call (admissionFor) for a local start and for a remote one,
// and the manager admits both with it (admitLocked), so the depth, the run caps, the slot and the
// queue are decided in one place and in one order.
type runAdmission struct {
	// session is the session the start executes in (the caller's). parent is the caller's node in
	// the run tree (ExecutionContext.RunNode), empty for a call of a user's own run: the start is
	// then a root. depth is the AgentDepth of the starting call (0 for a session's own run, a
	// profile_message target's included) and rootRun the top-level sub-agent run of this manager
	// it runs under, empty at depth 0; they name the run in its manifest. chainDepth is the call's
	// RunDepth, its depth across both kinds of run (R6): max_depth and fail fast go by it.
	session    string
	parent     string
	depth      int
	rootRun    string
	chainDepth int
	// maxDepth, maxPerRoot and maxPerSession bound nested starts; zero means 1 level deep and no
	// count limit.
	maxDepth      int
	maxPerRoot    int
	maxPerSession int
	// limit and maxQueued are the slots and the queue places of a local start. A remote start
	// takes neither.
	limit     int
	maxQueued int
}

// admissionFor is the admission of a start from a call with execution under the tool's settings.
func (s *subagentTool) admissionFor(execution tools.ExecutionContext) runAdmission {
	return runAdmission{
		session:       execution.SessionKey,
		parent:        execution.RunNode,
		depth:         execution.AgentDepth,
		rootRun:       execution.RootRun,
		chainDepth:    execution.RunDepth(),
		maxDepth:      int(s.settings.MaxDepth),
		maxPerRoot:    int(s.settings.MaxRunsPerRoot),
		maxPerSession: int(s.settings.MaxRunsPerSession),
		limit:         int(s.settings.MaxConcurrent),
		maxQueued:     int(s.settings.MaxQueued),
	}
}

// nested reports whether the start comes from inside a run of the tree (a sub-agent, or a
// profile_message target that a run started): the run caps count it, and its node hangs off its
// caller's. A call at AgentDepth above 0 is always one; it carries its node (start sets it).
func (a runAdmission) nested() bool { return a.parent != "" || a.depth > 0 }

// fromRun reports whether the start comes from inside any run below the user's, of either kind:
// max_depth applies, and it never waits for a slot. Every nested start is one.
func (a runAdmission) fromRun() bool { return a.runDepth() > 0 }

// runDepth is the depth max_depth is checked on (ExecutionContext.RunDepth).
func (a runAdmission) runDepth() int { return max(a.chainDepth, a.depth) }

// depthExceeds reports whether a call at depth may not start a run under maxDepth (at least 1).
// It is the one depth rule: the call-time gate (depthExceeded) and admission both use it.
func depthExceeds(depth, maxDepth int) bool {
	return depth+1 > max(maxDepth, 1)
}

// admitLocked admits one start and takes what it holds; the caller holds mu and has checked
// closed. A start from inside a run passes the depth gate on its chain depth. It then joins the
// run tree (RunTree.Reserve): a nested start passes the run caps of its root (the lowest
// max_runs_per_root on its path) and of its session there, counted across every manager and both
// kinds of run; a call at AgentDepth above 0 that carries no node is refused (fail closed). A
// local start then takes a slot, or else fails when it comes from inside a run (errSubagentNoSlot:
// a run that holds a slot, or waits for one that does, and waits for a queued child of its own
// could never be freed), or else takes a queue place, or else fails (errSubagentQueueFull), and
// its node is released. A remote start takes no slot or queue place. undoReservationLocked and
// releaseRemote give an admitted start's places back.
func (m *SubagentManager) admitLocked(a runAdmission, local bool) (holdsSlot bool, node *RunNode, err error) {
	if a.fromRun() && depthExceeds(a.runDepth(), a.maxDepth) {
		return false, nil, errSubagentDepth
	}
	if a.depth > 0 && a.parent == "" {
		return false, nil, ErrRunTreeParentGone
	}
	kind, state := RunNodeRemoteSubagent, RunNodeRunning
	if local {
		kind, state = RunNodeSubagent, RunNodeQueued
	}
	reservation := RunReservation{Parent: a.parent, Session: a.session, Kind: kind, State: state, MaxPerRoot: a.maxPerRoot, MaxPerSession: a.maxPerSession}
	if node, err = m.tree.Reserve(reservation); err != nil {
		return false, nil, err
	}
	if local {
		// The slot is taken here, not in the run's goroutine, so a free slot is never refused
		// because runs that were just started have not reached it yet.
		switch {
		case m.running < a.limit:
			m.running++
			holdsSlot = true
			node.SetState(RunNodeRunning)
		case a.fromRun():
			node.Release()
			return false, nil, errSubagentNoSlot
		case m.queued < a.maxQueued:
			m.queued++
		default:
			node.Release()
			return false, nil, errSubagentQueueFull
		}
	}
	return holdsSlot, node, nil
}
