package native

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/tools"
)

func memoryCall(t *testing.T, catalog *tools.Catalog, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "call-memory", "name": "memory", "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	return item.Output
}

func TestMemoryErrorsListTheValidValues(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	settings := memoryTestSettings(root)
	settings.AllowedTypes = []string{"note", "plan"}
	store := NewMemoryStore(root)
	tool, err := NewMemoryTool(settings, store)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, title := range []string{"a", "b", "c"} {
		out := memoryCall(t, catalog, `{"operation":"create","type":"note","data":{"title":"`+title+`"}}`)
		for _, line := range strings.Split(out, "\n") {
			if ref, ok := strings.CutPrefix(strings.TrimSpace(line), "ref: "); ok {
				refs = append(refs, ref)
			}
		}
	}
	if len(refs) != 3 {
		t.Fatalf("created %d refs, want 3", len(refs))
	}

	notAllowed := memoryCall(t, catalog, `{"operation":"list","type":"secret"}`)
	if !strings.Contains(notAllowed, "memory type is not allowed") || !strings.Contains(notAllowed, "available_types: note, plan") {
		t.Errorf("a type outside the allowed list did not name the allowed types:\n%s", notAllowed)
	}

	missing := memoryCall(t, catalog, `{"operation":"get","type":"note","ref":"note/note-20200101T000000.000Z-0000000000000001-00000000-0000-4000-8000-000000000000.json"}`)
	if !strings.Contains(missing, "memory record was not found") {
		t.Fatalf("expected a not-found answer:\n%s", missing)
	}
	// ListLimit is 2, so only the two newest refs are offered.
	line := ""
	for _, candidate := range strings.Split(missing, "\n") {
		if strings.HasPrefix(candidate, "available_refs: ") {
			line = candidate
		}
	}
	if line == "" || strings.Count(line, "note/") != 2 || !strings.Contains(line, refs[2]) || !strings.Contains(line, refs[1]) || strings.Contains(line, refs[0]) {
		t.Errorf("not-found answer did not list the two newest refs %v:\n%s", refs[1:], missing)
	}

	invalid := memoryCall(t, catalog, `{"operation":"get","type":"note","ref":"nonsense"}`)
	if !strings.Contains(invalid, "memory ref is invalid") || !strings.Contains(invalid, "available_refs: ") {
		t.Errorf("an invalid ref did not list refs:\n%s", invalid)
	}

	empty := memoryCall(t, catalog, `{"operation":"get","type":"plan","ref":"nonsense"}`)
	if strings.Contains(empty, "available_refs") {
		t.Errorf("a type with no records offered refs:\n%s", empty)
	}
}

func TestTaskNotFoundListsTheTaskIDs(t *testing.T) {
	directory := t.TempDir()
	settings := taskTestSettings(filepath.Join(directory, "tasks"))
	store := NewTaskStore(settings.StorageDir)
	if out := runTaskTool(t, store, settings, 1, map[string]any{"id": "T-001"}); strings.Contains(out, "available_ids") {
		t.Errorf("an empty store offered ids:\n%s", out)
	}
	first := taskIDFromOutput(t, runTaskTool(t, store, settings, 2, map[string]any{"title": "one"}))
	second := taskIDFromOutput(t, runTaskTool(t, store, settings, 3, map[string]any{"title": "two"}))
	done := taskIDFromOutput(t, runTaskTool(t, store, settings, 4, map[string]any{"title": "three"}))
	runTaskTool(t, store, settings, 5, map[string]any{"id": done, "status": "done"})
	out := runTaskTool(t, store, settings, 6, map[string]any{"id": "T-999"})
	if !strings.Contains(out, "error_code: task_not_found") {
		t.Fatalf("expected task_not_found:\n%s", out)
	}
	for _, id := range []string{first, second, done} {
		if !strings.Contains(out, id) {
			t.Errorf("available_ids lacks %s (done tasks are valid ids too):\n%s", id, out)
		}
	}
	if !strings.Contains(out, "available_ids: ") {
		t.Errorf("no available_ids line:\n%s", out)
	}
}

func TestProcessNotFoundListsTheProcessIDs(t *testing.T) {
	f := newProcessFixture(t)
	if out := f.call(t, "", map[string]any{"action": "status", "id": "proc_missing"}); !strings.Contains(out, "process_not_found") || strings.Contains(out, "available_ids") {
		t.Errorf("an empty manager must not offer ids:\n%s", out)
	}
	id := f.start(t, "", "sleep 30")
	out := f.call(t, "", map[string]any{"action": "status", "id": "proc_missing"})
	if !strings.Contains(out, "available_ids: "+id) {
		t.Errorf("not-found answer did not list %s:\n%s", id, out)
	}
}

func TestProcessNotFoundOffersOnlyTheCallersOwnIDs(t *testing.T) {
	f := newProcessFixture(t)
	parent := f.start(t, "", "sleep 30")
	child := f.start(t, "sub_one", "sleep 30")
	out := f.call(t, "sub_one", map[string]any{"action": "status", "id": "proc_missing"})
	if !strings.Contains(out, "available_ids: "+child) || strings.Contains(out, parent) {
		t.Errorf("a child must see its own id and not the parent's %s:\n%s", parent, out)
	}
}

func TestSubagentUnknownRunListsTheRunIDs(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var cancelled atomic.Int64
	first, second, _ := sharedManagerCatalogs(t, blockedLauncher(release, &cancelled))
	if out := subagentCallAs(t, first, "alpha", `{"id":"sub_00000000"}`); !strings.Contains(out, "unknown_run") || strings.Contains(out, "available_ids") {
		t.Errorf("a session with no runs must get no ids:\n%s", out)
	}
	id := runIDOf(t, subagentCallAs(t, first, "alpha", `{"task":"x","wait_ms":5}`))
	out := subagentCallAs(t, first, "alpha", `{"id":"sub_00000000"}`)
	if !strings.Contains(out, "error_code: unknown_run") || !strings.Contains(out, "available_ids: "+id) {
		t.Errorf("unknown run did not list %s:\n%s", id, out)
	}
	if other := subagentCallAs(t, second, "beta", `{"id":"sub_00000000"}`); strings.Contains(other, id) {
		t.Errorf("another session was offered the run id:\n%s", other)
	}
}
