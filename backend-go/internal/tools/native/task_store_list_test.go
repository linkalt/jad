package native

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestTaskStoreListPinsOrderAndWindow pins List: a limit returns the first matches of the
// full order with the same records, and HasMore reports whether matches were cut. The
// stamps repeat on purpose, so the ID tiebreak decides some of the order.
func TestTaskStoreListPinsOrderAndWindow(t *testing.T) {
	settings := taskTestSettings(t.TempDir())
	settings.MaxTasks = 60
	store := NewTaskStore(settings.StorageDir)
	if err := os.MkdirAll(settings.StorageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statuses := []string{"todo", "in-progress", "blocked", "done"}
	priorities := []string{"low", "normal", "high", "urgent"}
	var tasks []string
	for index := 0; index < 40; index++ {
		tasks = append(tasks, fmt.Sprintf(`{"id":"T-%03d","title":"task-%02d","status":"%s","priority":"%s","tags":["group-%d"],"documents":[],"created_at":"2026-01-01T00:00:00.000Z","updated_at":"2026-01-01T00:00:%02d.000Z"}`,
			index+1, index, statuses[index%len(statuses)], priorities[index%len(priorities)], index%3, index%5))
	}
	document := fmt.Sprintf(`{"schema_version":1,"next_id":41,"tasks":[%s]}`, strings.Join(tasks, ","))
	if err := os.WriteFile(filepath.Join(settings.StorageDir, taskStorageFileName), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	// The intended order, written from the sort rule and not from List: status rank, then
	// priority rank, then updated_at newest first, then ID ascending.
	independent := make([]TaskRecord, 0, len(tasks))
	for index := 0; index < 40; index++ {
		independent = append(independent, TaskRecord{
			ID: fmt.Sprintf("T-%03d", index+1), Status: TaskStatus(statuses[index%len(statuses)]),
			Priority: TaskPriority(priorities[index%len(priorities)]), UpdatedAt: fmt.Sprintf("2026-01-01T00:00:%02d.000Z", index%5),
		})
	}
	sort.SliceStable(independent, func(i, j int) bool {
		left, right := independent[i], independent[j]
		if statusRank(left.Status) != statusRank(right.Status) {
			return statusRank(left.Status) < statusRank(right.Status)
		}
		if priorityRank(left.Priority) != priorityRank(right.Priority) {
			return priorityRank(left.Priority) < priorityRank(right.Priority)
		}
		if left.UpdatedAt != right.UpdatedAt {
			return left.UpdatedAt > right.UpdatedAt
		}
		return left.ID < right.ID
	})
	all, err := store.List(TaskListFilter{Status: "all", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for index := range independent {
		if all.Tasks[index].ID != independent[index].ID {
			t.Fatalf("position %d: ID = %s, want %s", index, all.Tasks[index].ID, independent[index].ID)
		}
	}
	filters := []TaskListFilter{
		{Status: "all"},
		{Statuses: []TaskStatus{TaskTodo, TaskInProgress, TaskBlocked}},
		{Status: "all", Priority: TaskPriorityHigh},
		{Status: "all", Tag: "group-1"},
	}
	for _, filter := range filters {
		full, err := store.List(TaskListFilter{Status: filter.Status, Statuses: filter.Statuses, Priority: filter.Priority, Tag: filter.Tag, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, limit := range []int{0, 1, 3, 10, len(full.Tasks), len(full.Tasks) + 5} {
			filter.Limit = limit
			got, err := store.List(filter)
			if err != nil {
				t.Fatal(err)
			}
			want := full.Tasks
			if limit < len(want) {
				want = want[:limit]
			}
			if len(got.Tasks) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got.Tasks, want)) {
				t.Fatalf("filter %+v limit %d: tasks = %+v, want %+v", filter, limit, got.Tasks, want)
			}
			if got.HasMore != (limit < len(full.Tasks)) {
				t.Fatalf("filter %+v limit %d: HasMore = %v", filter, limit, got.HasMore)
			}
		}
	}
}
