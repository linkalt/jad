package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ForkRequest is the body of POST /api/sessions/<ref>/fork.
type ForkRequest struct {
	// Session is the new session's reference; it must not exist yet. Exactly one of
	// Session and Profile is set.
	Session string `json:"session"`
	// Profile forks into a new profile of the same session instead: the name of a new
	// profile file in the source profile's category, which must not exist yet. The new
	// profile is a copy of the source profile's file and is added to the session's scope
	// when the scope does not already hold it.
	Profile string `json:"profile,omitempty"`
	// Title is the display name of the new profile; empty keeps Profile as its name.
	Title string `json:"title,omitempty"`
	// State is "copy" (default) to copy the transcript up to the fork point, or "none"
	// for a new session or profile with an empty conversation (a duplicate of the
	// settings only).
	State string `json:"state,omitempty"`
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

// The fork states: copy the transcript up to the fork point, or start empty.
const (
	ForkStateCopy = "copy"
	ForkStateNone = "none"
)

// invalidProfileNameMessage checks the name of a new profile file: at most maxBytes
// (server.fork_name_max_bytes) and the catalog's identifier rule (letters, digits, '-', '_'
// and '.', starting with a letter or digit).
func invalidProfileNameMessage(name string, maxBytes int) string {
	if name == "" {
		return "must name the new profile"
	}
	if len(name) > maxBytes {
		return fmt.Sprintf("must be at most %d bytes (server.fork_name_max_bytes)", maxBytes)
	}
	for index, character := range name {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		case (character == '-' || character == '_' || character == '.') && index > 0:
		default:
			return "may only hold letters, digits, '-', '_' and '.', and must start with a letter or digit"
		}
	}
	return ""
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
	request, err := parseForkRequest(body, h.forkNameMaxBytes)
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

// parseForkRequest reads {"session" or "profile", "title"?, "state"?, "message_id"? or
// "cursor"?, "keep_turn"?}; keep_turn defaults to true and state to copy. A new profile's
// name and title are at most maxNameBytes.
func parseForkRequest(body []byte, maxNameBytes int) (ForkRequest, error) {
	refuse := func(message string) error { return &StatusError{Code: http.StatusBadRequest, Message: message} }
	var raw struct {
		Session   *string `json:"session"`
		Profile   *string `json:"profile"`
		Title     *string `json:"title"`
		State     *string `json:"state"`
		MessageID *string `json:"message_id"`
		Cursor    *int64  `json:"cursor"`
		KeepTurn  *bool   `json:"keep_turn"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil || decoder.More() {
		return ForkRequest{}, refuse(`the body must be {"session": <new session> or "profile": <new profile name>, "title"?: <profile display name>, "state"?: "copy"|"none", "message_id"?: <message id> or "cursor"?: <turn cursor>, "keep_turn"?: <boolean>}`)
	}
	request := ForkRequest{State: ForkStateCopy, KeepTurn: true}
	switch {
	case raw.Session != nil && raw.Profile != nil:
		return ForkRequest{}, refuse("give session or profile, not both")
	case raw.Profile != nil:
		name := *raw.Profile
		if message := invalidProfileNameMessage(name, maxNameBytes); message != "" {
			return ForkRequest{}, refuse("profile: " + message)
		}
		request.Profile = name
	case raw.Session == nil || strings.TrimSpace(*raw.Session) == "":
		return ForkRequest{}, refuse("session must name the new session (or profile a new profile of this session)")
	default:
		if message := invalidSessionReferenceMessage(*raw.Session); message != "" {
			return ForkRequest{}, refuse("session: " + message)
		}
		request.Session = *raw.Session
	}
	if raw.Title != nil {
		if raw.Profile == nil {
			return ForkRequest{}, refuse("title names a new profile; it needs profile")
		}
		title := strings.TrimSpace(*raw.Title)
		if title == "" || len(title) > maxNameBytes || strings.ContainsAny(title, "\r\n") {
			return ForkRequest{}, refuse(fmt.Sprintf("title must be one line of 1 to %d bytes (server.fork_name_max_bytes)", maxNameBytes))
		}
		request.Title = title
	}
	if raw.State != nil {
		if *raw.State != ForkStateCopy && *raw.State != ForkStateNone {
			return ForkRequest{}, refuse(`state must be "copy" or "none"`)
		}
		request.State = *raw.State
	}
	if request.State == ForkStateNone && (raw.MessageID != nil || raw.Cursor != nil) {
		return ForkRequest{}, refuse(`state "none" copies no transcript, so it takes no message_id or cursor`)
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
	request.Cursor = raw.Cursor
	if raw.MessageID != nil {
		request.MessageID = *raw.MessageID
	}
	if raw.KeepTurn != nil {
		request.KeepTurn = *raw.KeepTurn
	}
	return request, nil
}
