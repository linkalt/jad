package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// CatalogSessions is an optional extension that lists, reads, writes and deletes the
// session.json of the sessions stored in the catalog layout. The reference is the session id,
// or <root>:<id> when there is more than one session root. A delete is a check, then the
// handler stops the session's runs, then the removal.
type CatalogSessions interface {
	// ListSessionConfigs also reports the files it could not read (with Error set) and whether
	// its read budget cut the listing short.
	ListSessionConfigs(ctx context.Context) (files []CatalogFileInfo, truncated bool, err error)
	ReadSessionConfig(ctx context.Context, reference string) (CatalogFile, error)
	WriteSessionConfig(ctx context.Context, reference string, data []byte, ifMatch, ifNoneMatch string) (info CatalogFileInfo, created bool, err error)
	// CheckSessionConfigDelete refuses a delete whose precondition fails and returns the
	// canonical session reference, which the run registry is keyed by.
	CheckSessionConfigDelete(ctx context.Context, reference, ifMatch string) (string, error)
	DeleteSessionConfig(ctx context.Context, reference, ifMatch string, deleteState bool) error
	// CancelAllSessionChildren asks the sub-agent runs of every profile of a session to stop.
	CancelAllSessionChildren(reference string) int
}

// handleCatalogSessions serves /api/catalog/sessions and /api/catalog/sessions/{reference}.
func (h *Handler) handleCatalogSessions(w http.ResponseWriter, r *http.Request, segments []string) {
	if h.catalogSessions == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "session files are unavailable"})
		return
	}
	switch len(segments) {
	case 3:
		if r.Method != http.MethodGet {
			writeJSONError(w, &StatusError{Code: http.StatusMethodNotAllowed, Message: "method not allowed"})
			return
		}
		files, truncated, err := h.catalogSessions.ListSessionConfigs(r.Context())
		if err != nil {
			writeJSONError(w, err)
			return
		}
		if files == nil {
			files = []CatalogFileInfo{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"files": files, "truncated": truncated})
	case 4:
		h.handleCatalogSession(w, r, segments[3])
	default:
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "not found"})
	}
}

func (h *Handler) handleCatalogSession(w http.ResponseWriter, r *http.Request, reference string) {
	switch r.Method {
	case http.MethodGet:
		file, err := h.catalogSessions.ReadSessionConfig(r.Context(), reference)
		if err != nil {
			writeJSONError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("ETag", file.ETag)
		_, _ = w.Write(file.Data)
	case http.MethodPut:
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeJSONError(w, &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"})
				return
			}
			writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "request body could not be read"})
			return
		}
		unlock := h.lockSession(reference)
		defer unlock()
		info, created, err := h.catalogSessions.WriteSessionConfig(r.Context(), reference, data, strings.TrimSpace(r.Header.Get("If-Match")), strings.TrimSpace(r.Header.Get("If-None-Match")))
		if err != nil {
			writeJSONError(w, err)
			return
		}
		w.Header().Set("ETag", info.ETag)
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, info)
	case http.MethodDelete:
		h.deleteCatalogSession(w, r, reference)
	default:
		writeJSONError(w, &StatusError{Code: http.StatusMethodNotAllowed, Message: "method not allowed"})
	}
}

// deleteCatalogSession removes a session. Its runs are stopped first and waited for, so no
// run is writing to a transcript that is about to go. The state folder stays unless the
// request asks for it with ?state=delete.
func (h *Handler) deleteCatalogSession(w http.ResponseWriter, r *http.Request, reference string) {
	deleteState := false
	switch r.URL.Query().Get("state") {
	case "", "keep":
	case "delete":
		deleteState = true
	default:
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "state must be keep or delete"})
		return
	}
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	unlock := h.lockSession(reference)
	defer unlock()
	canonical, err := h.catalogSessions.CheckSessionConfigDelete(r.Context(), reference, ifMatch)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	if err := h.stopSessionRuns(r.Context(), canonical); err != nil {
		writeJSONError(w, err)
		return
	}
	if err := h.catalogSessions.DeleteSessionConfig(r.Context(), reference, ifMatch, deleteState); err != nil {
		writeJSONError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// stopSessionRuns stops every run of every profile of the session and its handed-off children,
// and waits up to the configured time for the runs to end.
func (h *Handler) stopSessionRuns(ctx context.Context, reference string) error {
	running := h.runs.runningInSession(reference)
	h.catalogSessions.CancelAllSessionChildren(reference)
	for _, run := range running {
		run.stop()
	}
	if len(running) == 0 {
		return nil
	}
	timer := time.NewTimer(h.sessionDeleteStopWait)
	defer timer.Stop()
	for _, run := range running {
		select {
		case <-run.done:
		case <-timer.C:
			return &StatusError{Code: http.StatusConflict, Message: "the session still has an active run; it was asked to stop, try again"}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
