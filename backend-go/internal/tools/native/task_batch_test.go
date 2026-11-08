package native

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"jad/backend-go/internal/tools"
)

// taskInfoLine is the value of the INFORMATION line key of a tool result, or "".
func taskInfoLine(output, key string) string {
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(line, key+": "); ok {
			return value
		}
	}
	return ""
}

// taskContent is the text of the CONTENT block of a tool result, or "".
func taskContent(output string) string {
	match := regexp.MustCompile(`(?s)--- CONTENT [^\n]*\n(.*)\n--- END CONTENT`).FindStringSubmatch(output)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

// seedTaskFile writes count todo/normal tasks T-001.. titled task-0.. that share one update
// time, so the list order among them is by id and does not depend on the clock.
func seedTaskFile(t *testing.T, directory string, count int) {
	t.Helper()
	tasks := make([]string, 0, count)
	for index := 0; index < count; index++ {
		tasks = append(tasks, fmt.Sprintf(`{"id":"T-%03d","title":"task-%d","status":"todo","priority":"normal","tags":[],"documents":[],"created_at":"2026-01-01T00:00:00.000Z","updated_at":"2026-01-01T00:00:00.000Z"}`, index+1, index))
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	document := fmt.Sprintf(`{"schema_version":1,"next_id":%d,"tasks":[%s]}`, count+1, strings.Join(tasks, ","))
	if err := os.WriteFile(filepath.Join(directory, taskStorageFileName), []byte(document+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func taskFileBytes(t *testing.T, directory string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, taskStorageFileName))
	if err != nil {
		t.Fatalf("read task storage: %v", err)
	}
	return data
}

func TestNativeTaskBatchAppliesOpsInOrderAgainstOneDocument(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	store := NewTaskStore(settings.StorageDir)
	for _, title := range []string{"one", "two", "three"} {
		if _, err := store.Create(TaskInput{Title: title, Status: TaskTodo, Priority: TaskPriorityNormal, Tags: []string{"x"}, Documents: []string{}}, settings); err != nil {
			t.Fatal(err)
		}
	}
	output := runTaskTool(t, store, settings, 1, map[string]any{"ops": []any{
		map[string]any{"title": "four", "tags": []any{"y"}},
		map[string]any{"id": "T-001", "status": "IN PROGRESS", "priority": "high"},
		map[string]any{"action": "COMPLETE", "taskId": "2"},
		map[string]any{"action": "delete", "id": "t3"},
		map[string]any{"id": "T-001"},
		map[string]any{"action": "list"},
		map[string]any{"id": "T-004", "title": "four, renamed"},
	}})
	if !strings.Contains(output, `"summary":"7 ops applied"`) || taskInfoLine(output, "action") != "batch" || taskInfoLine(output, "count") != "7" {
		t.Fatalf("batch envelope:\n%s", output)
	}
	want := strings.Join([]string{
		"1 add T-004 | todo | normal",
		"2 update T-001 | in-progress | high",
		"3 done T-002",
		"4 delete T-003",
		"5 get T-001 | in-progress | high | one | tags: x",
		"6 list 2 of 2",
		"  T-001 | in-progress | high | one | tags: x",
		"  T-004 | todo | normal | four | tags: y",
		"7 update T-004 | todo | normal",
	}, "\n")
	if got := taskContent(output); got != want {
		t.Fatalf("batch content:\n%s\nwant:\n%s", got, want)
	}
	var document taskDocument
	if err := json.Unmarshal(taskFileBytes(t, settings.StorageDir), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Tasks) != 3 || document.NextID != 5 {
		t.Fatalf("stored document = %+v", document)
	}
	byID := map[string]TaskRecord{}
	for _, task := range document.Tasks {
		byID[task.ID] = task
	}
	if byID["T-001"].Status != TaskInProgress || byID["T-002"].Status != TaskDone || byID["T-004"].Title != "four, renamed" {
		t.Fatalf("stored tasks = %+v", byID)
	}
	if _, deleted := byID["T-003"]; deleted {
		t.Fatal("T-003 was not deleted")
	}
}

func TestNativeTaskBatchOfReadsIsRunNotApplied(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	store := NewTaskStore(settings.StorageDir)
	if _, err := store.Create(TaskInput{Title: "done already", Status: TaskDone, Priority: TaskPriorityNormal, Tags: []string{}, Documents: []string{}}, settings); err != nil {
		t.Fatal(err)
	}
	before := taskFileBytes(t, settings.StorageDir)
	output := runTaskTool(t, store, settings, 1, map[string]any{"ops": []any{
		map[string]any{"action": "get", "id": "T-001"},
		map[string]any{"action": "done", "id": "T-001"},
		map[string]any{"status": "all"},
	}})
	if !strings.Contains(output, `"summary":"3 ops run"`) {
		t.Fatalf("a batch that changes nothing:\n%s", output)
	}
	if !bytes.Equal(before, taskFileBytes(t, settings.StorageDir)) {
		t.Fatal("a batch that changes nothing rewrote the file")
	}
}

func TestNativeTaskBatchFailureWritesNothing(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	settings.MaxTasks = 4
	store := NewTaskStore(settings.StorageDir)
	seedTaskFile(t, settings.StorageDir, 3)
	before := taskFileBytes(t, settings.StorageDir)
	cases := []struct {
		name         string
		ops          []any
		status       string
		failedOp     string
		opErrorCode  string
		extraContain []string
	}{
		{"missing id", []any{
			map[string]any{"title": "four"},
			map[string]any{"id": "T-001", "status": "done"},
			map[string]any{"action": "done", "id": "T-009"},
		}, "not_found", "3", "task_not_found", []string{"task_id: T-009", "open_ids: T-001, T-002, T-003\n"}},
		{"invalid op", []any{
			map[string]any{"id": "T-001", "status": "done"},
			map[string]any{"title": "four", "status": "blocked"},
		}, "invalid_input", "2", "invalid_creation_status", nil},
		{"invalid id", []any{
			map[string]any{"action": "get", "id": "T-xyz"},
		}, "invalid_input", "1", "invalid_task_id", nil},
		{"capacity", []any{
			map[string]any{"title": "four"},
			map[string]any{"title": "five"},
		}, "error", "2", "task_storage_error", nil},
	}
	for index, test := range cases {
		output := runTaskTool(t, store, settings, 10+index, map[string]any{"ops": test.ops})
		for _, want := range append([]string{
			`"status":"` + test.status + `"`,
			fmt.Sprintf(`"summary":"Op %s failed: nothing was applied"`, test.failedOp),
			"error_type: execution", "error_code: batch_failed", "failed_op: " + test.failedOp,
			"op_error_code: " + test.opErrorCode, "op_error: ", "applied: 0",
		}, test.extraContain...) {
			if !strings.Contains(output, want) {
				t.Errorf("%s: output lacks %q:\n%s", test.name, want, output)
			}
		}
		if strings.Contains(output, "--- CONTENT") {
			t.Errorf("%s: a failed batch has content:\n%s", test.name, output)
		}
		if !bytes.Equal(before, taskFileBytes(t, settings.StorageDir)) {
			t.Fatalf("%s: a failed batch changed the stored file", test.name)
		}
	}
	entries, err := os.ReadDir(settings.StorageDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestNativeTaskBatchRefusals(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	settings.MaxBatchOps = 2
	store := NewTaskStore(settings.StorageDir)
	op := map[string]any{"title": "x"}

	output := runTaskTool(t, store, settings, 1, map[string]any{"ops": []any{op, op, op}})
	if !strings.Contains(output, "error_code: batch_too_large") || !strings.Contains(output, "A batch holds at most 2 ops") || !strings.Contains(output, "applied: 0") || strings.Contains(output, "failed_op") {
		t.Errorf("oversized batch:\n%s", output)
	}
	for index, extra := range []map[string]any{{"title": "y"}, {"action": "add"}, {"limit": 3}} {
		arguments := map[string]any{"ops": []any{op}}
		for key, value := range extra {
			arguments[key] = value
		}
		output = runTaskTool(t, store, settings, 2+index, arguments)
		if !strings.Contains(output, "error_code: invalid_batch") || !strings.Contains(output, "ops must be a list of 1 to 2 task objects and cannot be combined with other fields") || strings.Contains(output, "failed_op") {
			t.Errorf("ops with %v:\n%s", extra, output)
		}
	}
	// Execute refuses what the schema would also refuse, for a caller that skips the schema.
	for index, ops := range [][]any{{}, {"not an object"}} {
		output, err := executeNativeTask(context.Background(), map[string]any{"ops": ops}, tools.ExecutionContext{CallID: "direct-invalid-batch-" + fmt.Sprint(index)}, settings, store)
		if err != nil || !strings.Contains(output, "error_code: invalid_batch") {
			t.Errorf("direct ops %v = %q, error %v", ops, output, err)
		}
	}
	if output := runTaskTool(t, store, settings, 9, map[string]any{"ops": []any{}}); !strings.Contains(output, "error_type: schema") {
		t.Errorf("empty ops through the schema:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(settings.StorageDir, taskStorageFileName)); !os.IsNotExist(err) {
		t.Errorf("a refused batch created the task file: %v", err)
	}
}

func TestNativeTaskBatchListSectionsShareTheByteBudget(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	store := NewTaskStore(settings.StorageDir)
	seedTaskFile(t, settings.StorageDir, 6)
	// "  T-001 | todo | normal | task-0" is 32 bytes and a newline: three lines fit 100.
	settings.MaxListBytes = 100
	output := runTaskTool(t, store, settings, 1, map[string]any{"ops": []any{
		map[string]any{"title": "added before the lists is not cut", "priority": "low"},
		map[string]any{"action": "list", "limit": 2},
		map[string]any{"action": "list", "offset": 2, "limit": 4},
		map[string]any{"action": "list", "offset": 20},
	}})
	want := strings.Join([]string{
		"1 add T-007 | todo | low",
		"2 list 2 of 7, next_offset 2",
		"  T-001 | todo | normal | task-0",
		"  T-002 | todo | normal | task-1",
		"3 list 1 of 7, next_offset 3",
		"  T-003 | todo | normal | task-2",
		"4 list 0 of 7",
	}, "\n")
	if got := taskContent(output); got != want {
		t.Fatalf("batch lists:\n%s\nwant:\n%s", got, want)
	}
}
