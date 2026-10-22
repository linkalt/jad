package native

import (
	"fmt"

	"jad/backend-go/internal/asyncmgr"
)

// AsyncStateContextItem is one pending handle in the prompt notice: no summary, no paths.
type AsyncStateContextItem struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	State  string `json:"state"`
	Label  string `json:"label,omitempty"`
	Unread bool   `json:"unread,omitempty"`
}

// AsyncStateContext is the async provider section of a state-context snapshot: the owner's
// live and unread handles (at most list_default_limit of them) and the counts of the rest.
// Terminal handles that were read are never listed.
type AsyncStateContext struct {
	Active            int                     `json:"active"`
	Unread            int                     `json:"unread"`
	Items             []AsyncStateContextItem `json:"items"`
	HasMore           bool                    `json:"has_more"`
	OmittedBySnapshot int                     `json:"omitted_by_snapshot,omitempty"`
}

// CollectAsyncStateContext lists the owner's pending handles from the async manager.
func CollectAsyncStateContext(manager *asyncmgr.Manager, owner string) (*AsyncStateContext, error) {
	if manager == nil {
		return nil, fmt.Errorf("async statecontext manager is not available")
	}
	result, err := manager.List(asyncmgr.ListQuery{Owner: owner, State: "active"})
	if err != nil {
		return nil, fmt.Errorf("list async handles: %w", err)
	}
	items := make([]AsyncStateContextItem, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, AsyncStateContextItem{
			ID: item.ID, Kind: string(item.Kind), State: string(item.State), Label: item.Label, Unread: item.Unread,
		})
	}
	return &AsyncStateContext{Active: result.Counts.Active, Unread: result.Counts.Unread, Items: items, HasMore: result.Next != ""}, nil
}

func (section *AsyncStateContext) StateContextItemCount() int {
	if section == nil {
		return 0
	}
	return len(section.Items)
}

func (section *AsyncStateContext) TrimStateContextItem() bool {
	if section == nil || len(section.Items) == 0 {
		return false
	}
	section.Items = section.Items[:len(section.Items)-1]
	section.OmittedBySnapshot++
	return true
}
