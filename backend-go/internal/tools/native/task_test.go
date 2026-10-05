package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

func taskTestSettings(directory string) sessionfs.NativeTaskSettings {
	return sessionfs.NativeTaskSettings{
		MaxTasks: 20, ListLimit: 10, MaxListLimit: 20, MaxTitleChars: 100,
		MaxTags: 4, MaxTagChars: 20, MaxDocuments: 4, MaxDocumentChars: 100,
		MaxStorageBytes: 10_000, MaxListBytes: 1_000, StorageDir: directory,
		StateContext: sessionfs.NativeTaskStateContextSettings{Statuses: []string{"in-progress", "done"}, Limit: 25},
	}
}

func runTaskTool(t *testing.T, store *TaskStore, settings sessionfs.NativeTaskSettings, sequence int, arguments map[string]any) string {
	t.Helper()
	tool, err := NewTaskTool(settings, store)
	if err != nil {
		t.Fatalf("NewTaskTool: %v", err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	encodedArguments, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	call, err := json.Marshal(map[string]any{
		"call_id":   fmt.Sprintf("task-call-%d", sequence),
		"name":      "task",
		"arguments": string(encodedArguments),
	})
	if err != nil {
		t.Fatalf("marshal call: %v", err)
	}
	output, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{})
	if err != nil {
		t.Fatalf("ExecuteFunctionCall: %v", err)
	}
	var response struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	return response.Output
}

func taskIDFromOutput(t *testing.T, output string) string {
	t.Helper()
	match := regexp.MustCompile(`(?m)^task_id: (T-[0-9]+)$`).FindStringSubmatch(output)
	if len(match) != 2 {
		t.Fatalf("task id missing from output:\n%s", output)
	}
	return match[1]
}

func TestNativeTaskCRUDNormalizationAndPersistence(t *testing.T) {
	directory := t.TempDir()
	settings := taskTestSettings(filepath.Join(directory, "tasks"))
	store := NewTaskStore(settings.StorageDir)
	firstOutput := runTaskTool(t, store, settings, 1, map[string]any{
		"title": "  Draft task design  ", "tags": []any{"Tools", "tools"}, "documents": []any{"design_task.md", "design_task.md"},
	})
	firstID := taskIDFromOutput(t, firstOutput)
	if firstID != "T-001" || !strings.Contains(firstOutput, "status: todo") || !strings.Contains(firstOutput, `tags: ["tools"]`) || !strings.Contains(firstOutput, `documents: ["design_task.md"]`) {
		t.Fatalf("normalized task add output:\n%s", firstOutput)
	}

	activeOutput := runTaskTool(t, store, settings, 2, map[string]any{"title": "Implement it", "status": "IN PROGRESS", "priority": "HIGH"})
	activeID := taskIDFromOutput(t, activeOutput)
	changed := runTaskTool(t, store, settings, 3, map[string]any{"id": activeID, "status": "blocked", "documents": []any{"design_task.md", "docs/plan.md"}})
	if !strings.Contains(changed, "status: blocked") || !strings.Contains(changed, `documents: ["design_task.md","docs/plan.md"]`) {
		t.Fatalf("task update output:\n%s", changed)
	}
	listed := runTaskTool(t, store, settings, 4, map[string]any{})
	if !strings.Contains(listed, firstID) || !strings.Contains(listed, activeID) || !strings.Contains(listed, "documents: 2") {
		t.Fatalf("task list output:\n%s", listed)
	}
	completed := runTaskTool(t, store, settings, 5, map[string]any{"action": "COMPLETE", "taskId": " " + firstID + " "})
	if !strings.Contains(completed, "status: done") {
		t.Fatalf("task completion output:\n%s", completed)
	}
	done := runTaskTool(t, store, settings, 6, map[string]any{"status": "done"})
	if !strings.Contains(done, firstID) {
		t.Fatalf("done task list output:\n%s", done)
	}
	fetched := runTaskTool(t, store, settings, 7, map[string]any{"id": activeID})
	if !strings.Contains(fetched, "docs/plan.md") {
		t.Fatalf("task get output:\n%s", fetched)
	}
	stored, err := os.ReadFile(filepath.Join(settings.StorageDir, taskStorageFileName))
	if err != nil {
		t.Fatalf("read task storage: %v", err)
	}
	var document taskDocument
	if err := json.Unmarshal(stored, &document); err != nil || len(document.Tasks) != 2 || document.Tasks[0].ID != firstID || document.Tasks[1].ID != activeID {
		t.Fatalf("persisted tasks = %+v, error = %v", document, err)
	}
}

func TestNativeTaskActionErrorsLimitsAndCorruptStorage(t *testing.T) {
	directory := t.TempDir()
	settings := taskTestSettings(filepath.Join(directory, "tasks"))
	settings.MaxTasks = 1
	settings.MaxListBytes = 20
	store := NewTaskStore(settings.StorageDir)
	if output := runTaskTool(t, store, settings, 1, map[string]any{"title": "Cannot start blocked", "status": "blocked"}); !strings.Contains(output, "error_code: invalid_creation_status") {
		t.Fatalf("blocked creation output:\n%s", output)
	}
	added := runTaskTool(t, store, settings, 2, map[string]any{"title": "One task", "documents": []any{"a.md"}})
	id := taskIDFromOutput(t, added)
	if output := runTaskTool(t, store, settings, 3, map[string]any{"title": "Too many"}); !strings.Contains(output, "error_code: task_storage_error") {
		t.Fatalf("capacity error output:\n%s", output)
	}
	list := runTaskTool(t, store, settings, 4, map[string]any{"status": "all"})
	if !strings.Contains(list, "has_more: true") || strings.Contains(list, "--- CONTENT") {
		t.Fatalf("bounded task list output:\n%s", list)
	}
	if output := runTaskTool(t, store, settings, 5, map[string]any{"id": id, "documents": []any{"a.md", "b.md", "c.md", "d.md", "e.md"}}); !strings.Contains(output, "error_code: invalid_documents") {
		t.Fatalf("document limit output:\n%s", output)
	}
	missingID, err := executeNativeTask(context.Background(), map[string]any{"action": "delete"}, tools.ExecutionContext{CallID: "direct-missing-id"}, settings, store)
	if err != nil || !strings.Contains(missingID, "error_code: missing_task_id") {
		t.Fatalf("direct missing id output:\n%s; error = %v", missingID, err)
	}
	if output := runTaskTool(t, store, settings, 7, map[string]any{"id": id, "action": "delete"}); !strings.Contains(output, "Task deleted") {
		t.Fatalf("delete output:\n%s", output)
	}
	if output := runTaskTool(t, store, settings, 8, map[string]any{"id": id}); !strings.Contains(output, "error_code: task_not_found") {
		t.Fatalf("missing task output:\n%s", output)
	}

	storagePath := filepath.Join(settings.StorageDir, taskStorageFileName)
	if err := os.WriteFile(storagePath, []byte("not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt storage: %v", err)
	}
	if output := runTaskTool(t, store, settings, 9, map[string]any{"title": "Unsafe overwrite"}); !strings.Contains(output, "error_code: task_storage_error") {
		t.Fatalf("corrupt storage output:\n%s", output)
	}
	corrupt, err := os.ReadFile(storagePath)
	if err != nil || string(corrupt) != "not-json" {
		t.Fatalf("corrupt storage changed to %q, error = %v", corrupt, err)
	}
}

func TestNativeTaskSchemaAndArgumentAliases(t *testing.T) {
	settings := taskTestSettings(t.TempDir())
	store := NewTaskStore(settings.StorageDir)
	invalid := runTaskTool(t, store, settings, 1, map[string]any{"action": "add"})
	if !strings.Contains(invalid, "error_type: schema") || !strings.Contains(invalid, "ajv_errors:") {
		t.Fatalf("missing add title should fail schema validation:\n%s", invalid)
	}
	added := runTaskTool(t, store, settings, 2, map[string]any{"title": "New task"})
	if taskIDFromOutput(t, added) != "T-001" {
		t.Fatalf("inferred add output:\n%s", added)
	}
	if normalized := normalizeTaskArguments(map[string]any{"action": "COMPLETE", "taskId": " t-001 "}); normalized["action"] != "done" || normalized["id"] != "T-001" {
		t.Fatalf("normalized aliases = %#v", normalized)
	}
	if normalized := normalizeTaskArguments(map[string]any{"status": "IN PROGRESS"}); normalized["status"] != "in-progress" || normalized["action"] != "list" {
		t.Fatalf("normalized status = %#v", normalized)
	}
}

func TestNativeTaskStoreStateContextAndConcurrentIDs(t *testing.T) {
	settings := taskTestSettings(t.TempDir())
	store := NewTaskStore(settings.StorageDir)
	for index, status := range []TaskStatus{TaskTodo, TaskInProgress, TaskDone, TaskBlocked} {
		_, err := store.Create(TaskInput{Title: fmt.Sprintf("task-%d", index), Status: status, Priority: TaskPriorityNormal, Tags: []string{}, Documents: []string{}}, settings)
		if err != nil {
			t.Fatalf("create task %d: %v", index, err)
		}
	}
	state, err := CollectTaskStateContext(store, sessionfs.NativeTaskStateContextSettings{Statuses: []string{"in-progress", "done"}, Limit: 1})
	if err != nil || len(state.Items) != 1 || !state.HasMore || (state.Items[0].Status != TaskDone && state.Items[0].Status != TaskInProgress) {
		t.Fatalf("task statecontext = %+v, error = %v", state, err)
	}
	if !state.TrimStateContextItem() || len(state.Items) != 0 || !state.HasMore {
		t.Fatalf("trimmed task statecontext = %+v", state)
	}

	settings.MaxTasks = 100
	var wait sync.WaitGroup
	ids := make(chan string, 20)
	for index := 0; index < 20; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			task, err := store.Create(TaskInput{Title: fmt.Sprintf("parallel-%d", index), Status: TaskTodo, Priority: TaskPriorityNormal, Tags: []string{}, Documents: []string{}}, settings)
			if err != nil {
				t.Errorf("parallel create: %v", err)
				return
			}
			ids <- task.ID
		}(index)
	}
	wait.Wait()
	close(ids)
	seen := make(map[string]struct{}, 20)
	for id := range ids {
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate task id %s", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != 20 {
		t.Fatalf("created %d parallel tasks, want 20", len(seen))
	}
}

func BenchmarkTaskStoreList100(b *testing.B) {
	settings := taskTestSettings(filepath.Join(b.TempDir(), "tasks"))
	settings.MaxTasks = 100
	settings.MaxStorageBytes = 64 * 1024
	store := NewTaskStore(settings.StorageDir)
	for index := 0; index < 100; index++ {
		_, err := store.Create(TaskInput{
			Title: fmt.Sprintf("task-%03d", index), Status: TaskTodo, Priority: TaskPriorityNormal,
			Tags: []string{}, Documents: []string{},
		}, settings)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := store.List(TaskListFilter{Status: "all", Limit: 100}); err != nil {
			b.Fatal(err)
		}
	}
}
