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
	id, seq := "", uint64(0)
	if next != nil {
		id, seq = next.ID, next.Seq
	} else if ended != nil {
		id, seq = ended.InteractionID, ended.Seq
	}
	if seq != 0 {
		if seq <= r.interactionSeq[id] {
			r.mu.Unlock()
			return
		}
		if r.interactionSeq == nil {
			r.interactionSeq = make(map[string]uint64)
		}
		r.interactionSeq[id] = seq
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

	for queue := range r.subscribers {
		if !queue.push(interactionEvent, false) {
			delete(r.subscribers, queue)
		}
	}
	r.mu.Unlock()
	r.publishInteractionMessage(next)
}

// publishInteractionMessage re-publishes the message with the pending question. The
// content read and the encode run outside the lock; the result is committed only if
// neither the message nor the question changed meanwhile. If the question changed, the
// call that changed it publishes the message.
func (r *activeRun) publishInteractionMessage(next *tools.PendingInteraction) {
	for {
		r.mu.Lock()
		if r.latestMessage == nil || r.pendingInteraction != next {
			r.mu.Unlock()
			return
		}
		base := r.latestMessage
		cache, final := r.content, r.contentFinal
		r.mu.Unlock()

		message := *base
		message.Parts = append([]webui.Part(nil), message.Parts...)
		if !final && cache != nil && message.Text == "" {
			injectContent(&message, cache.Snapshot())
		}
		if next == nil {
			message.Pending = nil
		} else {
			message.Pending = pendingToWebUI(*next)
		}
		event, err := makeSSEEvent("ui.message", map[string]any{"message": message})

		r.mu.Lock()
		if r.pendingInteraction != next {
			r.mu.Unlock()
			return
		}
		if r.latestMessage != base {
			r.mu.Unlock()
			continue
		}
		r.latestMessage = &message
		if err == nil {
			r.latest = &event
			for queue := range r.subscribers {
				if !queue.push(event, true) {
					delete(r.subscribers, queue)
				}
			}
		}
		r.mu.Unlock()
		return
	}
}

func pendingToWebUI(pending tools.PendingInteraction) *webui.Interaction {
	return &webui.Interaction{
		ID:           pending.ID,
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
