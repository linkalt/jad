package native

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionlock"
)

const (
	taskHelperDirEnv     = "JAD_TASKSTORE_HELPER_DIR"
	taskHelperStartedEnv = "JAD_TASKSTORE_HELPER_STARTED"
	taskHelperDoneEnv    = "JAD_TASKSTORE_HELPER_DONE"
	taskHelperAddsEnv    = "JAD_TASKSTORE_HELPER_ADDS"
)

// TestTaskStoreProcessHelper is the body of the child processes of the cross-process
// tests below (the sessionlock tests re-exec the test binary the same way). It does
// nothing in a normal run.
func TestTaskStoreProcessHelper(t *testing.T) {
	directory := os.Getenv(taskHelperDirEnv)
	if directory == "" {
		t.Skip("helper of the cross-process task store tests")
	}
	if started := os.Getenv(taskHelperStartedEnv); started != "" {
		if err := os.WriteFile(started, []byte("started"), 0o600); err != nil {
			t.Fatalf("write started marker: %v", err)
		}
	}
	var adds int
	if _, err := fmt.Sscan(os.Getenv(taskHelperAddsEnv), &adds); err != nil || adds < 1 {
		t.Fatalf("helper adds %q: %v", os.Getenv(taskHelperAddsEnv), err)
	}
	settings := taskTestSettings(directory)
	settings.MaxTasks = 100
	settings.MaxStorageBytes = 1 << 20
	store := NewTaskStore(directory)
	for index := 0; index < adds; index++ {
		input := TaskInput{Title: fmt.Sprintf("pid-%d-%d", os.Getpid(), index), Status: TaskTodo, Priority: TaskPriorityNormal, Tags: []string{}, Documents: []string{}}
		if _, err := store.Create(input, settings); err != nil {
			t.Fatalf("create %d: %v", index, err)
		}
	}
	if done := os.Getenv(taskHelperDoneEnv); done != "" {
		if err := os.WriteFile(done, []byte("done"), 0o600); err != nil {
			t.Fatalf("write done marker: %v", err)
		}
	}
}

// startTaskHelper starts a child process that adds adds tasks to the store in directory.
func startTaskHelper(t *testing.T, directory, started, done string, adds int) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTaskStoreProcessHelper$")
	cmd.Env = append(os.Environ(),
		taskHelperDirEnv+"="+directory,
		taskHelperStartedEnv+"="+started,
		taskHelperDoneEnv+"="+done,
		taskHelperAddsEnv+"="+fmt.Sprint(adds),
	)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start task helper: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd, &output
}

func waitForTaskMarker(t *testing.T, path string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat marker %q: %v", path, err)
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func storedTasks(t *testing.T, directory string) taskDocument {
	t.Helper()
	var document taskDocument
	if err := json.Unmarshal(taskFileBytes(t, directory), &document); err != nil {
		t.Fatalf("decode task storage: %v", err)
	}
	return document
}

func TestTaskStoreWaitsForTheLockOfAnotherProcess(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "tasks")
	lockPath, err := sessionlock.PathFor(filepath.Join(directory, taskStorageFileName))
	if err != nil {
		t.Fatal(err)
	}
	held, err := sessionlock.Acquire(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("acquire the task lock: %v", err)
	}
	defer held.Close()

	markers := t.TempDir()
	started, done := filepath.Join(markers, "started"), filepath.Join(markers, "done")
	cmd, output := startTaskHelper(t, directory, started, done, 1)
	if !waitForTaskMarker(t, started, 10*time.Second) {
		t.Fatalf("the child never started:\n%s", output)
	}
	if waitForTaskMarker(t, done, 300*time.Millisecond) {
		t.Fatal("the child added a task while this process held the task lock")
	}
	if _, err := os.Stat(filepath.Join(directory, taskStorageFileName)); !os.IsNotExist(err) {
		t.Fatalf("the child wrote tasks.json while the lock was held: %v", err)
	}

	if err := held.Close(); err != nil {
		t.Fatalf("release the task lock: %v", err)
	}
	if !waitForTaskMarker(t, done, 10*time.Second) {
		t.Fatalf("the child did not finish after the lock was released:\n%s", output)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("task helper failed: %v\n%s", err, output)
	}
	if document := storedTasks(t, directory); len(document.Tasks) != 1 || document.NextID != 2 {
		t.Fatalf("stored document after the child: %+v", document)
	}
}

func TestTaskStoreConcurrentProcessesLoseNoUpdate(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "tasks")
	const processes, adds = 4, 5
	type child struct {
		cmd    *exec.Cmd
		output *bytes.Buffer
	}
	children := make([]child, processes)
	for index := range children {
		cmd, output := startTaskHelper(t, directory, "", "", adds)
		children[index] = child{cmd, output}
	}
	for _, c := range children {
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("task helper failed: %v\n%s", err, c.output)
		}
	}
	document := storedTasks(t, directory)
	seen := map[string]struct{}{}
	for _, task := range document.Tasks {
		if _, duplicate := seen[task.ID]; duplicate {
			t.Fatalf("duplicate id %s", task.ID)
		}
		seen[task.ID] = struct{}{}
	}
	if want := processes * adds; len(document.Tasks) != want || document.NextID != int64(want+1) {
		t.Fatalf("stored %d tasks with next_id %d, want %d tasks (a lost update)", len(document.Tasks), document.NextID, want)
	}
}

func TestTaskStoreReadOfAnEmptyDirectoryCreatesNothing(t *testing.T) {
	directory := t.TempDir()
	store := NewTaskStore(directory)
	if listed, err := store.List(TaskListFilter{Limit: 5}); err != nil || listed.Total != 0 {
		t.Fatalf("list = %+v, error %v", listed, err)
	}
	if _, found, err := store.Get("T-001"); err != nil || found {
		t.Fatalf("get = found %v, error %v", found, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("a read of an existing empty directory left %v (error %v)", entries, err)
	}
}

func TestTaskStoreApplyOutcomeContract(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	store := NewTaskStore(settings.StorageDir)
	add := func(title string) TaskOp {
		return TaskOp{Action: taskActionAdd, Input: TaskInput{Title: title, Status: TaskTodo, Priority: TaskPriorityNormal, Tags: []string{}, Documents: []string{}}}
	}

	empty, err := store.Apply(nil, settings)
	if err != nil || len(empty.Results) != 0 || empty.Failed != -1 || empty.Miss != nil || empty.Wrote {
		t.Fatalf("Apply(nil) = %+v, error %v", empty, err)
	}

	// An op that misses stops the run: one result per op that ran, the miss at its index,
	// and nothing written, not even the add before it.
	missed, err := store.Apply([]TaskOp{add("first"), {Action: taskActionGet, ID: "T-099"}, add("never")}, settings)
	if err != nil || missed.Failed != 1 || len(missed.Results) != 1 || missed.Miss == nil || missed.Wrote {
		t.Fatalf("Apply with a miss = %+v, error %v", missed, err)
	}
	if _, err := os.Stat(filepath.Join(settings.StorageDir, taskStorageFileName)); !os.IsNotExist(err) {
		t.Fatalf("a batch with a miss wrote tasks.json: %v", err)
	}

	// The single-op wrappers report a miss as not found, never as a panic.
	if _, found, err := store.Get("T-099"); found || err != nil {
		t.Fatalf("Get miss = %v, %v", found, err)
	}
	title := "x"
	if _, found, err := store.Update("T-099", TaskPatch{Title: &title}, settings); found || err != nil {
		t.Fatalf("Update miss = %v, %v", found, err)
	}
	if found, err := store.Delete("T-099", settings); found || err != nil {
		t.Fatalf("Delete miss = %v, %v", found, err)
	}
	// A failed op (capacity) is an error with the failing index and no results for it.
	full := settings
	full.MaxTasks = 1
	failed, err := store.Apply([]TaskOp{add("one"), add("two")}, full)
	var storeError *TaskStoreError
	if !errors.As(err, &storeError) || failed.Failed != 1 || failed.Wrote {
		t.Fatalf("Apply over capacity = %+v, error %v", failed, err)
	}
}

func TestOneTaskResultTurnsABrokenOutcomeIntoAnError(t *testing.T) {
	one := TaskOpResult{Task: TaskRecord{ID: "T-001"}}
	for _, test := range []struct {
		name      string
		outcome   TaskApplyResult
		err       error
		wantFound bool
		wantError bool
	}{
		{"one result", TaskApplyResult{Results: []TaskOpResult{one}, Failed: -1}, nil, true, false},
		{"miss", TaskApplyResult{Failed: 0, Miss: &TaskMiss{}}, nil, false, false},
		{"error", TaskApplyResult{Failed: -1}, errors.New("boom"), false, true},
		{"no results and no failure", TaskApplyResult{Failed: -1}, nil, false, true},
		{"too many results", TaskApplyResult{Results: []TaskOpResult{one, one}, Failed: -1}, nil, false, true},
		{"failure without a miss", TaskApplyResult{Failed: 0}, nil, false, true},
	} {
		result, found, err := oneTaskResult(test.outcome, test.err)
		if found != test.wantFound || (err != nil) != test.wantError {
			t.Errorf("%s: found %v, error %v", test.name, found, err)
		}
		if found && result.Task.ID != "T-001" {
			t.Errorf("%s: result %+v", test.name, result)
		}
	}
}

func TestNativeTaskListNeverStalls(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	store := NewTaskStore(settings.StorageDir)
	seedTaskFile(t, settings.StorageDir, 5)
	// A line is 30 bytes: no line fits 10, so each page shows its first line anyway.
	settings.MaxListBytes = 10
	seen := []string{}
	offset := 0
	for page := 0; page < 10; page++ {
		output := runTaskTool(t, store, settings, page, map[string]any{"offset": offset})
		if taskInfoLine(output, "count") != "1" {
			t.Fatalf("page at offset %d:\n%s", offset, output)
		}
		seen = append(seen, strings.Fields(taskContent(output))[0])
		next := taskInfoLine(output, "next_offset")
		if next == "" {
			break
		}
		if next != fmt.Sprint(offset+1) {
			t.Fatalf("next_offset = %s at offset %d:\n%s", next, offset, output)
		}
		offset++
	}
	if strings.Join(seen, ",") != "T-001,T-002,T-003,T-004,T-005" {
		t.Fatalf("paged ids = %v", seen)
	}

	// In a batch the first list line of the call is shown, a later section gets what is
	// left of the shared budget; one that gets nothing says so and prints no next_offset,
	// which would equal its own offset and loop a client that reissues the batch.
	output := runTaskTool(t, store, settings, 20, map[string]any{"ops": []any{
		map[string]any{"action": "list", "offset": 3},
		map[string]any{"action": "list", "offset": 3},
	}})
	want := strings.Join([]string{
		"1 list 1 of 5, next_offset 4",
		"  T-004 | todo | normal | task-3",
		"2 list 0 of 5" + taskBatchOverBudget,
	}, "\n")
	if got := taskContent(output); got != want {
		t.Fatalf("batch lists:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(want, "2 list 0 of 5, over budget, ask again alone") || strings.Contains(strings.Split(want, "\n")[2], "next_offset") {
		t.Fatalf("the marker text changed: %q", want)
	}
	// Asked alone, that section is the first list of its call and moves on.
	alone := runTaskTool(t, store, settings, 21, map[string]any{"ops": []any{map[string]any{"action": "list", "offset": 3}}})
	if got := taskContent(alone); !strings.HasPrefix(got, "1 list 1 of 5, next_offset 4\n") {
		t.Fatalf("section asked alone:\n%s", got)
	}
}

func TestNativeTaskNullOpsIsASchemaError(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	store := NewTaskStore(settings.StorageDir)
	// The schema types ops as an array, like every other field: null is refused there,
	// not routed to the batch path and not read as an absent ops.
	for index, arguments := range []map[string]any{
		{"ops": nil},
		{"ops": nil, "title": "x"},
		{"ops": []any{}},
		{"ops": "x"},
	} {
		output := runTaskTool(t, store, settings, index, arguments)
		if !strings.Contains(output, "error_code: validation_failed") || !strings.Contains(output, "/ops") {
			t.Errorf("arguments %v:\n%s", arguments, output)
		}
	}
	// A valid ops next to another field passes the schema and is the batch refusal.
	output := runTaskTool(t, store, settings, 9, map[string]any{"ops": []any{map[string]any{"title": "a"}}, "title": "b"})
	if !strings.Contains(output, "error_code: invalid_batch") || taskInfoLine(output, "applied") != "0" {
		t.Errorf("ops with another field:\n%s", output)
	}
	if _, err := os.Stat(settings.StorageDir); !os.IsNotExist(err) {
		t.Errorf("a refused call created the storage: %v", err)
	}
}
