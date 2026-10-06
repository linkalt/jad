package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func (h *Handler) handleSessionFrontCompact(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.maintenance == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "front compaction is unavailable"})
		return
	}
	messageID, err := readFrontCompactMessageID(w, r, h.maxBodyBytes)
	if err != nil {
		writeJSONError(w, err)
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
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: "session profile or transcript is already changing"})
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
	if run := h.runs.current(key); run != nil && run.state() == "running" {
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: "cannot front compact a profile while a run is active"})
		return
	}
	result, err := resolved.Session.FrontCompact(r.Context(), messageID)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]FrontCompactionResponse{"front_compaction": result})
}

func readFrontCompactMessageID(w http.ResponseWriter, r *http.Request, maxBytes int64) (string, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return "", &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
		}
		return "", &StatusError{Code: http.StatusBadRequest, Message: "invalid JSON body"}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return "", &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
		}
		return "", &StatusError{Code: http.StatusBadRequest, Message: "request body must contain one JSON object"}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return "", &StatusError{Code: http.StatusBadRequest, Message: "message_id must identify an active transcript user message"}
	}
	var messageID string
	if value, exists := fields["message_id"]; !exists || json.Unmarshal(value, &messageID) != nil || !frontCompactMessageIDPattern.MatchString(messageID) {
		return "", &StatusError{Code: http.StatusBadRequest, Message: "message_id must identify an active transcript user message"}
	}
	return messageID, nil
}
