package httpapi

import (
	"encoding/json"
	"net/http"
)

func (h *Handler) handleSessionCompact(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.maintenance == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "session compaction is unavailable"})
		return
	}
	resolved, err := h.maintenance.ResolveMaintenance(r.Context(), reference, profile)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	if resolved.Session == nil {
		writeJSONError(w, &StatusError{Code: http.StatusInternalServerError, Message: "session maintenance provider is not initialized"})
		return
	}
	key := h.runKey(ResolvedSession{Reference: resolved.Reference, Profile: resolved.Profile})
	// The gate is taken before the per-session lock: a request that finds the
	// session changing is refused at once, as TypeScript does, instead of waiting
	// for the other change to end and then running after it.
	if !h.beginMutation(key) {
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: "session profile or compaction is already changing"})
		return
	}
	release, err := h.lockSessionAndTranscript(r.Context(), reference, profile)
	if err != nil {
		h.endMutation(key)
		writeJSONError(w, err)
		return
	}
	// The gate is cleared before the locks are given back: a start queued behind this
	// compaction takes the session lock the moment it is free, and must not find the gate.
	defer func() {
		h.endMutation(key)
		release()
	}()
	result, err := resolved.Session.Compact(r.Context())
	if err != nil {
		writeJSONError(w, err)
		return
	}
	compaction := map[string]any{
		"compacted":       result.Compacted,
		"compacted_items": result.CompactedItems,
		"retained_items":  result.RetainedItems,
	}
	body := map[string]any{"compaction": compaction}
	if len(result.Usage) > 0 {
		body["usage"] = json.RawMessage(result.Usage)
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *Handler) handleSessionReset(w http.ResponseWriter, r *http.Request, reference, profile string) {
	reset, ok := h.resolver.(ProfileReset)
	if !ok {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "session reset is unavailable"})
		return
	}
	if err := reset.ResetProfile(r.Context(), reference, profile); err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"reset": true})
}

// cancelSessionChildren stops the work the session started that outlives its runs, when
// the resolver tracks any.
func (h *Handler) cancelSessionChildren(reference, profile string) {
	if children, ok := h.resolver.(SessionChildren); ok {
		children.CancelSessionChildren(reference, profile)
	}
}
