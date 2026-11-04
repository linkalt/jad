package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// ForkRequest is the body of POST /api/sessions/<ref>/fork.
type ForkRequest struct {
	// Session is the new session's reference; it must not exist yet.
	Session string `json:"session"`
	// MessageID names a message of the session snapshot; the fork point is the turn that
	// holds it. Cursor names the turn by its cursor instead. With neither, the whole
	// transcript is copied.
	MessageID string `json:"message_id,omitempty"`
	Cursor    *int64 `json:"cursor,omitempty"`
	// KeepTurn keeps that turn as well; false ends the copy just before it, so the new
	// session can ask that turn's question again. Default true.
	KeepTurn bool `json:"keep_turn"`
}

// ForkResult answers a fork.
type ForkResult struct {
	Session string `json:"session"`
	Profile string `json:"profile"`
	Turns   int    `json:"turns"`
	Bytes   int64  `json:"bytes"`
	Images  int    `json:"images"`
}

// ProfileFork is implemented by a resolver that can fork a session profile.
type ProfileFork interface {
	ForkProfile(ctx context.Context, reference, profile string, request ForkRequest) (ForkResult, error)
}

func (h *Handler) handleSessionFork(w http.ResponseWriter, r *http.Request, reference, profile string) {
	fork, ok := h.resolver.(ProfileFork)
	if !ok {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "session fork is unavailable"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSONError(w, &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"})
			return
		}
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "request body could not be read"})
		return
	}
	request, err := parseForkRequest(body)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	result, err := fork.ForkProfile(r.Context(), reference, profile, request)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// parseForkRequest reads {"session", "cursor"?, "keep_turn"?}; keep_turn defaults to true.
func parseForkRequest(body []byte) (ForkRequest, error) {
	refuse := func(message string) error { return &StatusError{Code: http.StatusBadRequest, Message: message} }
	var raw struct {
		Session   *string `json:"session"`
		MessageID *string `json:"message_id"`
		Cursor    *int64  `json:"cursor"`
		KeepTurn  *bool   `json:"keep_turn"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil || decoder.More() {
		return ForkRequest{}, refuse(`the body must be {"session": <new session>, "message_id"?: <message id> or "cursor"?: <turn cursor>, "keep_turn"?: <boolean>}`)
	}
	if raw.Session == nil || strings.TrimSpace(*raw.Session) == "" {
		return ForkRequest{}, refuse("session must name the new session")
	}
	if message := invalidSessionReferenceMessage(*raw.Session); message != "" {
		return ForkRequest{}, refuse("session: " + message)
	}
	if raw.Cursor != nil && *raw.Cursor < 0 {
		return ForkRequest{}, refuse("cursor must not be negative")
	}
	if raw.MessageID != nil && strings.TrimSpace(*raw.MessageID) == "" {
		return ForkRequest{}, refuse("message_id must not be empty")
	}
	if raw.MessageID != nil && raw.Cursor != nil {
		return ForkRequest{}, refuse("give message_id or cursor, not both")
	}
	if raw.KeepTurn != nil && raw.MessageID == nil && raw.Cursor == nil {
		return ForkRequest{}, refuse("keep_turn needs message_id or cursor")
	}
	request := ForkRequest{Session: *raw.Session, Cursor: raw.Cursor, KeepTurn: true}
	if raw.MessageID != nil {
		request.MessageID = *raw.MessageID
	}
	if raw.KeepTurn != nil {
		request.KeepTurn = *raw.KeepTurn
	}
	return request, nil
}
