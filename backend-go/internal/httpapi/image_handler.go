package httpapi

import (
	"net/http"
	"strconv"
)

// handleSessionImage serves one file of the session's image store. The caller has been
// authorised already. The body is an image the server wrote itself, but it is sent with
// nosniff and a sandbox so a browser never runs it as anything else.
func (h *Handler) handleSessionImage(w http.ResponseWriter, r *http.Request, reference, profile, name string) {
	if h.images == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "stored images are unavailable"})
		return
	}
	image, err := h.images.ReadSessionImage(r.Context(), reference, profile, name)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	header := w.Header()
	header.Set("Content-Type", image.ContentType)
	header.Set("Content-Length", strconv.Itoa(len(image.Data)))
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "sandbox")
	// A name is the hash of its bytes, so the answer for a name never changes.
	header.Set("Cache-Control", "private, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(image.Data)
}
