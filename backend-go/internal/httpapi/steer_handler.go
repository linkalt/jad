package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"jad/backend-go/internal/session"
)

// handleRunSteer queues a user message for the session's running run: POST
// /api/sessions/{ref}/run/steer with {"input": <string or input items>}, the input shapes
// POST /run takes. The run stores it as user input before its next provider round, so the
// model sees it without the run stopping; the stored items are published as a "steer"
// event on the run stream. 202 answers {"steer":{"turn_id","pending"}}. The input is checked
// as POST /run checks it (shape, images and files against the server.max_request_* limits).
// No running run (none, or only finished ones) is 404; a run that has finished taking
// messages, a full queue, or steering turned off (server.steer_max_pending 0) is 409. A
// message accepted but never stored because the run then failed or was stopped is reported
// as a "steer_dropped" event.
func (h *Handler) handleRunSteer(w http.ResponseWriter, r *http.Request, reference, profile string) {
	resolved, err := h.resolve(r.Context(), reference, profile, "")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	run := h.runs.current(h.runKey(resolved))
	if run == nil || run.state() != "running" {
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "no run of this session is running; start a run instead"})
		return
	}
	if h.runs.steerLimit < 1 {
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: "steering a run is turned off (server.steer_max_pending is 0)"})
		return
	}
	fields, err := readInteractionBody(w, r, h.runBodyLimit())
	if err != nil {
		writeJSONError(w, err)
		return
	}
	for key := range fields {
		if key != "input" {
			writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "unknown field " + key + "; the body takes only input"})
			return
		}
	}
	raw, ok := fields["input"]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "input is required"})
		return
	}
	input, err := parseRunInput(raw, h.runInputLimits)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	pending, err := run.steer.Push(input)
	switch {
	case errors.Is(err, session.ErrSteerClosed):
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: "the run is finishing and takes no more messages; start a new run"})
		return
	case errors.Is(err, session.ErrSteerFull):
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: "the run already holds the most queued messages (server.steer_max_pending); wait for its next round"})
		return
	case err != nil:
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"steer": map[string]any{"turn_id": run.turnID, "pending": pending}})
}

// rawItems returns items as JSON values for an event payload.
func rawItems(items [][]byte) []json.RawMessage {
	out := make([]json.RawMessage, len(items))
	for index, item := range items {
		out[index] = json.RawMessage(item)
	}
	return out
}
