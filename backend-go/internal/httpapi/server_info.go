package httpapi

import "net/http"

// handleServerInfo answers GET /api/server: the server settings a client must follow and
// cannot read elsewhere. Today that is fork_name_max_bytes (server.fork_name_max_bytes),
// the longest name a fork into a new profile may give, so the web client proposes and
// checks names against the server's limit instead of a copy in its own config.
func (h *Handler) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSONError(w, &StatusError{Code: http.StatusMethodNotAllowed, Message: "the server info takes GET"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]int{"fork_name_max_bytes": h.forkNameMaxBytes})
}
