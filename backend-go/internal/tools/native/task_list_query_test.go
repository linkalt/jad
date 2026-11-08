package native

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestNativeTaskListOffsetTotalAndNextOffset(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	store := NewTaskStore(settings.StorageDir)
	seedTaskFile(t, settings.StorageDir, 7)

	first := runTaskTool(t, store, settings, 1, map[string]any{"limit": 3})
	for key, want := range map[string]string{"action": "list", "count": "3", "total": "7", "has_more": "true", "next_offset": "3"} {
		if got := taskInfoLine(first, key); got != want {
			t.Errorf("first page %s = %q, want %q:\n%s", key, got, want, first)
		}
	}
	// The information lines come in this order.
	if !strings.Contains(first, "action: list\ncount: 3\ntotal: 7\nhas_more: true\nnext_offset: 3\n") {
		t.Errorf("information order:\n%s", first)
	}
	second := runTaskTool(t, store, settings, 2, map[string]any{"limit": 3, "offset": 3})
	if taskContent(second) != "T-004 | todo | normal | task-3\nT-005 | todo | normal | task-4\nT-006 | todo | normal | task-5" || taskInfoLine(second, "next_offset") != "6" {
		t.Errorf("second page:\n%s", second)
	}
	last := runTaskTool(t, store, settings, 3, map[string]any{"limit": 3, "offset": 6})
	if taskInfoLine(last, "count") != "1" || taskInfoLine(last, "total") != "7" || strings.Contains(last, "has_more") || strings.Contains(last, "next_offset") {
		t.Errorf("last page:\n%s", last)
	}
	past := runTaskTool(t, store, settings, 4, map[string]any{"offset": 9})
	if !strings.Contains(past, `"summary":"0 tasks listed"`) || taskInfoLine(past, "total") != "7" || strings.Contains(past, "has_more") || strings.Contains(past, "--- CONTENT") {
		t.Errorf("offset past total:\n%s", past)
	}
	// Normalization clamps a negative offset to 0, like a limit below 1.
	if output := runTaskTool(t, store, settings, 5, map[string]any{"action": "list", "offset": -4}); taskInfoLine(output, "count") != "7" {
		t.Errorf("negative offset is clamped to 0:\n%s", output)
	}
	// Execute itself refuses what normalization did not fix, such as an op of a batch the schema leaves loose.
	for _, bad := range []map[string]any{{"action": "list", "offset": "x"}, {"action": "list", "q": 4}, {"action": "list", "detail": "yes"}, {"action": "list", "offset": 1.5}} {
		output := runTaskTool(t, store, settings, 6, map[string]any{"ops": []any{bad}})
		if !strings.Contains(output, "op_error_code: invalid_list") || !strings.Contains(output, "failed_op: 1") {
			t.Errorf("list op %v:\n%s", bad, output)
		}
	}
}

func TestNativeTaskListByteBudgetSetsNextOffset(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	store := NewTaskStore(settings.StorageDir)
	seedTaskFile(t, settings.StorageDir, 7)
	// A line is 30 bytes: two lines and the newline between them fit 61, a third does not.
	settings.MaxListBytes = 70
	output := runTaskTool(t, store, settings, 1, map[string]any{"limit": 5, "offset": 1})
	if taskInfoLine(output, "count") != "2" || taskInfoLine(output, "total") != "7" || taskInfoLine(output, "has_more") != "true" || taskInfoLine(output, "next_offset") != "3" {
		t.Errorf("byte-truncated page:\n%s", output)
	}
	if !strings.Contains(taskContent(output), "T-002") || strings.Contains(taskContent(output), "T-004") {
		t.Errorf("byte-truncated lines:\n%s", output)
	}
	// The window ends at the end of the matches, but the bytes cut the page: more remain.
	output = runTaskTool(t, store, settings, 2, map[string]any{"limit": 3, "offset": 4})
	if taskInfoLine(output, "count") != "2" || taskInfoLine(output, "next_offset") != "6" {
		t.Errorf("byte cut inside the window:\n%s", output)
	}
}

func TestNativeTaskListQueryMatchesTitleTagsAndDocuments(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	store := NewTaskStore(settings.StorageDir)
	for _, input := range []TaskInput{
		{Title: "Design the Store", Tags: []string{"backend"}, Documents: []string{"design_task.md"}},
		{Title: "Write tests", Tags: []string{"backend", "Alpha"}, Documents: []string{"docs/Plan.md"}},
		{Title: "Polish UI", Tags: []string{"frontend"}, Documents: []string{}},
	} {
		input.Status, input.Priority = TaskTodo, TaskPriorityNormal
		if _, err := store.Create(input, settings); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(arguments map[string]any) string {
		output := runTaskTool(t, store, settings, 1, arguments)
		var found []string
		for _, line := range strings.Split(taskContent(output), "\n") {
			if line != "" {
				found = append(found, strings.SplitN(line, " |", 2)[0])
			}
		}
		sort.Strings(found)
		return strings.Join(found, ",")
	}
	for _, test := range []struct {
		query string
		want  string
	}{
		{"design", "T-001"},           // title, any case
		{"STORE", "T-001"},            // upper-case query
		{"frontend", "T-003"},         // tag
		{"alpha", "T-002"},            // tag, stored lower-case
		{"plan.md", "T-002"},          // document path, any case
		{"design_task", "T-001"},      // document path
		{"backend plan", "T-002"},     // every word, in different fields
		{"design plan", ""},           // words found in different tasks do not combine
		{"  Tests   WRITE ", "T-002"}, // whitespace and case
		{"nothing-matches-this", ""},  // no match
		{"", "T-001,T-002,T-003"},     // empty query is no filter
	} {
		if got := ids(map[string]any{"q": test.query}); got != test.want {
			t.Errorf("q %q = %q, want %q", test.query, got, test.want)
		}
	}
	listed, err := store.List(TaskListFilter{Status: "all", Query: "backend", Limit: 1, Offset: 1})
	if err != nil || listed.Total != 2 || len(listed.Tasks) != 1 || listed.HasMore {
		t.Fatalf("query with offset: %+v, error %v", listed, err)
	}
}

func TestNativeTaskListDetailNamesDocumentsAndUpdateTime(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	store := NewTaskStore(settings.StorageDir)
	if err := os.MkdirAll(settings.StorageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	document := `{"schema_version":1,"next_id":3,"tasks":[` +
		`{"id":"T-001","title":"linked","status":"todo","priority":"normal","tags":["t"],"documents":["a.md","docs/b.md"],"created_at":"2026-01-01T00:00:00.000Z","updated_at":"2026-05-01T10:00:00.000Z"},` +
		`{"id":"T-002","title":"plain","status":"todo","priority":"low","tags":[],"documents":[],"created_at":"2026-01-01T00:00:00.000Z","updated_at":"2026-05-01T09:00:00.000Z"}]}`
	if err := os.WriteFile(filepath.Join(settings.StorageDir, taskStorageFileName), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	plain := runTaskTool(t, store, settings, 1, map[string]any{})
	if want := "T-001 | todo | normal | linked | tags: t | documents: 2\nT-002 | todo | low | plain"; taskContent(plain) != want {
		t.Errorf("plain list:\n%s", plain)
	}
	detailed := runTaskTool(t, store, settings, 2, map[string]any{"detail": true})
	want := "T-001 | todo | normal | linked | tags: t | documents: a.md,docs/b.md | updated: 2026-05-01T10:00:00.000Z\n" +
		"T-002 | todo | low | plain | updated: 2026-05-01T09:00:00.000Z"
	if taskContent(detailed) != want {
		t.Errorf("detailed list:\n%s\nwant:\n%s", taskContent(detailed), want)
	}
	batch := runTaskTool(t, store, settings, 3, map[string]any{"ops": []any{map[string]any{"action": "list", "detail": true, "limit": 1}}})
	if !strings.Contains(taskContent(batch), "  T-001 | todo | normal | linked | tags: t | documents: a.md,docs/b.md | updated: 2026-05-01T10:00:00.000Z") {
		t.Errorf("detailed batch list:\n%s", batch)
	}
}

func TestNativeTaskLenientIDs(t *testing.T) {
	for input, want := range map[string]string{
		"1": "T-001", "T-1": "T-001", "t1": "T-001", "t-1": "T-001", " 7 ": "T-007", "T-001": "T-001", "t-001": "T-001",
		"12": "T-012", "T-1000": "T-1000", "1000": "T-1000", "T-0004": "T-004",
		"0": "0", "T-0": "T-0", "T-": "T-", "abc": "ABC", "T-1a": "T-1A", "-1": "-1", "": "", "T--1": "T--1", "1.5": "1.5",
		"99999999999999999999": "99999999999999999999",
	} {
		if got := normalizeTaskID(input); got != want {
			t.Errorf("normalizeTaskID(%q) = %q, want %q", input, got, want)
		}
		if again := normalizeTaskID(normalizeTaskID(input)); again != normalizeTaskID(input) {
			t.Errorf("normalizeTaskID(%q) is not idempotent: %q then %q", input, normalizeTaskID(input), again)
		}
	}
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	store := NewTaskStore(settings.StorageDir)
	seedTaskFile(t, settings.StorageDir, 2)
	for _, id := range []string{"2", "T-2", "t2", "T-002"} {
		if output := runTaskTool(t, store, settings, 1, map[string]any{"id": id}); taskIDFromOutput(t, output) != "T-002" {
			t.Errorf("get id %q:\n%s", id, output)
		}
	}
	for _, id := range []string{"0", "T-0", "x", "T-x", "-1"} {
		if output := runTaskTool(t, store, settings, 2, map[string]any{"action": "get", "id": id}); !strings.Contains(output, "error_code: invalid_task_id") {
			t.Errorf("get id %q should be invalid_task_id:\n%s", id, output)
		}
	}
}

func TestTaskStoreMissAndDoneOnDoneWriteNothing(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	store := NewTaskStore(settings.StorageDir)
	title := "x"
	// An empty store: a miss must not create tasks.json.
	if _, found, err := store.Update("T-009", TaskPatch{Title: &title}, settings); err != nil || found {
		t.Fatalf("update on an empty store = found %v, error %v", found, err)
	}
	if deleted, err := store.Delete("T-009", settings); err != nil || deleted {
		t.Fatalf("delete on an empty store = %v, error %v", deleted, err)
	}
	if _, err := os.Stat(filepath.Join(settings.StorageDir, taskStorageFileName)); !os.IsNotExist(err) {
		t.Fatalf("a miss created the task file: %v", err)
	}
	if _, err := store.Create(TaskInput{Title: "finish me", Status: TaskTodo, Priority: TaskPriorityNormal, Tags: []string{}, Documents: []string{}}, settings); err != nil {
		t.Fatal(err)
	}
	if output := runTaskTool(t, store, settings, 1, map[string]any{"action": "done", "id": "T-001"}); !strings.Contains(output, "status: done") {
		t.Fatalf("done:\n%s", output)
	}
	before := taskFileBytes(t, settings.StorageDir)
	info, _ := os.Stat(filepath.Join(settings.StorageDir, taskStorageFileName))

	// A second done, a miss by update and by delete, and a miss when the byte cap would fail
	// a write: none writes, so none bumps updated_at or fails on the cap.
	tight := settings
	tight.MaxStorageBytes = 1
	if output := runTaskTool(t, store, tight, 2, map[string]any{"action": "done", "id": "T-001"}); !strings.Contains(output, "status: done") || strings.Contains(output, "task_storage_error") {
		t.Fatalf("done on done:\n%s", output)
	}
	if output := runTaskTool(t, store, tight, 3, map[string]any{"id": "T-009", "title": "ghost"}); !strings.Contains(output, "error_code: task_not_found") {
		t.Fatalf("update miss:\n%s", output)
	}
	if output := runTaskTool(t, store, tight, 4, map[string]any{"action": "delete", "id": "T-009"}); !strings.Contains(output, "error_code: task_not_found") {
		t.Fatalf("delete miss:\n%s", output)
	}
	after := taskFileBytes(t, settings.StorageDir)
	if !bytes.Equal(before, after) {
		t.Fatalf("no-op calls rewrote the file:\n%s\n%s", before, after)
	}
	if infoAfter, _ := os.Stat(filepath.Join(settings.StorageDir, taskStorageFileName)); !infoAfter.ModTime().Equal(info.ModTime()) {
		t.Fatal("no-op calls replaced the file")
	}
}

func TestNativeTaskNotFoundOpenIDsAreCappedAndMarked(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	settings.ListLimit = 2
	store := NewTaskStore(settings.StorageDir)
	if err := os.MkdirAll(settings.StorageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var tasks []string
	for index, status := range []string{"done", "todo", "blocked", "in-progress", "done", "todo"} {
		tasks = append(tasks, fmt.Sprintf(`{"id":"T-%03d","title":"t","status":"%s","priority":"normal","tags":[],"documents":[],"created_at":"2026-01-01T00:00:00.000Z","updated_at":"2026-01-01T00:00:00.000Z"}`, index+1, status))
	}
	document := fmt.Sprintf(`{"schema_version":1,"next_id":7,"tasks":[%s]}`, strings.Join(tasks, ","))
	if err := os.WriteFile(filepath.Join(settings.StorageDir, taskStorageFileName), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	// Open tasks in list order: blocked T-003, in-progress T-004, then todo T-002, T-006.
	output := runTaskTool(t, store, settings, 1, map[string]any{"id": "T-099"})
	if got := taskInfoLine(output, "open_ids"); got != "T-003, T-004 (+2 more)" {
		t.Errorf("open_ids = %q:\n%s", got, output)
	}
	settings.ListLimit = 10
	output = runTaskTool(t, store, settings, 2, map[string]any{"id": "T-099"})
	if got := taskInfoLine(output, "open_ids"); got != "T-003, T-004, T-002, T-006" {
		t.Errorf("open_ids = %q:\n%s", got, output)
	}
}

func TestTaskStoreConcurrentApplyAcrossStores(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	settings.MaxTasks = 500
	settings.MaxStorageBytes = 1 << 20
	// Two stores on one directory have separate mutexes: only the lock file keeps their
	// load-modify-write cycles apart, as it does for two processes.
	stores := []*TaskStore{NewTaskStore(settings.StorageDir), NewTaskStore(settings.StorageDir)}
	const workers, batches, perBatch = 8, 4, 3
	var wait sync.WaitGroup
	ids := make(chan string, workers*batches*perBatch)
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			store := stores[worker%len(stores)]
			for batch := 0; batch < batches; batch++ {
				ops := make([]TaskOp, perBatch)
				for index := range ops {
					ops[index] = TaskOp{Action: taskActionAdd, Input: TaskInput{Title: fmt.Sprintf("w%d-b%d-%d", worker, batch, index), Status: TaskTodo, Priority: TaskPriorityNormal, Tags: []string{}, Documents: []string{}}}
				}
				outcome, err := store.Apply(ops, settings)
				if err != nil {
					t.Errorf("Apply: %v", err)
					return
				}
				for _, result := range outcome.Results {
					ids <- result.Task.ID
				}
				if _, err := store.List(TaskListFilter{Status: "all", Limit: 5}); err != nil {
					t.Errorf("List: %v", err)
				}
			}
		}(worker)
	}
	wait.Wait()
	close(ids)
	seen := map[string]struct{}{}
	for id := range ids {
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = struct{}{}
	}
	want := workers * batches * perBatch
	var document taskDocument
	if err := json.Unmarshal(taskFileBytes(t, settings.StorageDir), &document); err != nil {
		t.Fatal(err)
	}
	if len(seen) != want || len(document.Tasks) != want || document.NextID != int64(want+1) {
		t.Fatalf("ids %d, stored tasks %d, next_id %d, want %d tasks", len(seen), len(document.Tasks), document.NextID, want)
	}
	if _, err := os.Stat(filepath.Join(settings.StorageDir, ".tasks.json.jad-lock")); err != nil {
		t.Fatalf("the lock file beside tasks.json: %v", err)
	}
}

func TestTaskStoreReadOfMissingDirectoryCreatesNothing(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "never")
	store := NewTaskStore(directory)
	if listed, err := store.List(TaskListFilter{Limit: 5}); err != nil || listed.Total != 0 {
		t.Fatalf("list = %+v, error %v", listed, err)
	}
	if _, found, err := store.Get("T-001"); err != nil || found {
		t.Fatalf("get = found %v, error %v", found, err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("a read created the storage directory: %v", err)
	}
}

func TestTaskStateContextItemsModeOutputIsUnchanged(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	store := NewTaskStore(settings.StorageDir)
	if err := os.MkdirAll(settings.StorageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	document := `{"schema_version":1,"next_id":5,"tasks":[` +
		`{"id":"T-001","title":"not listed","status":"todo","priority":"urgent","tags":[],"documents":[],"created_at":"2026-01-01T00:00:00.000Z","updated_at":"2026-01-01T00:00:00.000Z"},` +
		`{"id":"T-002","title":"first","status":"in-progress","priority":"high","tags":["x"],"documents":["d.md"],"created_at":"2026-01-01T00:00:00.000Z","updated_at":"2026-01-02T00:00:00.000Z"},` +
		`{"id":"T-003","title":"finished","status":"done","priority":"normal","tags":[],"documents":[],"created_at":"2026-01-01T00:00:00.000Z","updated_at":"2026-01-03T00:00:00.000Z"},` +
		`{"id":"T-004","title":"second","status":"in-progress","priority":"normal","tags":[],"documents":[],"created_at":"2026-01-01T00:00:00.000Z","updated_at":"2026-01-04T00:00:00.000Z"}]}`
	if err := os.WriteFile(filepath.Join(settings.StorageDir, taskStorageFileName), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := CollectTaskStateContext(store, sessionfs.NativeTaskStateContextSettings{Statuses: []string{"in-progress", "done"}, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"items":[` +
		`{"id":"T-002","title":"first","status":"in-progress","priority":"high","tags":["x"],"updated_at":"2026-01-02T00:00:00.000Z"},` +
		`{"id":"T-004","title":"second","status":"in-progress","priority":"normal","tags":[],"updated_at":"2026-01-04T00:00:00.000Z"}],"has_more":true}`
	if string(encoded) != want {
		t.Fatalf("items-mode statecontext changed:\n got: %s\nwant: %s", encoded, want)
	}
}
