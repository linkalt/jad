package httpapi

import (
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/webui"
)

func (r *activeRun) canRequestInteraction() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.supportsInteraction
}

// setPendingInteraction publishes the run's question, or its end. `ended` says why
// a question left; it rides only on the clearing event and is not replayed.
func (r *activeRun) setPendingInteraction(pending *tools.PendingInteraction, ended *tools.InteractionEnd) {
	if r == nil {
		return
	}
	var next *tools.PendingInteraction
	if pending != nil {
		copy := pending.Clone()
		next = &copy
	}

	r.mu.Lock()
	if r.status != "running" && next != nil {
		r.mu.Unlock()
		return
	}
	if next == nil && r.pendingInteraction == nil {
		r.mu.Unlock()
		return
	}
	r.pendingInteraction = next
	r.interactionKnown = true

	var pendingValue any
	if next != nil {
		pendingValue = next
	}
	payload := map[string]any{"turn_id": r.turnID, "pending": pendingValue}
	if next == nil && ended != nil {
		payload["ended"] = ended
	}
	interactionEvent, err := makeSSEEvent("interaction", payload)
	if err != nil {
		r.mu.Unlock()
		return
	}

	var messageEvent *streamEvent
	if r.latestMessage != nil {
		message := r.snapshotMessageLocked()
		if next == nil {
			message.Pending = nil
		} else {
			message.Pending = pendingToWebUI(*next)
		}
		r.latestMessage = &message
		if event, eventErr := makeSSEEvent("ui.message", map[string]any{"message": message}); eventErr == nil {
			r.latest = &event
			messageEvent = &event
		}
	}
	for queue := range r.subscribers {
		if !queue.push(interactionEvent, false) {
			delete(r.subscribers, queue)
			continue
		}
		if messageEvent != nil && !queue.push(*messageEvent, true) {
			delete(r.subscribers, queue)
		}
	}
	r.mu.Unlock()
}

func pendingToWebUI(pending tools.PendingInteraction) *webui.Interaction {
	return &webui.Interaction{
		ID:           pending.ID,
		Kind:         string(pending.Kind),
		Tool:         pending.Tool,
		ToolCallID:   pending.ToolCallID,
		Draft:        append([]byte(nil), pending.Draft...),
		Title:        pending.Title,
		CreatedAt:    pending.CreatedAt,
		ExpiresAt:    pending.ExpiresAt,
		MaxExpiresAt: pending.MaxExpiresAt,
		ExtendMS:     pending.ExtendMS,
	}
}
