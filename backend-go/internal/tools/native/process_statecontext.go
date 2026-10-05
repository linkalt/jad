package native

import (
	"encoding/json"
	"fmt"
	"sort"

	"example.com/agent-harness/backend-go/internal/sessionfs"
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
	limit := int(settings.Limit)
	if limit < 0 {
		limit = 0
	}
	count := min(len(matching), limit)
	items := make([]ProcessStateContextItem, 0, count)
	for _, snapshot := range matching[:count] {
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
	return &ProcessStateContext{Items: items, HasMore: len(matching) > count}, nil
}

func (section *ProcessStateContext) StateContextItemCount() int {
	if section == nil {
		return 0
	}
	return len(section.Items)
}

func (section *ProcessStateContext) TrimStateContextItem() bool {
	if section == nil || len(section.Items) == 0 {
		return false
	}
	section.Items = section.Items[:len(section.Items)-1]
	section.OmittedBySnapshot++
	return true
}
