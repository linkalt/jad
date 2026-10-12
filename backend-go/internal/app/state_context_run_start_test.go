package app

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/tools/native"
	"example.com/agent-harness/backend-go/internal/transcript"
)

func (fixture *sessionStartFixture) storedBlocks() []transcript.StateContextEntry {
	fixture.t.Helper()
	data, err := os.ReadFile(fixture.snapshotPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		fixture.t.Fatal(err)
	}
	var entries []transcript.StateContextEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry transcript.StateContextEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			fixture.t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func (fixture *sessionStartFixture) addTask(title string) string {
	fixture.t.Helper()
	record, err := fixture.taskStore.Create(native.TaskInput{Title: title, Status: native.TaskInProgress, Priority: native.TaskPriorityHigh}, fixture.taskSettings)
	if err != nil {
		fixture.t.Fatal(err)
	}
	return record.ID
}

func (fixture *sessionStartFixture) deleteTask(id string) {
	fixture.t.Helper()
	if _, err := fixture.taskStore.Delete(id, fixture.taskSettings); err != nil {
		fixture.t.Fatal(err)
	}
}

func TestHTTPRunStartWritesABlockOnlyWhenTheDataChanges(t *testing.T) {
	fixture := newStateContextFixture(t, false, map[string]any{"run_start": map[string]any{"tools": []string{"task"}, "max_bytes": 4096}})

	fixture.run("one")
	if blocks := fixture.storedBlocks(); len(blocks) != 0 {
		t.Fatalf("no data and no earlier block: %d blocks written, want none", len(blocks))
	}

	id := fixture.addTask("first task")
	fixture.run("two")
	blocks := fixture.storedBlocks()
	if len(blocks) != 1 || blocks[0].Trigger != "run_start" || !strings.Contains(blocks[0].Text, "first task") || blocks[0].After != 3 {
		t.Fatalf("after the data appeared: %+v; want one run_start block after the second input", blocks)
	}

	fixture.run("three")
	if blocks = fixture.storedBlocks(); len(blocks) != 1 {
		t.Fatalf("unchanged data: %d blocks, want the same one", len(blocks))
	}

	fixture.deleteTask(id)
	fixture.run("four")
	blocks = fixture.storedBlocks()
	if len(blocks) != 2 || strings.Contains(blocks[1].Text, "first task") || !strings.Contains(blocks[1].Text, `"items": []`) {
		t.Fatalf("after the data was removed: %+v; want one block that reports the empty state", blocks)
	}

	fixture.run("five")
	if blocks = fixture.storedBlocks(); len(blocks) != 2 {
		t.Fatalf("empty state after an empty block: %d blocks, want 2", len(blocks))
	}

	fixture.addTask("second task")
	fixture.run("six")
	if blocks = fixture.storedBlocks(); len(blocks) != 3 || !strings.Contains(blocks[2].Text, "second task") {
		t.Fatalf("data back after an empty block: %+v; want a third block", blocks)
	}
}

func TestHTTPRunStartSkipsWhenAnotherTriggersLatestBlockHasTheSameData(t *testing.T) {
	fixture := newStateContextFixture(t, false, map[string]any{
		"session_start": map[string]any{"tools": []string{"task"}, "max_bytes": 4096},
		"run_start":     map[string]any{"tools": []string{"task"}, "max_bytes": 4096},
	})
	fixture.addTask("first task")
	fixture.run("one")
	fixture.run("two")
	blocks := fixture.storedBlocks()
	if len(blocks) != 1 || blocks[0].Trigger != "session_start" {
		t.Fatalf("blocks = %+v; want only the session_start block, since run_start sees the same data", blocks)
	}
	fixture.addTask("second task")
	fixture.run("three")
	if blocks = fixture.storedBlocks(); len(blocks) != 2 || blocks[1].Trigger != "run_start" {
		t.Fatalf("blocks = %+v; want a run_start block once the data changes", blocks)
	}
}
