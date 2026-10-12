package app

import (
	"encoding/json"
	"fmt"

	"example.com/agent-harness/backend-go/internal/transcript"
)

// requestDraftStateContext is the request_draft provider section: the latest draft
// the user accepted, or no item when none was accepted.
type requestDraftStateContext struct {
	Items   []requestDraftStateContextItem `json:"items"`
	HasMore bool                           `json:"has_more"`
}

type requestDraftStateContextItem struct {
	Source     string          `json:"source"`
	Action     string          `json:"action"`
	AcceptedAt string          `json:"accepted_at"`
	Draft      json.RawMessage `json:"draft"`
}

func collectRequestDraftStateContext(store *transcript.TurnStore) (*requestDraftStateContext, error) {
	if store == nil {
		return nil, fmt.Errorf("request_draft statecontext store is not available")
	}
	accepted, exists, err := store.ReadAcceptedRequest()
	if err != nil {
		return nil, err
	}
	state := &requestDraftStateContext{Items: []requestDraftStateContextItem{}}
	if exists {
		state.Items = append(state.Items, requestDraftStateContextItem{
			Source: accepted.Source, Action: accepted.Action, AcceptedAt: accepted.AcceptedAt, Draft: accepted.Draft,
		})
	}
	return state, nil
}

func isRequestDraftSection(value any) bool {
	_, ok := value.(*requestDraftStateContext)
	return ok
}

// dropRequestDraft empties the draft section. It is the last thing trimmed: the
// user accepted that draft explicitly, so every other provider gives up its items first.
func dropRequestDraft(providers orderedStateContextProviders) bool {
	for _, provider := range providers {
		if state, ok := provider.value.(*requestDraftStateContext); ok && state.TrimStateContextItem() {
			return true
		}
	}
	return false
}

func (state *requestDraftStateContext) StateContextItemCount() int {
	if state == nil {
		return 0
	}
	return len(state.Items)
}

func (state *requestDraftStateContext) TrimStateContextItem() bool {
	if state == nil || len(state.Items) == 0 {
		return false
	}
	state.Items = state.Items[:0]
	state.HasMore = true
	return true
}
