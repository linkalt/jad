package httpapi

import (
	"encoding/json"
	"net/http"
)

// handleSessionStateContext fires the user trigger. It is refused while a run is active,
// as manual compaction is, because the block is placed after the history that exists now.
func (h *Handler) handleSessionStateContext(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.maintenance == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "state context is unavailable"})
		return
	}
	fields, err := readInteractionBody(w, r, h.maxBodyBytes)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	var prompt string
	if value, exists := fields["prompt"]; exists && string(value) != "null" {
		if json.Unmarshal(value, &prompt) != nil {
			writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "prompt must be a string"})
			return
		}
	}
	var preview bool
	if value, exists := fields["preview"]; exists && string(value) != "null" {
		if json.Unmarshal(value, &preview) != nil {
			writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "preview must be a boolean"})
			return
		}
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
	if !h.beginMutation(key) {
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: "session profile or transcript is already changing"})
		return
	}
	// The gate is cleared before the locks are given back (see handleSessionCompact).
	var unlock, processUnlock func()
	defer func() {
		h.endMutation(key)
		if processUnlock != nil {
			processUnlock()
		}
		unlock()
	}()
	unlock = h.lockSession(reference)
	if h.runIsActive(key) {
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: "cannot write state context while a run is active"})
		return
	}
	processUnlock, err = h.lockSessionProcess(r.Context(), reference, profile, "")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	result, err := resolved.Session.StateContext(r.Context(), prompt, preview)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]StateContextResponse{"statecontext": result})
}
