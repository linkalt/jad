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
	defer h.endMutation(key)
	unlock := h.sessionLocks.lock(reference)
	defer unlock()
	processUnlock, err := h.lockSessionProcess(r.Context(), reference, profile, "")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	defer processUnlock()
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
	if h.maintenance == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "session reset is unavailable"})
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
	// The gate is taken before the per-session lock (see handleSessionCompact).
	if !h.beginMutation(key) {
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: "session profile is already changing"})
		return
	}
	defer h.endMutation(key)
	unlock := h.sessionLocks.lock(reference)
	defer unlock()
	// A run holds the transcript lock until it ends, so the check has to come
	// before waiting for that lock: after it, the run is over and the reset would
	// wipe the answer it just wrote. The session lock above serializes this check
	// with a run start, and the mutation gate keeps a new run from beginning.
	if h.runIsActive(key) {
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: "cannot reset a profile while a run is active"})
		return
	}
	processUnlock, err := h.lockSessionProcess(r.Context(), reference, profile, "")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	defer processUnlock()
	if err := resolved.Session.Reset(r.Context()); err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"reset": true})
}
