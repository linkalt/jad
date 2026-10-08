package native

import (
	"fmt"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// SubagentStateContext is the subagent provider section of a compaction snapshot. It
// lists the runs of one session so the model still has the ids it was given after its
// history was compacted. It can drop its least-recent item when the global snapshot
// budget is reached.
type SubagentStateContext struct {
	Items             []SubagentStateContextItem `json:"items"`
	HasMore           bool                       `json:"has_more"`
	OmittedBySnapshot int                        `json:"omitted_by_snapshot,omitempty"`
}

// SubagentStateContextItem is one run. The task is the short preview the run list
// keeps, never the whole task; the answer is not copied, because the model collects it
// by id.
type SubagentStateContextItem struct {
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	State     string `json:"state"`
	Task      string `json:"task"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
	Rounds    int    `json:"rounds,omitempty"`
	Tokens    int64  `json:"tokens,omitempty"`
}

// CollectSubagentStateContext returns the configured bounded summary of the runs that
// owner started, newest first.
func CollectSubagentStateContext(manager *SubagentManager, owner string, settings sessionfs.NativeSubagentStateContextSettings) (*SubagentStateContext, error) {
	if manager == nil {
		return nil, fmt.Errorf("subagent statecontext manager is not available")
	}
	allowed := make(map[string]struct{}, len(settings.States))
	for _, state := range settings.States {
		allowed[state] = struct{}{}
	}
	matching := make([]subagentRunView, 0)
	for _, view := range manager.list(owner) {
		if _, ok := allowed[view.state]; ok {
			matching = append(matching, view)
		}
	}
	count := min(len(matching), max(int(settings.Limit), 0))
	items := make([]SubagentStateContextItem, 0, count)
	for _, view := range matching[:count] {
		item := SubagentStateContextItem{
			ID:        view.id,
			Agent:     view.agent,
			State:     view.state,
			Task:      view.task,
			StartedAt: view.startedAt.UTC().Format(time.RFC3339),
			Rounds:    view.result.Rounds,
			Tokens:    view.result.Tokens,
		}
		if !view.endedAt.IsZero() {
			item.EndedAt = view.endedAt.UTC().Format(time.RFC3339)
		}
		items = append(items, item)
	}
	return &SubagentStateContext{Items: items, HasMore: len(matching) > count}, nil
}

func (section *SubagentStateContext) StateContextItemCount() int {
	if section == nil {
		return 0
	}
	return len(section.Items)
}

func (section *SubagentStateContext) TrimStateContextItem() bool {
	if section == nil || len(section.Items) == 0 {
		return false
	}
	section.Items = section.Items[:len(section.Items)-1]
	section.OmittedBySnapshot++
	return true
}
