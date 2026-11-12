package httpapi

import (
	"context"
	"net/http"
)

// WorkspaceInfo is one workspace of /api/workspaces (workspace layout only).
type WorkspaceInfo struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Directory string `json:"directory"`
	// Start marks the workspace the server was started in: the default of a catalog route
	// without a scope and of a library request without a workspace.
	Start bool `json:"start,omitempty"`
	// Unavailable says why a listed workspace is not served; empty when it is.
	Unavailable string `json:"unavailable,omitempty"`
}

// WorkspaceLister lists the workspaces; a resolver without it answers an empty list.
type WorkspaceLister interface {
	Workspaces(ctx context.Context) ([]WorkspaceInfo, error)
}

// handleWorkspaces answers GET /api/workspaces with {"workspaces": [...]}: the workspaces of
// the workspace layout, served ones and skipped ones; empty outside it.
func (h *Handler) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSONError(w, &StatusError{Code: http.StatusMethodNotAllowed, Message: "the workspace list takes GET"})
		return
	}
	workspaces := []WorkspaceInfo{}
	if h.workspaces != nil {
		listed, err := h.workspaces.Workspaces(r.Context())
		if err != nil {
			writeJSONError(w, err)
			return
		}
		workspaces = append(workspaces, listed...)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string][]WorkspaceInfo{"workspaces": workspaces})
}
