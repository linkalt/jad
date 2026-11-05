package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

func (h *Handler) handleSessionModelDefault(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.settings == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "session model updates are unavailable"})
		return
	}
	fields, err := readSettingsObject(w, r, h.maxBodyBytes)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	var modelDefault string
	if raw, exists := fields["model_default"]; !exists || json.Unmarshal(raw, &modelDefault) != nil || strings.TrimSpace(modelDefault) == "" {
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "model_default must be a non-empty string"})
		return
	}
	modelDefault = strings.TrimSpace(modelDefault)
	unlock := h.lockSession(reference)
	defer unlock()
	processUnlock, err := h.lockSessionSettingsProcess(r.Context(), reference)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	defer processUnlock()
	updated, err := h.settings.UpdateModelDefault(r.Context(), reference, profile, modelDefault, strings.TrimSpace(r.Header.Get("If-Match")))
	if err != nil {
		writeJSONError(w, err)
		return
	}
	if updated.ProfileETag != "" {
		w.Header().Set("ETag", updated.ProfileETag)
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) handleSessionFiles(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.settings == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "session file listing is unavailable"})
		return
	}
	listing, err := h.settings.ListSessionFiles(r.Context(), reference, profile)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

func readSettingsObject(w http.ResponseWriter, r *http.Request, maxBytes int64) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	var body json.RawMessage
	if err := decoder.Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
		}
		if errors.Is(err, io.EOF) {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "request body must be JSON"}
		}
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "request body contains invalid JSON"}
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
		}
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "request body contains invalid JSON"}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return map[string]json.RawMessage{}, nil
	}
	return fields, nil
}
