package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

func TestCodeContextCallWithoutArgumentsListsTasks(t *testing.T) {
	var storage string
	f := newCodeContextFixture(t, func(settings *sessionfs.NativeCodeContextSettings) { storage = settings.StorageDir })
	if got := f.call(`{}`); got != "code_context: no tasks yet; read a range with a task to start one\n" {
		t.Fatalf("an empty storage is not an empty task list:\n%s", got)
	}
	f.write("t.go", numbered("t", 4))
	f.call(`{"action":"read","task":"T-1","task_description":"Make the glob cap configurable.","path":"t.go","start":1,"end":2,"priority":1,"description":"cap"}`)
	f.call(`{"action":"read","task":"T-2","path":"t.go","start":3,"end":4}`)
	// T-1 is written last, so it is listed first.
	now := time.Now()
	if err := os.Chtimes(filepath.Join(storage, "T-2.json"), now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{`{}`, `{"action":"list"}`, `{"action":" LIST "}`} {
		got := f.call(arguments)
		if !strings.HasPrefix(got, "code_context: 2 tasks, most recently written first; list one with task\nT-1: 1 entries, generation 1, by executor, ") ||
			!strings.Contains(got, "Z\n  Make the glob cap configurable.\nT-2: 1 entries, generation 1, by executor, ") {
			t.Fatalf("%s does not list both tasks, newest first:\n%s", arguments, got)
		}
	}
	// A task without an action is that task's list.
	if got := f.call(`{"task":"T-1"}`); got != "code_context task T-1, generation 1: 1 entries\ntask description: Make the glob cap configurable.\n#1 p1 t.go:1-2  cap\n" {
		t.Fatalf("a call with only task is not the task's list:\n%s", got)
	}

	// An unreadable task file is named, not skipped; max_scan_tasks bounds the list.
	if err := os.WriteFile(filepath.Join(storage, "T-3.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.call(`{}`); !strings.Contains(got, "\nT-3: error: code_context storage is not valid JSON\n") {
		t.Fatalf("an invalid task file is not reported:\n%s", got)
	}
	small := newCodeContextFixture(t, func(settings *sessionfs.NativeCodeContextSettings) {
		settings.StorageDir, settings.MaxScanTasks = storage, 1
	})
	if got := small.call(`{}`); !strings.HasPrefix(got, "code_context: the 1 most recently written of 3 tasks (max_scan_tasks)") || strings.Count(got, "\nT-") != 1 {
		t.Fatalf("max_scan_tasks did not bound the task list:\n%s", got)
	}
}

func TestCodeContextTaskFilesSkipForeignFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"T-1.json", "T-2.json", "notes.txt", ".hidden.json", "T-3.json.tmp-1"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A state directory an earlier version left is not a task.
	if err := os.MkdirAll(filepath.Join(dir, ".state"), 0o755); err != nil {
		t.Fatal(err)
	}
	files, total, err := codeContextStore{dir: dir}.tasks(10)
	if err != nil || total != 2 || len(files) != 2 || files[0].task == files[1].task {
		t.Fatalf("tasks = %+v, %d, %v; want T-1 and T-2", files, total, err)
	}
	if files, total, _ := (codeContextStore{dir: dir}).tasks(1); len(files) != 1 || total != 2 {
		t.Fatalf("tasks limit = %+v, %d", files, total)
	}
}

func TestCodeContextTaskDescription(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("d.go", numbered("d", 6))
	// update with only task_description sets it; it may start a task.
	if got := f.call(`{"action":"update","task":"T-5","task_description":"  Fix the loader.\nKeep the schema.  "}`); got != "code_context task T-5, generation 1: task description set\n" {
		t.Fatalf("setting the task description answered:\n%s", got)
	}
	// With items it is set in the same write, and every item still runs.
	got := f.call(`{"action":"read","task":"T-5","task_description":"Fix the loader.\nKeep the schema, too.","path":"d.go","priority":2,"items":[{"start":1,"end":2,"description":"a"},{"start":3,"end":4,"description":"b"}]}`)
	if !strings.HasPrefix(got, "code_context task T-5, generation 2: task description set; 2 of 2 items done\n") {
		t.Fatalf("task_description with items answered:\n%s", got)
	}
	if got := f.call(`{"action":"get","task":"T-5"}`); !strings.HasPrefix(got, "code_context task T-5, generation 2: 2 entries shown\ntask description: Fix the loader.\n  Keep the schema, too.\n\n=== d.go lines 1-4 ===") {
		t.Fatalf("get does not show the task description:\n%s", got)
	}
	for _, arguments := range []string{
		`{"action":"get","task_description":"x","task":"T-5"}`,
		`{"action":"get"}`,
		`{"action":"update","task":"T-5"}`,
		`{"action":"update","task":"T-5","task_description":""}`,
	} {
		if got := f.call(arguments); !strings.Contains(got, `"status":"invalid_input"`) {
			t.Fatalf("%s was not refused:\n%s", arguments, got)
		}
	}
	long := newCodeContextFixture(t, func(settings *sessionfs.NativeCodeContextSettings) { settings.MaxDescriptionChars = 5 })
	if got := long.call(`{"action":"update","task":"T-5","task_description":"too long"}`); !strings.Contains(got, "task_description is empty or longer than 5 characters") {
		t.Fatalf("max_description_chars was not applied to task_description:\n%s", got)
	}
}
