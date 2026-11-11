package app

import (
	"context"
	"errors"
	"sync"
)

// liveRunStops are the persistent runs going now, each with its own lineage key (the key its
// calls' handles and sub-agent runs carry, native.RunLineageOf), so that a stop of the user's
// run reaches what the run starts after the stop's fan-out has looked.
//
// A user stop cancels the run and then stops the owner's work (CancelSessionChildren); a session
// delete does the same in the other order. Both choose the work first and stop it after, so a
// call of the stopped run still in flight (a tool that registers a handle before it sees the
// cancel) can start work the fan-out did not see. The fan-out notes its reason on the owner's runs
// first (note); a run that ends with its context cancelled and a noted reason stops the work under
// its key once its runner returned and it starts nothing more (end). A run that had ended before
// the note started nothing after the fan-out looked, so the fan-out reached all of it. A run that
// started after the note is not noted, and one the fan-out noted but nothing cancelled (a reset
// finds no run going; a stop of one run that does not reach another) cascades nothing: the work of
// earlier and later runs is left to the fan-outs that name it.
type liveRunStops struct {
	mu   sync.Mutex
	runs map[*liveRunStop]struct{}
}

// liveRunStop is one persistent run: its owner (session key), the session's owner prefix (every
// profile of the session, what a session delete names), its lineage key and the reason a fan-out
// noted on it.
type liveRunStop struct {
	owner, session, key, reason string
}

// begin records a run of profile in session reference under key, before any call of it can start
// work.
func (s *liveRunStops) begin(reference, profile, key string) *liveRunStop {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs == nil {
		s.runs = map[*liveRunStop]struct{}{}
	}
	run := &liveRunStop{owner: sessionKey(reference, profile), session: sessionKey(reference, ""), key: key}
	s.runs[run] = struct{}{}
	return run
}

// note gives reason to every run going now whose owner match accepts and that has none yet. A
// session delete replaces a reason noted before it: its end is the stronger one (the session's
// records go), as the async manager's delete wins over a stop (asyncmgr endRemove).
func (s *liveRunStops) note(match func(owner string) bool, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for run := range s.runs {
		if (run.reason == "" || reason == reasonSessionDelete) && match(run.owner) {
			run.reason = reason
		}
	}
}

// end forgets run and returns the reason noted on it, if any. Under the same lock as note, so a
// fan-out either noted the run before this or comes after it.
func (s *liveRunStops) end(run *liveRunStop) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runs, run)
	return run.reason
}

// endRunStop ends run once its runner returned, and stops the work under its key when a fan-out
// noted it and its context was cancelled (a stop reached it; a run limit's timeout is not one).
// After a session delete the work is deleted as the delete's own fan-out deletes it
// (deleteDescendants): the deleted session's handles lose their records too. The stops are
// cancels outside every lock; it does not wait.
func (r *Resolver) endRunStop(ctx context.Context, run *liveRunStop) {
	reason := r.runStops.end(run)
	if reason == "" || !errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	if reason == reasonSessionDelete {
		r.deleteDescendants(run.key, run.session, reason)
		return
	}
	r.cancelDescendants(run.key, reason)
}
