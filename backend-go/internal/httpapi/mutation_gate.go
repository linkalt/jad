package httpapi

import (
	"context"
	"sync"
)

func (h *Handler) beginMutation(key string) bool {
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	if h.mutations == nil {
		h.mutations = make(map[string]struct{})
	}
	if _, exists := h.mutations[key]; exists {
		return false
	}
	h.mutations[key] = struct{}{}
	return true
}

// runIsActive reports whether a run of key is still running.
func (h *Handler) runIsActive(key string) bool {
	run := h.runs.current(key)
	return run != nil && run.state() == "running"
}

func (h *Handler) endMutation(key string) {
	h.mutationMu.Lock()
	delete(h.mutations, key)
	h.mutationMu.Unlock()
}

// markRestoring records that a real checkpoint restore holds key, and returns
// the function that clears it. The caller already holds the mutation gate.
func (h *Handler) markRestoring(key string) func() {
	h.mutationMu.Lock()
	if h.restoring == nil {
		h.restoring = make(map[string]struct{})
	}
	h.restoring[key] = struct{}{}
	h.mutationMu.Unlock()
	return func() {
		h.mutationMu.Lock()
		delete(h.restoring, key)
		h.mutationMu.Unlock()
	}
}

// isRestoring reports whether a checkpoint restore is in flight for key.
func (h *Handler) isRestoring(key string) bool {
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	_, exists := h.restoring[key]
	return exists
}

func (h *Handler) beginRun(key, turnID string) (*activeRun, error) {
	return h.beginRunFrom(key, turnID, "", "")
}

func (h *Handler) beginRunFrom(key, turnID, source, caller string) (*activeRun, error) {
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	if _, exists := h.mutations[key]; exists {
		return nil, &StatusError{Code: 409, Message: "session profile or compaction is already changing"}
	}
	return h.runs.beginFrom(key, turnID, source, caller)
}

const profileMessageSource = "profile_message"

type profileRunTracker struct{ handler *Handler }

// ProfileRuns returns the port that registers profile_message runs in the run registry.
func (h *Handler) ProfileRuns() ProfileRunTracker {
	return profileRunTracker{handler: h}
}

func (t profileRunTracker) BeginProfileRun(reference, profile, turnID, caller string) (context.Context, func(string), error) {
	run, err := t.handler.beginRunFrom(t.handler.runKey(ResolvedSession{Reference: reference, Profile: profile}), turnID, profileMessageSource, caller)
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	finish := func(status string) {
		once.Do(func() {
			run.finish(status, nil)
			run.cancel()
		})
	}
	return run.context(), finish, nil
}
