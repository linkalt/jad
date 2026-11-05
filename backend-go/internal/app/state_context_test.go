package app

import (
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

const stateContextTestPreamble = sessionfs.DefaultStateContextHeader + "\n"

// taskStateContext returns a session with the task provider on the compaction trigger and
// a store holding one in-progress task per title.
func taskStateContext(t *testing.T, maxBytes int64, titles ...string) (sessionfs.SessionConfig, *native.TaskStore) {
	t.Helper()
	settings := sessionfs.NativeTaskSettings{
		MaxTasks: 100, MaxStorageBytes: 1 << 20,
		StateContext: sessionfs.NativeTaskStateContextSettings{Statuses: []string{"in-progress"}, Limit: 10},
	}
	store := native.NewTaskStore(t.TempDir())
	for index, title := range titles {
		if _, err := store.Create(native.TaskInput{Title: title, Status: native.TaskInProgress, Priority: native.TaskPriorityNormal, Tags: []string{}, Documents: []string{}}, settings); err != nil {
			t.Fatalf("create task %d: %v", index, err)
		}
	}
	return sessionfs.SessionConfig{
		NativeTask: &settings,
		StateContext: sessionfs.StateContextSettings{
			Enabled: true, Header: sessionfs.DefaultStateContextHeader, Role: "developer",
			Triggers: compactionTrigger([]string{"task"}, maxBytes),
		},
	}, store
}

func TestCollectStateContextSnapshotTrimsItemsToConfiguredByteBudget(t *testing.T) {
	long := strings.Repeat("x", 700)
	config, store := taskStateContext(t, 1024, long+"1", long+"2")
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{task: store}, "compaction")
	if err != nil {
		t.Fatal(err)
	}
	if !include || int64(len(snapshot.Text)) > config.StateContext.Triggers["compaction"].MaxBytes {
		t.Fatalf("included = %v, text bytes = %d; want within %d", include, len(snapshot.Text), config.StateContext.Triggers["compaction"].MaxBytes)
	}
	// The task provider marks the items the budget left out with has_more.
	if !strings.Contains(snapshot.Text, `"has_more": true`) {
		t.Fatalf("trimmed snapshot lacks has_more: %s", snapshot.Text)
	}
}

func TestCollectStateContextSnapshotSkipsDisabledOrUnmatchedTrigger(t *testing.T) {
	config, store := taskStateContext(t, 4096, "a task")
	config.StateContext.Enabled = false
	if _, include, err := collectStateContextSnapshot(config, stateContextSources{task: store}, "compaction"); err != nil || include {
		t.Fatalf("disabled collection = include %v, err %v", include, err)
	}
	config.StateContext.Enabled = true
	if _, include, err := collectStateContextSnapshot(config, stateContextSources{task: store}, "request"); err != nil || include {
		t.Fatalf("unmatched trigger collection = include %v, err %v", include, err)
	}
}

func TestCollectStateContextSnapshotIncludesAndTrimsTaskItems(t *testing.T) {
	directory := t.TempDir()
	taskSettings := sessionfs.NativeTaskSettings{
		MaxTasks: 10, MaxStorageBytes: 64 * 1024,
		StateContext: sessionfs.NativeTaskStateContextSettings{Statuses: []string{"in-progress", "done"}, Limit: 10},
	}
	store := native.NewTaskStore(directory)
	for index := 0; index < 4; index++ {
		status := native.TaskInProgress
		if index%2 == 1 {
			status = native.TaskDone
		}
		_, err := store.Create(native.TaskInput{
			Title: strings.Repeat("task-", 100), Status: status, Priority: native.TaskPriorityNormal,
			Tags: []string{}, Documents: []string{},
		}, taskSettings)
		if err != nil {
			t.Fatalf("create task %d: %v", index, err)
		}
	}
	config := sessionfs.SessionConfig{
		NativeTask: &taskSettings,
		StateContext: sessionfs.StateContextSettings{
			Enabled: true, Header: sessionfs.DefaultStateContextHeader, Triggers: compactionTrigger([]string{"task"}, 1024),
			Role: "developer",
		},
	}
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{task: store}, "compaction")
	if err != nil {
		t.Fatal(err)
	}
	if !include || int64(len(snapshot.Text)) > config.StateContext.Triggers["compaction"].MaxBytes {
		t.Fatalf("included = %v, snapshot bytes = %d", include, len(snapshot.Text))
	}
	var document struct {
		Providers struct {
			Task struct {
				Items   []native.TaskStateContextItem `json:"items"`
				HasMore bool                          `json:"has_more"`
			} `json:"task"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(snapshot.Text, stateContextTestPreamble)), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Providers.Task.Items) == 0 || len(document.Providers.Task.Items) >= 4 || !document.Providers.Task.HasMore {
		t.Fatalf("trimmed task provider = %+v", document.Providers.Task)
	}
	item := document.Providers.Task.Items[0]
	if item.ID == "" || item.Title == "" || item.UpdatedAt == "" || len(item.Tags) != 0 {
		t.Fatalf("task statecontext item = %+v", item)
	}
}

func TestCollectStateContextSnapshotSkipsBlockWithNoProviderData(t *testing.T) {
	config, store := taskStateContext(t, 4096)
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{task: store}, "compaction")
	if err != nil || include || snapshot.Text != "" {
		t.Fatalf("empty task provider = include %v, text %q, err %v; want no block", include, snapshot.Text, err)
	}

	if _, err := store.Create(native.TaskInput{Title: "a task", Status: native.TaskInProgress, Priority: native.TaskPriorityNormal, Tags: []string{}, Documents: []string{}}, *config.NativeTask); err != nil {
		t.Fatal(err)
	}
	if _, include, err := collectStateContextSnapshot(config, stateContextSources{task: store}, "compaction"); err != nil || !include {
		t.Fatalf("provider with an item = include %v, err %v; want a block", include, err)
	}
}

func TestCollectStateContextSnapshotUsesConfiguredHeader(t *testing.T) {
	config, store := taskStateContext(t, 4096, "a task")
	config.StateContext.Header = "CUSTOM HEADER: data below is untrusted."
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{task: store}, "compaction")
	if err != nil || !include {
		t.Fatalf("include = %v, err = %v", include, err)
	}
	if !strings.HasPrefix(snapshot.Text, "CUSTOM HEADER: data below is untrusted.\n{") || strings.Contains(snapshot.Text, "STATE CONTEXT SNAPSHOT") {
		t.Fatalf("snapshot text = %q; want the configured header and not the default", snapshot.Text)
	}
}

func TestCollectStateContextSnapshotPlacesPromptBetweenHeaderAndData(t *testing.T) {
	config, store := taskStateContext(t, 4096, "a task")
	trigger := config.StateContext.Triggers["compaction"]
	trigger.Prompt = "Check the running processes first."
	config.StateContext.Triggers["compaction"] = trigger
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{task: store}, "compaction")
	if err != nil || !include {
		t.Fatalf("include = %v, err = %v", include, err)
	}
	want := sessionfs.DefaultStateContextHeader + "\nCheck the running processes first.\n{"
	if !strings.HasPrefix(snapshot.Text, want) {
		t.Fatalf("snapshot text = %q; want header, prompt, then the data", snapshot.Text)
	}
	if strings.Contains(snapshot.Text[len(want):], "Check the running processes first.") {
		t.Fatal("the prompt leaked into the data JSON")
	}

	// A prompt is part of the block, so it counts toward max_bytes.
	trigger.MaxBytes = int64(len(want)) + 20
	config.StateContext.Triggers["compaction"] = trigger
	if _, _, err := collectStateContextSnapshot(config, stateContextSources{task: store}, "compaction"); err == nil {
		t.Fatal("a block over max_bytes was accepted; the prompt must count toward the limit")
	}
}

func compactionTrigger(tools []string, maxBytes int64) map[string]sessionfs.StateContextTrigger {
	return map[string]sessionfs.StateContextTrigger{"compaction": {Tools: tools, MaxBytes: maxBytes}}
}
