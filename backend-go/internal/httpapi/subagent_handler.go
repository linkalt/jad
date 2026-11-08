package httpapi

import (
	"encoding/json"
	"net/http"
)

// handleSubagent serves the completed steps of the child behind one subagent tool call.
//
//	GET /api/sessions/{reference}/subagents/{callId}[?profile=<profile>]
//	200 {"subagent": {"agent"?: string, "steps": [{"id", "status": "completed", "parts": [...]}]}}
//	404 {"error": "subagent transcript not found"}  (no child folder for the call id)
//
// The transcript routes carry only {agent?, steps: <count>} on a subagent tool_result; this
// route is where a browser reads the steps, once, when the user opens them. The child is
// found by the call id alone and read as it is: a child still running gives the steps it
// has completed so far (an empty list before the first), and the answer is never cached,
// since a later read may hold more. The caller has been authorised already.
func (h *Handler) handleSubagent(w http.ResponseWriter, r *http.Request, reference, profile, callID string) {
	if h.subagents == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "subagent transcripts are unavailable"})
		return
	}
	body, err := h.subagents.ReadSubagentSnapshot(r.Context(), reference, profile, callID)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}
