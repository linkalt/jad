package native

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/tools"
)

func TestWriteAndEditReportOnlyRealChanges(t *testing.T) {
	root := scopeWorkspace(t)
	write, _ := NewWriteTool(testWriteSettings(root, 1<<20), NewPathLocks())
	edit, _ := NewEditTool(testEditSettings(root, 1<<20), NewPathLocks())
	type change struct{ path, action string }
	var seen []change
	run := func(tool tools.Tool, arguments map[string]any) {
		t.Helper()
		_, err := tool.Execute(context.Background(), arguments, tools.ExecutionContext{
			CallID: "c", WorkspaceRoot: root,
			OnFileChange: func(path, action string) { seen = append(seen, change{path, action}) },
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	run(write, map[string]any{"path": "src/a.txt", "content": "one"})                      // created
	run(write, map[string]any{"path": "src/a.txt", "content": "one"})                      // unchanged
	run(write, map[string]any{"path": "src/a.txt", "content": "two"})                      // updated
	run(edit, map[string]any{"path": "src/a.txt", "oldText": "two", "newText": "three"})   // edited
	run(edit, map[string]any{"path": "src/a.txt", "oldText": "three", "newText": "three"}) // unchanged
	run(edit, map[string]any{"path": "src/a.txt", "oldText": "absent", "newText": "x"})    // no match
	run(edit, map[string]any{"path": "src/missing.txt", "oldText": "a", "newText": "b"})   // not found
	want := []change{{"src/a.txt", "created"}, {"src/a.txt", "updated"}, {"src/a.txt", "edited"}}
	if len(seen) < len(want) {
		t.Fatalf("reported %v, want at least %v", seen, want)
	}
	for i, w := range want {
		if seen[i] != w {
			t.Errorf("report %d = %v, want %v (all: %v)", i, seen[i], w, seen)
		}
	}
	for _, c := range seen[len(want):] {
		t.Errorf("a call that changed nothing was reported: %v", c)
	}
}

func TestWriteWithoutAHookDoesNotPanic(t *testing.T) {
	root := scopeWorkspace(t)
	write, _ := NewWriteTool(testWriteSettings(root, 1<<20), NewPathLocks())
	assertMutationStatus(t, scopeRun(t, write, root, nil, map[string]any{"path": "src/n.txt", "content": "x"}), "ok")
}

// changesLauncher makes the child "write" the given files through the request's hook, the
// way the real write and edit tools do.
func changesLauncher(files ...string) *fakeLauncher {
	return &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		for _, file := range files {
			request.Execution.OnFileChange(file, "created")
		}
		return SubagentResult{Text: "done", Rounds: 2, Tokens: 10}, nil
	}}
}

func TestSubagentResultListsTheFilesAWriterChanged(t *testing.T) {
	settings := writeSettings(writerAgent("fixer", "read", "write"))
	settings.ChangedFilesMax = 100
	catalog, _ := buildSubagentCatalog(t, settings, changesLauncher("src/a.go", "src/b.go", "src/a.go"), readOnlyTool("read"), writeTool("write"))
	out := subagentCall(t, catalog, `{"task":"fix"}`)
	if !strings.Contains(out, "written_files: src/a.go (created), src/b.go (created)") {
		t.Fatalf("the finished result does not list the files once each, in order:\n%s", out)
	}
	if strings.Contains(out, "written_files_omitted") {
		t.Fatalf("nothing was left out:\n%s", out)
	}
}

func TestSubagentResultSaysWhenAWriterChangedNoFile(t *testing.T) {
	settings := writeSettings(writerAgent("fixer", "read", "write"))
	settings.ChangedFilesMax = 100
	catalog, _ := buildSubagentCatalog(t, settings, changesLauncher(), readOnlyTool("read"), writeTool("write"))
	out := subagentCall(t, catalog, `{"task":"fix"}`)
	if !strings.Contains(out, "written_files: none (changes made by bash, exec or process are not listed)") {
		t.Fatalf("an empty list must say so, because bash, exec and process are not covered:\n%s", out)
	}
}

func TestSubagentChangedFilesAreCappedAndTheRestCounted(t *testing.T) {
	settings := writeSettings(writerAgent("fixer", "read", "write"))
	settings.ChangedFilesMax = 2
	catalog, _ := buildSubagentCatalog(t, settings, changesLauncher("a", "b", "c", "d", "a"), readOnlyTool("read"), writeTool("write"))
	out := subagentCall(t, catalog, `{"task":"fix"}`)
	if !strings.Contains(out, "written_files: a (created), b (created)\n") || !strings.Contains(out, "written_files_omitted: 2") {
		t.Fatalf("two kept, two counted (a repeat of a kept file is not a new file):\n%s", out)
	}
}

func TestSubagentChangedFilesOffAndReadersSayNothing(t *testing.T) {
	off := writeSettings(writerAgent("fixer", "read", "write"))
	off.ChangedFilesMax = 0
	catalog, _ := buildSubagentCatalog(t, off, changesLauncher(), readOnlyTool("read"), writeTool("write"))
	if out := subagentCall(t, catalog, `{"task":"fix"}`); strings.Contains(out, "written_files") {
		t.Fatalf("changed_files_max 0 still printed a list:\n%s", out)
	}
	reader := writeSettings(agentNamed("reader"))
	reader.ChangedFilesMax = 100
	launcher := &fakeLauncher{}
	catalog, _ = buildSubagentCatalog(t, reader, launcher, readOnlyTool("read"), writeTool("write"))
	if out := subagentCall(t, catalog, `{"task":"look"}`); strings.Contains(out, "written_files") {
		t.Fatalf("a read-only agent printed a file list:\n%s", out)
	}
	if launcher.requests[0].Execution.OnFileChange != nil {
		t.Fatal("a read-only child got a change hook")
	}
}

func TestSubagentCutOffWriterStillListsWhatItChanged(t *testing.T) {
	settings := writeSettings(writerAgent("fixer", "read", "write"))
	settings.ChangedFilesMax = 100
	settings.Agents[0].TimeoutMS = 1000
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		request.Execution.OnFileChange("src/half.go", "created")
		<-ctx.Done()
		return SubagentResult{Text: "partway", Rounds: 1}, ctx.Err()
	}}
	catalog, _ := buildSubagentCatalog(t, settings, launcher, readOnlyTool("read"), writeTool("write"))
	out := subagentCall(t, catalog, `{"task":"fix"}`)
	for _, want := range []string{"subagent_timeout", "some changes may already be made", "written_files: src/half.go (created)"} {
		if !strings.Contains(out, want) {
			t.Errorf("timeout result lacks %q:\n%s", want, out)
		}
	}
}

func TestSubagentChangedFilesSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	reported := make(chan struct{})
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		request.Execution.OnFileChange("src/kept.go", "edited")
		request.Execution.OnFileChange("src/more.go", "created")
		close(reported)
		<-ctx.Done()
		return SubagentResult{}, ctx.Err()
	}}
	first, manager := persistedWriterCatalog(t, dir, launcher)
	// A writer is never handed off, so its call blocks; the test lets it run in the
	// background and reads the manifest the way a restart would find it.
	callContext, stopCall := context.WithCancel(context.Background())
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_9", "name": "subagent", "arguments": `{"task":"fix"}`})
		_, _ = first.ExecuteFunctionCall(callContext, call, tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: "owner"})
	}()
	defer func() { stopCall(); <-callDone }()
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("the child never ran")
	}
	var id string
	for deadline := time.Now().Add(5 * time.Second); id == "" && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if views := manager.list("owner"); len(views) == 1 {
			id = views[0].id
		}
	}
	if id == "" {
		t.Fatal("no run was registered")
	}
	manifest := manifestOf(t, dir, id)
	if manifest.State != subagentRunRunning || len(manifest.Files) != 2 || manifest.Files[0].Path != "src/kept.go" || !manifest.Tracked {
		t.Fatalf("manifest = %+v; the changed files must be on disk while the run is still going", manifest)
	}

	// The process dies here: the first manager is not closed, so the manifest still
	// says running. A new manager over the same directory is the restart.
	_, second := persistedWriterCatalog(t, dir, &fakeLauncher{})
	out := restoredOutput(t, second, "owner", id)
	if !strings.Contains(out, "subagent_lost") || !strings.Contains(out, "written_files: src/kept.go (edited), src/more.go (created)") {
		t.Fatalf("a lost writer must still say which files it had changed:\n%s", out)
	}
}

func persistedWriterCatalog(t *testing.T, dir string, launcher SubagentLauncher) (*tools.Catalog, *SubagentManager) {
	t.Helper()
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: dir, Keep: 10})
	t.Cleanup(func() { _ = manager.Close() })
	settings := writeSettings(writerAgent("explore", "read", "write"))
	settings.StorageDir = dir
	settings.PersistRuns = true
	settings.ChangedFilesMax = 100
	parent := withExecutors(t, readOnlyTool("read"), writeTool("write"))
	tool, err := NewSubagentToolWithManager(settings, parent, launcher, nil, manager)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	return catalog, manager
}

// The changed-file list is bounded in bytes as well as by count: a long list of long paths
// is cut to the result limit, and the files it leaves out are counted.
func TestSubagentChangedFilesListIsCutToTheResultLimit(t *testing.T) {
	settings := writeSettings(writerAgent("fixer", "read", "write"))
	settings.ChangedFilesMax, settings.ResultMaxBytes = 100, 512
	var files []string
	for i := range 40 {
		files = append(files, fmt.Sprintf("generated/%03d/%s.go", i, strings.Repeat("deep", 20)))
	}
	catalog, _ := buildSubagentCatalog(t, settings, changesLauncher(files...), readOnlyTool("read"), writeTool("write"))
	out := subagentCall(t, catalog, `{"task":"fix"}`)
	line := ""
	for _, text := range strings.Split(out, "\n") {
		if strings.HasPrefix(text, "written_files: ") {
			line = text
		}
	}
	if len(line) > 512+len("written_files: ") {
		t.Fatalf("the written_files value is %d bytes, over result_max_bytes 512:\n%s", len(line), out)
	}
	kept := strings.Count(line, " (created)")
	if !strings.Contains(out, fmt.Sprintf("written_files_omitted: %d\n", 40-kept)) {
		t.Fatalf("the %d files the cut left out are not all counted (kept %d):\n%s", 40-kept, kept, out)
	}
}

// The byte count that decides whether a file fits is the length of the list it would make.
func TestSubagentChangedFilesByteCountIsTheLengthOfTheList(t *testing.T) {
	files := []subagentFileChange{{Path: "a.go", Action: "created"}, {Path: "src/b.go", Action: "edited"}}
	more := subagentFileChange{Path: "c/d.go", Action: "created"}
	if got, want := filesValueBytes(files, more), len(filesValue(append(append([]subagentFileChange(nil), files...), more))); got != int64(want) {
		t.Fatalf("filesValueBytes = %d, the list is %d bytes", got, want)
	}
	if got, want := filesValueBytes(nil, more), len(filesValue([]subagentFileChange{more})); got != int64(want) {
		t.Fatalf("filesValueBytes of the first file = %d, the list is %d bytes", got, want)
	}
}
