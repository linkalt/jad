package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// CheckpointListing is the body of GET /api/sessions/{id}/checkpoints.
type CheckpointListing struct {
	Enabled     bool `json:"enabled"`
	Checkpoints any  `json:"checkpoints"`
}

// CheckpointRestoreRequest is the validated body of a restore. An empty Mode
// takes the session's default.
type CheckpointRestoreRequest struct {
	DryRun bool
	Mode   string
}

// SessionCheckpoints is implemented by the session a maintenance resolver
// returns when run checkpoints are available. It reports a refusal as a
// *StatusError: 404 unknown id, 409 nothing to restore, 422 git unavailable.
type SessionCheckpoints interface {
	// ListCheckpoints lists the checkpoints; runIsLive says whether a run of the
	// session is running, so a record left open by a stopped server is reported
	// as interrupted rather than open.
	ListCheckpoints(ctx context.Context, runIsLive bool) (CheckpointListing, error)
	RestoreCheckpoint(ctx context.Context, id string, request CheckpointRestoreRequest) (any, error)
}

func (h *Handler) handleCheckpointList(w http.ResponseWriter, r *http.Request, reference, profile string) {
	if h.maintenance == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "checkpoints are unavailable"})
		return
	}
	resolved, err := h.maintenance.ResolveMaintenance(r.Context(), reference, profile)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	checkpoints, ok := resolved.Session.(SessionCheckpoints)
	if !ok {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "checkpoints are unavailable"})
		return
	}
	// A run's status turns from "running" only after its end snapshot is written
	// (the checkpoint is finished in a defer inside the run), so an open record
	// with no live run belongs to a run that was cut short.
	key := h.runKey(ResolvedSession{Reference: resolved.Reference, Profile: resolved.Profile})
	run := h.runs.current(key)
	listing, err := checkpoints.ListCheckpoints(r.Context(), run != nil && run.state() == "running")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

func (h *Handler) handleCheckpointRestore(w http.ResponseWriter, r *http.Request, reference, profile, checkpointID string) {
	if h.maintenance == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "checkpoints are unavailable"})
		return
	}
	request, err := readCheckpointRestoreRequest(w, r, h.maxBodyBytes)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	unlock := h.sessionLocks.lock(reference)
	defer unlock()
	resolved, err := h.maintenance.ResolveMaintenance(r.Context(), reference, profile)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	checkpoints, ok := resolved.Session.(SessionCheckpoints)
	if !ok {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "checkpoints are unavailable"})
		return
	}
	key := h.runKey(ResolvedSession{Reference: resolved.Reference, Profile: resolved.Profile})
	// Held for the whole restore: a run cannot start while files are being rewritten.
	if !h.beginMutation(key) {
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: "session profile is already changing"})
		return
	}
	defer h.endMutation(key)
	// A run holds the cross-process session lock until it ends, so the run is
	// checked first: waiting on that lock would delay the refusal until the run
	// was over, and TypeScript refuses at once.
	if run := h.runs.current(key); run != nil && run.state() == "running" {
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: "cannot restore a checkpoint while a run is active"})
		return
	}
	processUnlock, err := h.lockSessionProcess(r.Context(), reference, profile, "")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	defer processUnlock()
	result, err := checkpoints.RestoreCheckpoint(r.Context(), checkpointID, request)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restore": result})
}

// readCheckpointRestoreRequest reads and validates the body with the messages
// the TypeScript backend uses.
func readCheckpointRestoreRequest(w http.ResponseWriter, r *http.Request, maxBytes int64) (CheckpointRestoreRequest, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return CheckpointRestoreRequest{}, &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
		}
		return CheckpointRestoreRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "invalid JSON body"}
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return CheckpointRestoreRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "request body must be JSON"}
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return CheckpointRestoreRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "request body contains invalid JSON"}
	}
	fields, isObject := value.(map[string]any)
	if !isObject {
		return CheckpointRestoreRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "body must be a JSON object"}
	}
	var request CheckpointRestoreRequest
	for key, field := range fields {
		switch key {
		case "dry_run":
			flag, ok := field.(bool)
			if !ok {
				return CheckpointRestoreRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "dry_run must be a boolean"}
			}
			request.DryRun = flag
		case "mode":
			mode, ok := field.(string)
			if !ok || (mode != "safe" && mode != "full") {
				return CheckpointRestoreRequest{}, &StatusError{Code: http.StatusBadRequest, Message: `mode must be "safe" or "full"`}
			}
			request.Mode = mode
		default:
			return CheckpointRestoreRequest{}, &StatusError{Code: http.StatusBadRequest, Message: "unsupported field: " + key}
		}
	}
	return request, nil
}
