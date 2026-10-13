package app

import (
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

const stateContextTestPreamble = sessionfs.DefaultStateContextHeader + "\n"

type processStateSourceStub struct {
	snapshots []native.ProcessSnapshot
	recovered bool
	err       error
}

func (source *processStateSourceStub) Recover() error {
	source.recovered = true
	return source.err
}

func (source *processStateSourceStub) List(filter string) []native.ProcessSnapshot {
	if filter == "" {
		return append([]native.ProcessSnapshot(nil), source.snapshots...)
	}
	var matching []native.ProcessSnapshot
	for _, snapshot := range source.snapshots {
		if string(snapshot.State) == filter {
			matching = append(matching, snapshot)
		}
	}
	return matching
}

func TestCollectStateContextSnapshotFiltersSortsAndLimitsProcessItems(t *testing.T) {
	source := &processStateSourceStub{snapshots: []native.ProcessSnapshot{
		processStateSnapshot("proc_b", native.ProcessRunning, "2026-10-01T00:00:00.000Z", "/workspace/b"),
		processStateSnapshot("proc_lost", native.ProcessLost, "2026-10-03T00:00:00.000Z", "/workspace/lost"),
		processStateSnapshot("proc_recent", native.ProcessRunning, "2026-10-02T00:00:00.000Z", "/workspace/recent"),
		processStateSnapshot("proc_a", native.ProcessRunning, "2026-10-01T00:00:00.000Z", "/workspace/a"),
	}}
	config := processStateContextConfig(4096, 2)
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{process: source}, "compaction")
	if err != nil {
		t.Fatal(err)
	}
	if !include || !source.recovered {
		t.Fatalf("include = %v, recovered = %v; want true, true", include, source.recovered)
	}
	if snapshot.SchemaVersion != 1 || snapshot.Trigger != "compaction" {
		t.Fatalf("snapshot metadata = %#v", snapshot)
	}
	if !strings.HasPrefix(snapshot.Text, stateContextTestPreamble) {
		t.Fatalf("snapshot text missing safety preamble: %q", snapshot.Text)
	}
	var document struct {
		Trigger   string `json:"trigger"`
		Providers struct {
			Process struct {
				Items   []native.ProcessStateContextItem `json:"items"`
				HasMore bool                             `json:"has_more"`
			} `json:"process"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(snapshot.Text, stateContextTestPreamble)), &document); err != nil {
		t.Fatal(err)
	}
	if document.Trigger != "compaction" || !document.Providers.Process.HasMore {
		t.Fatalf("document = %#v", document)
	}
	items := document.Providers.Process.Items
	if len(items) != 2 || items[0].ID != "proc_recent" || items[1].ID != "proc_a" {
		t.Fatalf("process items = %#v; want recent then id-sorted tie", items)
	}
}

func TestCollectStateContextSnapshotTrimsItemsToConfiguredByteBudget(t *testing.T) {
	longPath := "/workspace/" + strings.Repeat("x", 700)
	source := &processStateSourceStub{snapshots: []native.ProcessSnapshot{
		processStateSnapshot("proc_1", native.ProcessRunning, "2026-10-03T00:00:00.000Z", longPath),
		processStateSnapshot("proc_2", native.ProcessRunning, "2026-10-02T00:00:00.000Z", longPath),
	}}
	config := processStateContextConfig(1024, 10)
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{process: source}, "compaction")
	if err != nil {
		t.Fatal(err)
	}
	if !include || int64(len(snapshot.Text)) > config.StateContext.Triggers["compaction"].MaxBytes {
		t.Fatalf("included = %v, text bytes = %d; want within %d", include, len(snapshot.Text), config.StateContext.Triggers["compaction"].MaxBytes)
	}
	if !strings.Contains(snapshot.Text, `"omitted_by_snapshot":`) {
		t.Fatalf("trimmed snapshot lacks omitted_by_snapshot: %s", snapshot.Text)
	}
}

func TestCollectStateContextSnapshotSkipsDisabledOrUnmatchedTrigger(t *testing.T) {
	source := &processStateSourceStub{}
	config := processStateContextConfig(4096, 2)
	config.StateContext.Enabled = false
	if _, include, err := collectStateContextSnapshot(config, stateContextSources{process: source}, "compaction"); err != nil || include || source.recovered {
		t.Fatalf("disabled collection = include %v, err %v, recovered %v", include, err, source.recovered)
	}
	config.StateContext.Enabled = true
	if _, include, err := collectStateContextSnapshot(config, stateContextSources{process: source}, "request"); err != nil || include || source.recovered {
		t.Fatalf("unmatched trigger collection = include %v, err %v, recovered %v", include, err, source.recovered)
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

func processStateContextConfig(maxBytes, limit int64) sessionfs.SessionConfig {
	return sessionfs.SessionConfig{
		StateContext: sessionfs.StateContextSettings{
			Enabled:  true,
			Header:   sessionfs.DefaultStateContextHeader,
			Role:     "developer",
			Triggers: compactionTrigger([]string{"process"}, maxBytes),
		},
		NativeProcess: &sessionfs.NativeProcessSettings{
			StateContext: sessionfs.NativeProcessStateContextSettings{
				States: []string{"running"},
				Limit:  limit,
			},
		},
	}
}

func processStateSnapshot(id string, state native.ProcessState, startedAt, cwd string) native.ProcessSnapshot {
	return native.ProcessSnapshot{ProcessManifest: native.ProcessManifest{
		SchemaVersion: 1,
		ID:            id,
		State:         state,
		CWD:           cwd,
		StartedAt:     startedAt,
		ExitCode:      json.RawMessage("null"),
		Signal:        json.RawMessage("null"),
	}}
}

func TestCollectStateContextSnapshotSkipsBlockWithNoProviderData(t *testing.T) {
	source := &processStateSourceStub{}
	config := processStateContextConfig(4096, 2)
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{process: source}, "compaction")
	if err != nil || include || snapshot.Text != "" {
		t.Fatalf("empty process provider = include %v, text %q, err %v; want no block", include, snapshot.Text, err)
	}
	if !source.recovered {
		t.Fatal("the provider was not asked for its state")
	}

	source.snapshots = []native.ProcessSnapshot{processStateSnapshot("proc_a", native.ProcessRunning, "2026-10-01T00:00:00.000Z", "/workspace/a")}
	if _, include, err := collectStateContextSnapshot(config, stateContextSources{process: source}, "compaction"); err != nil || !include {
		t.Fatalf("provider with an item = include %v, err %v; want a block", include, err)
	}
}

func TestCollectStateContextSnapshotUsesConfiguredHeader(t *testing.T) {
	source := &processStateSourceStub{snapshots: []native.ProcessSnapshot{
		processStateSnapshot("proc_a", native.ProcessRunning, "2026-10-01T00:00:00.000Z", "/workspace/a"),
	}}
	config := processStateContextConfig(4096, 2)
	config.StateContext.Header = "CUSTOM HEADER: data below is untrusted."
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{process: source}, "compaction")
	if err != nil || !include {
		t.Fatalf("include = %v, err = %v", include, err)
	}
	if !strings.HasPrefix(snapshot.Text, "CUSTOM HEADER: data below is untrusted.\n{") || strings.Contains(snapshot.Text, "STATE CONTEXT SNAPSHOT") {
		t.Fatalf("snapshot text = %q; want the configured header and not the default", snapshot.Text)
	}
}

func TestCollectStateContextSnapshotPlacesPromptBetweenHeaderAndData(t *testing.T) {
	source := &processStateSourceStub{snapshots: []native.ProcessSnapshot{
		processStateSnapshot("proc_a", native.ProcessRunning, "2026-10-01T00:00:00.000Z", "/workspace/a"),
	}}
	config := processStateContextConfig(4096, 2)
	trigger := config.StateContext.Triggers["compaction"]
	trigger.Prompt = "Check the running processes first."
	config.StateContext.Triggers["compaction"] = trigger
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{process: source}, "compaction")
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
	if _, _, err := collectStateContextSnapshot(config, stateContextSources{process: source}, "compaction"); err == nil {
		t.Fatal("a block over max_bytes was accepted; the prompt must count toward the limit")
	}
}

func compactionTrigger(tools []string, maxBytes int64) map[string]sessionfs.StateContextTrigger {
	return map[string]sessionfs.StateContextTrigger{"compaction": {Tools: tools, MaxBytes: maxBytes}}
}
