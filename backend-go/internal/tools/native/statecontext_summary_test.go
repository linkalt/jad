package native

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

func TestTaskStateContextSummaryCountsEveryStatusAndListsOnlyDetailStatuses(t *testing.T) {
	settings := taskTestSettings(t.TempDir())
	store := NewTaskStore(settings.StorageDir)
	settings.MaxTasks = 100
	for index, status := range []TaskStatus{TaskInProgress, TaskInProgress, TaskInProgress, TaskDone, TaskDone, TaskBlocked} {
		if _, err := store.Create(TaskInput{Title: fmt.Sprintf("task-%d", index), Status: status, Priority: TaskPriorityNormal, Tags: []string{}, Documents: []string{}}, settings); err != nil {
			t.Fatal(err)
		}
	}
	state, err := CollectTaskStateContext(store, sessionfs.NativeTaskStateContextSettings{
		Statuses: []string{"in-progress", "done", "blocked", "todo"}, Limit: 2,
		Mode: sessionfs.NativeStateContextModeSummary, DetailStatuses: []string{"in-progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCounts := []TaskStateContextCount{{"in-progress", 3}, {"done", 2}, {"blocked", 1}}
	if fmt.Sprint(state.Counts) != fmt.Sprint(wantCounts) {
		t.Fatalf("counts = %+v, want %+v (a status with no task is left out)", state.Counts, wantCounts)
	}
	if len(state.Items) != 2 || !state.HasMore {
		t.Fatalf("items = %d, has_more = %v; want 2 in-progress tasks and more", len(state.Items), state.HasMore)
	}
	for _, item := range state.Items {
		if item.Status != TaskInProgress {
			t.Fatalf("detail item has status %s", item.Status)
		}
	}
	if state.StateContextItemCount() != 5 {
		t.Fatalf("item count = %d, want 2 items + 3 counts", state.StateContextItemCount())
	}
	for state.StateContextItemCount() > 3 {
		state.TrimStateContextItem()
	}
	if len(state.Items) != 0 || len(state.Counts) != 3 {
		t.Fatalf("trim must drop the listed tasks before any count: %+v", state)
	}
	if !state.TrimStateContextItem() || len(state.Counts) != 2 || state.OmittedCounts != 1 {
		t.Fatalf("after trimming a count: %+v", state)
	}
}

func TestTaskStateContextItemsModeHasNoCounts(t *testing.T) {
	settings := taskTestSettings(t.TempDir())
	store := NewTaskStore(settings.StorageDir)
	if _, err := store.Create(TaskInput{Title: "a", Status: TaskDone, Priority: TaskPriorityNormal, Tags: []string{}, Documents: []string{}}, settings); err != nil {
		t.Fatal(err)
	}
	state, err := CollectTaskStateContext(store, sessionfs.NativeTaskStateContextSettings{Statuses: []string{"done"}, Limit: 5, Mode: sessionfs.NativeStateContextModeItems})
	if err != nil || len(state.Items) != 1 || state.Counts != nil {
		t.Fatalf("items mode = %+v, err %v; want one item and no counts", state, err)
	}
	if encoded, _ := json.Marshal(state); strings.Contains(string(encoded), "counts") {
		t.Fatalf("items mode changed its encoding: %s", encoded)
	}
}

func TestSubagentStateContextSummaryCountsEveryStateAndListsOnlyDetailStates(t *testing.T) {
	release := make(chan struct{})
	first, _, manager := sharedManagerCatalogs(t, blockedLauncher(release, nil))
	var ids []string
	for _, task := range []string{"one", "two", "three"} {
		ids = append(ids, runIDOf(t, subagentCallAs(t, first, "alpha", `{"task":"`+task+`","block_ms":2}`)))
		time.Sleep(3 * time.Millisecond)
	}
	settings := sessionfs.NativeSubagentStateContextSettings{
		States: []string{"queued", "running", "done"}, Limit: 1,
		Mode: sessionfs.NativeStateContextModeSummary, DetailStates: []string{"running"},
	}
	section, err := CollectSubagentStateContext(manager, "alpha", settings)
	if err != nil {
		t.Fatal(err)
	}
	want := []StateContextCount{{"queued", 1}, {"running", 2}}
	if fmt.Sprint(section.Counts) != fmt.Sprint(want) {
		t.Fatalf("counts = %+v, want %+v (queued and running first, done has none)", section.Counts, want)
	}
	if len(section.Items) != 1 || section.Items[0].State != "running" || section.Items[0].ID != ids[1] || !section.HasMore {
		t.Fatalf("items = %+v, has_more = %v; want the newest running run and more", section.Items, section.HasMore)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		done, _ := CollectSubagentStateContext(manager, "alpha", sessionfs.NativeSubagentStateContextSettings{States: []string{"done"}, Limit: 5})
		if len(done.Items) == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	finished, err := CollectSubagentStateContext(manager, "alpha", settings)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(finished.Counts) != fmt.Sprint([]StateContextCount{{"done", 3}}) || len(finished.Items) != 0 || finished.HasMore {
		t.Fatalf("after the runs ended: counts %+v, items %d; want only done=3 and nothing listed", finished.Counts, len(finished.Items))
	}
}

func TestSummaryStateContextCountsAloneAreData(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	first, _, manager := sharedManagerCatalogs(t, blockedLauncher(release, nil))
	subagentCallAs(t, first, "alpha", `{"task":"one","block_ms":2}`)
	subagent, err := CollectSubagentStateContext(manager, "alpha", sessionfs.NativeSubagentStateContextSettings{
		States: []string{"running"}, Limit: 5, Mode: sessionfs.NativeStateContextModeSummary, DetailStates: nil,
	})
	if err != nil || len(subagent.Items) != 0 || subagent.StateContextItemCount() != 1 {
		t.Fatalf("subagent counts only: %+v, count %d, err %v; want one entry that counts as data", subagent, subagent.StateContextItemCount(), err)
	}
}
