package native

import (
	"encoding/json"
	"fmt"
	"sort"

	"jad/backend-go/internal/sessionfs"
)

// ProcessStateSource is the small process-manager view used by the
// statecontext collector.
type ProcessStateSource interface {
	Recover() error
	List(filter string) []ProcessSnapshot
}

// ProcessStateContext is the process provider section included in compaction
// snapshots. It can drop its least-recent item when the global snapshot budget
// is reached.
type ProcessStateContext struct {
	Items             []ProcessStateContextItem `json:"items"`
	HasMore           bool                      `json:"has_more"`
	OmittedBySnapshot int                       `json:"omitted_by_snapshot,omitempty"`
	// Counts, set in summary mode, holds one entry per configured state that has
	// processes; Items then lists only the detail states.
	Counts        []StateContextCount `json:"counts,omitempty"`
	OmittedCounts int                 `json:"omitted_counts,omitempty"`
}

type ProcessStateContextItem struct {
	ID        string          `json:"id"`
	State     string          `json:"state"`
	CWD       string          `json:"cwd"`
	StartedAt string          `json:"started_at"`
	EndedAt   string          `json:"ended_at,omitempty"`
	PID       *int            `json:"pid,omitempty"`
	ExitCode  json.RawMessage `json:"exit_code,omitempty"`
	Signal    json.RawMessage `json:"signal,omitempty"`
}

// CollectProcessStateContext returns the configured bounded process summary.
func CollectProcessStateContext(manager ProcessStateSource, settings sessionfs.NativeProcessStateContextSettings) (*ProcessStateContext, error) {
	if manager == nil {
		return nil, fmt.Errorf("process statecontext manager is not available")
	}
	if err := manager.Recover(); err != nil {
		return nil, fmt.Errorf("recover process statecontext: %w", err)
	}
	allowed := make(map[string]struct{}, len(settings.States))
	for _, state := range settings.States {
		allowed[state] = struct{}{}
	}
	matching := make([]ProcessSnapshot, 0)
	for _, snapshot := range manager.List("") {
		if _, ok := allowed[string(snapshot.State)]; ok {
			matching = append(matching, snapshot)
		}
	}
	sort.Slice(matching, func(i, j int) bool {
		if matching[i].StartedAt != matching[j].StartedAt {
			return matching[i].StartedAt > matching[j].StartedAt
		}
		return matching[i].ID < matching[j].ID
	})
	var counts []StateContextCount
	listed := matching
	if settings.Mode == sessionfs.NativeStateContextModeSummary {
		values := make([]string, 0, len(matching))
		listed = make([]ProcessSnapshot, 0, len(matching))
		for _, snapshot := range matching {
			values = append(values, string(snapshot.State))
			if containsState(settings.DetailStates, string(snapshot.State)) {
				listed = append(listed, snapshot)
			}
		}
		counts = countStates(settings.States, values)
	}
	limit := int(settings.Limit)
	if limit < 0 {
		limit = 0
	}
	count := min(len(listed), limit)
	items := make([]ProcessStateContextItem, 0, count)
	for _, snapshot := range listed[:count] {
		item := ProcessStateContextItem{
			ID:        snapshot.ID,
			State:     string(snapshot.State),
			CWD:       snapshot.CWD,
			StartedAt: snapshot.StartedAt,
			EndedAt:   snapshot.EndedAt,
			ExitCode:  append(json.RawMessage(nil), snapshot.ExitCode...),
			Signal:    append(json.RawMessage(nil), snapshot.Signal...),
		}
		if snapshot.PID != 0 {
			pid := snapshot.PID
			item.PID = &pid
		}
		items = append(items, item)
	}
	return &ProcessStateContext{Items: items, HasMore: len(listed) > count, Counts: counts}, nil
}

func (section *ProcessStateContext) StateContextItemCount() int {
	if section == nil {
		return 0
	}
	return stateContextItemCount(section.Items, section.Counts)
}

func (section *ProcessStateContext) TrimStateContextItem() bool {
	if section == nil {
		return false
	}
	return trimStateContextItem(&section.Items, &section.OmittedBySnapshot, &section.Counts, &section.OmittedCounts)
}
