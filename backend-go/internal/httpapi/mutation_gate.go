package httpapi

func (h *Handler) beginMutation(key string) bool {
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	if h.mutations == nil {
		h.mutations = make(map[string]struct{})
	}
	if _, exists := h.mutations[key]; exists {
		return false
	}
	h.mutations[key] = struct{}{}
	return true
}

func (h *Handler) endMutation(key string) {
	h.mutationMu.Lock()
	delete(h.mutations, key)
	h.mutationMu.Unlock()
}

func (h *Handler) beginRun(key, turnID string) (*activeRun, error) {
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	if _, exists := h.mutations[key]; exists {
		return nil, &StatusError{Code: 409, Message: "session profile or compaction is already changing"}
	}
	return h.runs.begin(key, turnID)
}
