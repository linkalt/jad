package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf16"

	"jad/backend-go/internal/tools"
)

const interactionReasonMaxChars = 2_000

func (h *Handler) handleRunInteraction(w http.ResponseWriter, r *http.Request, reference, profile string) {
	resolved, err := h.resolve(r.Context(), reference, profile, "")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	run := h.runs.current(h.runKey(resolved))
	if run == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "no run of this session is waiting for an answer"})
		return
	}
	fields, err := readInteractionBody(w, r, h.maxBodyBytes)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	id, reply, parseErr := parseInteractionReply(fields, run.interactions.Limits())
	if parseErr != nil {
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: parseErr.Error()})
		return
	}
	status := run.interactions.Reply(id, reply)
	if status != tools.InteractionStatusAnswered {
		message := "the run is not waiting for that question"
		if status == tools.InteractionStatusExpired {
			message = "the draft expired before the answer arrived"
		}
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: message})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interaction": map[string]any{
		"interaction_id": id,
		"decision":       reply.Decision,
		"status":         status,
	}})
}

func (h *Handler) handleRunInteractionExtend(w http.ResponseWriter, r *http.Request, reference, profile string) {
	resolved, err := h.resolve(r.Context(), reference, profile, "")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	run := h.runs.current(h.runKey(resolved))
	if run == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "no run of this session is waiting for an answer"})
		return
	}
	fields, err := readInteractionBody(w, r, h.maxBodyBytes)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	id, err := parseInteractionExtend(fields)
	if err != nil {
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: err.Error()})
		return
	}
	status := run.interactions.Extend(id)
	if status != tools.InteractionExtendExtended {
		message := "the run is not waiting for that question"
		switch status {
		case tools.InteractionExtendExpired:
			message = "the draft expired before it could be extended"
		case tools.InteractionExtendAtMax:
			message = "the draft is already at the longest wait this session allows"
		}
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: message})
		return
	}
	pending := run.interactions.Pending()
	if pending == nil {
		writeJSONError(w, &StatusError{Code: http.StatusConflict, Message: "the run is not waiting for that question"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interaction": map[string]any{
		"interaction_id": id,
		"status":         "pending",
		"expires_at":     pending.ExpiresAt,
		"max_expires_at": pending.MaxExpiresAt,
	}})
}

func readInteractionBody(w http.ResponseWriter, r *http.Request, maxBytes int64) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
		}
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "invalid JSON body"}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
		}
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "request body must contain one JSON object"}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "body must be a JSON object"}
	}
	return fields, nil
}

func parseInteractionExtend(fields map[string]json.RawMessage) (string, error) {
	var id string
	if json.Unmarshal(fields["interaction_id"], &id) != nil || id == "" {
		return "", errors.New("interaction_id must be a non-empty string")
	}
	return id, nil
}

func parseInteractionReply(fields map[string]json.RawMessage, limits *tools.InteractionLimits) (string, tools.InteractionReply, error) {
	var id string
	if json.Unmarshal(fields["interaction_id"], &id) != nil || id == "" {
		return "", tools.InteractionReply{}, errors.New("interaction_id must be a non-empty string")
	}
	var decision tools.InteractionDecision
	if json.Unmarshal(fields["decision"], &decision) != nil || (decision != tools.InteractionAccept && decision != tools.InteractionUpdate && decision != tools.InteractionDecline) {
		return "", tools.InteractionReply{}, errors.New("decision must be \"accept\", \"update\" or \"decline\"")
	}
	reply := tools.InteractionReply{Decision: decision}
	if rawReason, exists := fields["reason"]; exists {
		var reason string
		if json.Unmarshal(rawReason, &reason) != nil {
			return "", tools.InteractionReply{}, errors.New("reason must be a string")
		}
		reply.Reason = &reason
	}
	if decision == tools.InteractionUpdate {
		rawDraft, exists := fields["draft"]
		var draft map[string]json.RawMessage
		if !exists || json.Unmarshal(rawDraft, &draft) != nil || draft == nil {
			return "", tools.InteractionReply{}, errors.New("draft must be a JSON object when decision is \"update\"")
		}
		if limits != nil {
			byteCount, err := tools.DraftJSONBytes(rawDraft)
			if err != nil {
				return "", tools.InteractionReply{}, errors.New("draft must be a JSON object when decision is \"update\"")
			}
			if byteCount > limits.MaxDraftBytes {
				return "", tools.InteractionReply{}, fmt.Errorf("draft the payload is %d bytes; the limit is %d", byteCount, limits.MaxDraftBytes)
			}
			depth, err := tools.DraftJSONDepth(rawDraft)
			if err != nil {
				return "", tools.InteractionReply{}, errors.New("draft must be a JSON object when decision is \"update\"")
			}
			if depth > limits.MaxDraftDepth {
				return "", tools.InteractionReply{}, fmt.Errorf("draft the payload nests %d levels; the limit is %d", depth, limits.MaxDraftDepth)
			}
		}
		reply.Draft = append(json.RawMessage(nil), rawDraft...)
	} else if decision == tools.InteractionDecline {
		if reply.Reason == nil {
			return "", tools.InteractionReply{}, errors.New("reason must say why when decision is \"decline\"")
		}
		trimmed := strings.TrimSpace(*reply.Reason)
		if trimmed == "" {
			return "", tools.InteractionReply{}, errors.New("reason must say why when decision is \"decline\"")
		}
		if len(utf16.Encode([]rune(trimmed))) > interactionReasonMaxChars {
			return "", tools.InteractionReply{}, errors.New("reason must be at most 2000 characters")
		}
		reply.Reason = &trimmed
	}
	return id, reply, nil
}
