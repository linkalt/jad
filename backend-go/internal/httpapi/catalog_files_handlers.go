package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
)

// CatalogFileInfo identifies one catalog file and the version of its bytes.
type CatalogFileInfo struct {
	ID   string `json:"id"`
	ETag string `json:"etag"`
}

// CatalogFile is one catalog file as stored.
type CatalogFile struct {
	CatalogFileInfo
	Data []byte
}

// CatalogFiles is an optional extension that lists, reads, writes and deletes the files of
// a configuration catalog (profiles). The kind is the URL word ("profiles"). Writes and
// deletes carry the HTTP preconditions; the extension checks them.
type CatalogFiles interface {
	ListCatalogFiles(ctx context.Context, kind string) ([]CatalogFileInfo, error)
	ReadCatalogFile(ctx context.Context, kind, id string) (CatalogFile, error)
	WriteCatalogFile(ctx context.Context, kind, id string, data []byte, ifMatch, ifNoneMatch string) (info CatalogFileInfo, created bool, err error)
	DeleteCatalogFile(ctx context.Context, kind, id, ifMatch string) error
}

// handleCatalogFiles serves /api/catalog/{kind} and /api/catalog/{kind}/{category}/{name}.
func (h *Handler) handleCatalogFiles(w http.ResponseWriter, r *http.Request, segments []string) {
	if h.catalogFiles == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "catalog files are unavailable"})
		return
	}
	if len(segments) < 3 || len(segments) > 5 || len(segments) == 4 {
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "not found"})
		return
	}
	kind := segments[2]
	if len(segments) == 3 {
		if r.Method != http.MethodGet {
			writeJSONError(w, &StatusError{Code: http.StatusMethodNotAllowed, Message: "method not allowed"})
			return
		}
		files, err := h.catalogFiles.ListCatalogFiles(r.Context(), kind)
		if err != nil {
			writeJSONError(w, err)
			return
		}
		if files == nil {
			files = []CatalogFileInfo{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"files": files})
		return
	}
	id := segments[3] + "/" + segments[4]
	switch r.Method {
	case http.MethodGet:
		file, err := h.catalogFiles.ReadCatalogFile(r.Context(), kind, id)
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
		info, created, err := h.catalogFiles.WriteCatalogFile(r.Context(), kind, id, data, strings.TrimSpace(r.Header.Get("If-Match")), strings.TrimSpace(r.Header.Get("If-None-Match")))
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
		if err := h.catalogFiles.DeleteCatalogFile(r.Context(), kind, id, strings.TrimSpace(r.Header.Get("If-Match"))); err != nil {
			writeJSONError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSONError(w, &StatusError{Code: http.StatusMethodNotAllowed, Message: "method not allowed"})
	}
}
