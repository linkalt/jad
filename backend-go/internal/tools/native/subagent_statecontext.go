package native

import (
	"fmt"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// SubagentStateContext is the subagent provider section of a compaction snapshot. It
// lists the runs of one session so the model still has the ids it was given after its
// history was compacted. It can drop its least-recent item when the global snapshot
// budget is reached.
type SubagentStateContext struct {
	Items             []SubagentStateContextItem `json:"items"`
	HasMore           bool                       `json:"has_more"`
	OmittedBySnapshot int                        `json:"omitted_by_snapshot,omitempty"`
	// Counts, set in summary mode, holds one entry per configured state that has runs;
	// Items then lists only the detail states.
	Counts        []StateContextCount `json:"counts,omitempty"`
	OmittedCounts int                 `json:"omitted_counts,omitempty"`
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
	var counts []StateContextCount
	listed := matching
	if settings.Mode == sessionfs.NativeStateContextModeSummary {
		values := make([]string, 0, len(matching))
		listed = make([]subagentRunView, 0, len(matching))
		for _, view := range matching {
			values = append(values, view.state)
			if containsState(settings.DetailStates, view.state) {
				listed = append(listed, view)
			}
		}
		counts = countStates(settings.States, values)
	}
	count := min(len(listed), max(int(settings.Limit), 0))
	items := make([]SubagentStateContextItem, 0, count)
	for _, view := range listed[:count] {
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
	return &SubagentStateContext{Items: items, HasMore: len(listed) > count, Counts: counts}, nil
}

func (section *SubagentStateContext) StateContextItemCount() int {
	if section == nil {
		return 0
	}
	return stateContextItemCount(section.Items, section.Counts)
}

func (section *SubagentStateContext) TrimStateContextItem() bool {
	if section == nil {
		return false
	}
	return trimStateContextItem(&section.Items, &section.OmittedBySnapshot, &section.Counts, &section.OmittedCounts)
}
