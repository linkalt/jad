package app

import (
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

func summaryTaskConfig(t *testing.T, mode string, maxBytes int64) (sessionfs.SessionConfig, *native.TaskStore) {
	t.Helper()
	taskSettings := sessionfs.NativeTaskSettings{
		MaxTasks: 200, MaxStorageBytes: 1024 * 1024,
		StateContext: sessionfs.NativeTaskStateContextSettings{
			Statuses: []string{"todo", "in-progress", "done"}, Limit: 100, Mode: mode, DetailStatuses: []string{"in-progress"},
		},
	}
	store := native.NewTaskStore(t.TempDir())
	statuses := []native.TaskStatus{native.TaskInProgress, native.TaskInProgress}
	for index := 0; index < 40; index++ {
		statuses = append(statuses, native.TaskDone)
	}
	for _, status := range statuses {
		if _, err := store.Create(native.TaskInput{Title: strings.Repeat("finished work ", 8), Status: status, Priority: native.TaskPriorityNormal, Tags: []string{}, Documents: []string{}}, taskSettings); err != nil {
			t.Fatal(err)
		}
	}
	config := sessionfs.SessionConfig{
		NativeTask: &taskSettings,
		StateContext: sessionfs.StateContextSettings{
			Enabled: true, Header: sessionfs.DefaultStateContextHeader, Role: "developer",
			Triggers: compactionTrigger([]string{"task"}, maxBytes),
		},
	}
	return config, store
}

func TestSummaryModeTaskBlockIsSmallerAndKeepsTheCounts(t *testing.T) {
	itemsConfig, itemsStore := summaryTaskConfig(t, sessionfs.NativeStateContextModeItems, 1024*1024)
	items, include, err := collectStateContextSnapshot(itemsConfig, stateContextSources{task: itemsStore}, "compaction")
	if err != nil || !include {
		t.Fatalf("items block: include %v, err %v", include, err)
	}
	summaryConfig, summaryStore := summaryTaskConfig(t, sessionfs.NativeStateContextModeSummary, 1024*1024)
	summary, include, err := collectStateContextSnapshot(summaryConfig, stateContextSources{task: summaryStore}, "compaction")
	if err != nil || !include {
		t.Fatalf("summary block: include %v, err %v", include, err)
	}
	if len(summary.Text)*4 > len(items.Text) {
		t.Fatalf("summary block is %d bytes against %d in items mode; want at most a quarter", len(summary.Text), len(items.Text))
	}
	var document struct {
		Providers struct {
			Task struct {
				Items  []native.TaskStateContextItem  `json:"items"`
				Counts []native.TaskStateContextCount `json:"counts"`
			} `json:"task"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(summary.Text, stateContextTestPreamble)), &document); err != nil {
		t.Fatal(err)
	}
	task := document.Providers.Task
	if len(task.Items) != 2 || task.Items[0].Status != native.TaskInProgress {
		t.Fatalf("listed tasks = %+v; want the two in-progress ones", task.Items)
	}
	if len(task.Counts) != 2 || task.Counts[0] != (native.TaskStateContextCount{Status: "in-progress", Count: 2}) || task.Counts[1] != (native.TaskStateContextCount{Status: "done", Count: 40}) {
		t.Fatalf("counts = %+v", task.Counts)
	}
}

func TestSummaryModeTrimsListedTasksBeforeCountsWhenOverBudget(t *testing.T) {
	config, store := summaryTaskConfig(t, sessionfs.NativeStateContextModeSummary, 1024*1024)
	full, _, err := collectStateContextSnapshot(config, stateContextSources{task: store}, "compaction")
	if err != nil {
		t.Fatal(err)
	}
	config.StateContext.Triggers["compaction"] = sessionfs.StateContextTrigger{Tools: []string{"task"}, MaxBytes: int64(len(full.Text)) - 100}
	trimmed, include, err := collectStateContextSnapshot(config, stateContextSources{task: store}, "compaction")
	if err != nil || !include {
		t.Fatalf("trimmed block: include %v, err %v", include, err)
	}
	if !strings.Contains(trimmed.Text, `"count": 40`) || !strings.Contains(trimmed.Text, `"count": 2`) {
		t.Fatalf("a count was dropped before the listed tasks:\n%s", trimmed.Text)
	}
	if strings.Count(trimmed.Text, `"title"`) >= 2 {
		t.Fatalf("no listed task was dropped:\n%s", trimmed.Text)
	}
}

func TestSummaryModeSkipsTheBlockWhenNoTaskMatches(t *testing.T) {
	config, _ := summaryTaskConfig(t, sessionfs.NativeStateContextModeSummary, 4096)
	empty := native.NewTaskStore(t.TempDir())
	if _, include, err := collectStateContextSnapshot(config, stateContextSources{task: empty}, "compaction"); err != nil || include {
		t.Fatalf("empty store: include %v, err %v; want no block", include, err)
	}
}

func TestSummaryModeCountsAloneAreData(t *testing.T) {
	config, store := summaryTaskConfig(t, sessionfs.NativeStateContextModeSummary, 1024*1024)
	config.NativeTask.StateContext.DetailStatuses = []string{"todo"}
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{task: store}, "compaction")
	if err != nil || !include {
		t.Fatalf("counts without listed tasks: include %v, err %v; want a block", include, err)
	}
	if strings.Contains(snapshot.Text, `"title"`) || !strings.Contains(snapshot.Text, `"count": 40`) {
		t.Fatalf("block:\n%s", snapshot.Text)
	}
}
