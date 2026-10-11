package httpapi

import (
	"net/http"
	"strconv"
)

// handleSessionImage serves one file of the session's image store. The caller has been
// authorised already. The body is an image the server wrote itself, but it is sent with
// nosniff and a sandbox so a browser never runs it as anything else. A name is the hash of
// its bytes, so the answer for a name never changes.
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
	writeImage(w, image, "private, max-age=31536000, immutable")
}

// handleMessageImage serves an image a tool result still holds inline in the transcript.
// Unlike a stored file its name is not a hash of its bytes, so the answer is not cached.
func (h *Handler) handleMessageImage(w http.ResponseWriter, r *http.Request, reference, profile, messageID, callID, position string) {
	if h.messageImages == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotImplemented, Message: "message images are unavailable"})
		return
	}
	index, err := strconv.Atoi(position)
	if err != nil || index < 0 || strconv.Itoa(index) != position {
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "image position must be a non-negative integer"})
		return
	}
	cursor, err := parseNonNegativeQueryInteger(r.URL.Query().Get("cursor"), "cursor")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	image, err := h.messageImages.ReadMessageImage(r.Context(), reference, profile, messageID, callID, index, cursor)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeImage(w, image, "private, no-store")
}

func writeImage(w http.ResponseWriter, image SessionImage, cacheControl string) {
	header := w.Header()
	header.Set("Content-Type", image.ContentType)
	header.Set("Content-Length", strconv.Itoa(len(image.Data)))
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "sandbox")
	header.Set("Cache-Control", cacheControl)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(image.Data)
}
