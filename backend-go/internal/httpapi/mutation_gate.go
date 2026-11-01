package httpapi

import (
	"jad/backend-go/internal/sessiongate"
)

// newSessionGate returns the gate the resolver shares, or a private one for a resolver
// that does not share it.
func newSessionGate(resolver SessionResolver) *sessiongate.Gate {
	if shared, ok := resolver.(SessionGater); ok {
		if gate := shared.SessionGate(); gate != nil {
			return gate
		}
	}
	return &sessiongate.Gate{}
}

// runIsActive reports whether a run of key is still running.
func (h *Handler) runIsActive(key string) bool {
	run := h.runs.current(key)
	return run != nil && run.state() == "running"
}

// RunActive reports whether a run of the session profile is still running. The resolver
// calls it before a reset, so the refusal is decided in the same registry the run starts in.
func (h *Handler) RunActive(reference, profile string) bool {
	return h.runIsActive(h.runKey(ResolvedSession{Reference: reference, Profile: profile}))
}

func (h *Handler) beginMutation(key string) bool {
	return h.gate.Begin(key)
}

func (h *Handler) endMutation(key string) {
	h.gate.End(key)
}

func (h *Handler) markRestoring(key string) func() {
	return h.gate.MarkRestoring(key)
}

func (h *Handler) isRestoring(key string) bool {
	return h.gate.Restoring(key)
}

func (h *Handler) beginRun(key, turnID string) (*activeRun, error) {
	return h.beginRunFrom(key, turnID, "", "")
}

func (h *Handler) beginRunFrom(key, turnID, source, caller string) (*activeRun, error) {
	var run *activeRun
	free, err := h.gate.WhileFree(key, func() error {
		var err error
		run, err = h.runs.beginFrom(key, turnID, source, caller)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !free {
		return nil, &StatusError{Code: 409, Message: "session profile or compaction is already changing"}
	}
	return run, nil
}
