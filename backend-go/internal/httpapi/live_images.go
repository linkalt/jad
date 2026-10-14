package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"jad/backend-go/internal/webui"
)

// keepLiveImages decodes the inline images of a tool output the run just published, so the
// browser can show them before the result is in the transcript. The live message carries
// only their count; the image route of a stored message cannot find a result that is not
// persisted yet. The copies are dropped with the run's other state.
func (r *activeRun) keepLiveImages(item json.RawMessage) {
	part, ok := webui.ToolResultPart(item, 0)
	if !ok || part.InlineImages == 0 {
		return
	}
	images := make([]SessionImage, 0, part.InlineImages)
	for index := 0; index < part.InlineImages; index++ {
		data, contentType, ok := webui.InlineImage(item, index)
		if !ok {
			return
		}
		images = append(images, SessionImage{ContentType: contentType, Data: data})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.liveImages == nil {
		r.liveImages = make(map[string][]SessionImage)
	}
	r.liveImages[part.CallID] = images
}

func (r *activeRun) liveImage(callID string, index int) (SessionImage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	images := r.liveImages[callID]
	if index < 0 || index >= len(images) {
		return SessionImage{}, false
	}
	return images[index], true
}

// handleLiveRunImage serves an image of a tool result of the session's current run. Unlike
// the message image route it needs no transcript record, so it works while the run is going.
func (h *Handler) handleLiveRunImage(w http.ResponseWriter, r *http.Request, reference, profile, callID, position string) {
	index, err := strconv.Atoi(position)
	if err != nil || index < 0 || strconv.Itoa(index) != position {
		writeJSONError(w, &StatusError{Code: http.StatusBadRequest, Message: "image position must be a non-negative integer"})
		return
	}
	resolved, err := h.resolve(r.Context(), reference, profile, "")
	if err != nil {
		writeJSONError(w, err)
		return
	}
	run := h.runs.current(h.runKey(resolved))
	if run == nil {
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "image not found"})
		return
	}
	image, ok := run.liveImage(callID, index)
	if !ok {
		writeJSONError(w, &StatusError{Code: http.StatusNotFound, Message: "image not found"})
		return
	}
	writeImage(w, image, "private, no-store")
}
