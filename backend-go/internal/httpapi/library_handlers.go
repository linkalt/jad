package httpapi

import (
	"errors"
	"net/http"

	"jad/backend-go/internal/contentlibrary"
)

func (h *Handler) handleRootLibrary(w http.ResponseWriter, r *http.Request, segments []string) {
	if r.Method != http.MethodGet {
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "not found"})
		return
	}
	if h.library == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "root library is unavailable"})
		return
	}
	switch len(segments) {
	case 2:
		catalog, err := h.library.List(r.Context())
		if err != nil {
			writeRootLibraryError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, catalog)
	case 4:
		kind := contentlibrary.Kind(segments[2])
		if kind != contentlibrary.Prompt && kind != contentlibrary.Template && kind != contentlibrary.Skill {
			writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "not found"})
			return
		}
		asset, found, err := h.library.Read(r.Context(), kind, segments[3])
		if err != nil {
			writeRootLibraryError(w, err)
			return
		}
		if !found {
			writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "library item not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]contentlibrary.Asset{"asset": asset})
	default:
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "not found"})
	}
}

func writeRootLibraryError(w http.ResponseWriter, err error) {
	var configErr *contentlibrary.ConfigError
	if errors.As(err, &configErr) {
		writeJSONError(w, &StatusError{Code: http.StatusUnprocessableEntity, Message: configErr.Error()})
		return
	}
	writeJSONError(w, err)
}
