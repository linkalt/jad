package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"jad/backend-go/internal/contentlibrary"
)

func (h *Handler) handleRootLibrary(w http.ResponseWriter, r *http.Request, segments []string) {
	if len(segments) == 5 && segments[2] == string(contentlibrary.Command) && segments[4] == "expand" && r.Method == http.MethodPost {
		h.handleCommandExpand(w, r, segments[3])
		return
	}
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
		if kind != contentlibrary.Prompt && kind != contentlibrary.Template && kind != contentlibrary.Skill && kind != contentlibrary.Command {
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

// handleCommandExpand fills a custom command's text with {"arguments": "..."} and answers
// {"command": ..., "text": ...}. It writes nothing.
func (h *Handler) handleCommandExpand(w http.ResponseWriter, r *http.Request, id string) {
	if h.library == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "root library is unavailable"})
		return
	}
	var body struct {
		Arguments *string `json:"arguments"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, &StatusError{Code: http.StatusRequestEntityTooLarge, Message: "request body is too large"})
			return
		}
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "body must be {\"arguments\": string}"})
		return
	}
	arguments := ""
	if body.Arguments != nil {
		arguments = *body.Arguments
	}
	asset, found, err := h.library.Read(r.Context(), contentlibrary.Command, id)
	if err != nil {
		writeRootLibraryError(w, err)
		return
	}
	if !found {
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "library item not found"})
		return
	}
	text := contentlibrary.ExpandCommand(asset.Content, arguments)
	if strings.TrimSpace(text) == "" {
		writeJSONError(w, &StatusError{Code: http.StatusUnprocessableEntity, Message: "the command's text is empty"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"command": asset.Command, "text": text})
}

func writeRootLibraryError(w http.ResponseWriter, err error) {
	var configErr *contentlibrary.ConfigError
	if errors.As(err, &configErr) {
		writeJSONError(w, &StatusError{Code: http.StatusUnprocessableEntity, Message: configErr.Error()})
		return
	}
	writeJSONError(w, err)
}
