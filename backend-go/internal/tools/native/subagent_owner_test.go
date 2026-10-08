package native

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

// subagentCallAs calls the tool as the session with the given key.
func subagentCallAs(t *testing.T, catalog *tools.Catalog, sessionKey, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_9", "name": "subagent", "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: sessionKey})
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

// sharedManagerCatalogs builds two tools over one manager, as two sessions that share a
// storage directory get.
func sharedManagerCatalogs(t *testing.T, launcher SubagentLauncher) (*tools.Catalog, *tools.Catalog, *SubagentManager) {
	t.Helper()
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	build := func() *tools.Catalog {
		parent := withExecutors(t, readOnlyTool("read"))
		tool, err := NewSubagentToolWithManager(subagentSettings(agentNamed("explore")), parent, launcher, nil, manager)
		if err != nil {
			t.Fatal(err)
		}
		catalog, err := tools.NewCatalog(append(parent, tool))
		if err != nil {
			t.Fatal(err)
		}
		return catalog
	}
	return build(), build(), manager
}

func TestSubagentRunBelongsToTheSessionThatStartedIt(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var cancelled atomic.Int64
	first, second, _ := sharedManagerCatalogs(t, blockedLauncher(release, &cancelled))

	id := runIDOf(t, subagentCallAs(t, first, "alpha", `{"task":"x","wait_ms":5}`))

	for name, arguments := range map[string]string{
		"read":  `{"id":"` + id + `","wait_ms":0}`,
		"stop":  `{"id":"` + id + `","stop":true,"wait_ms":0}`,
		"other": `{"id":"` + id + `"}`,
	} {
		if out := subagentCallAs(t, second, "beta", arguments); !strings.Contains(out, "error_code: unknown_run") {
			t.Errorf("%s: another session reached the run:\n%s", name, out)
		}
	}
	if cancelled.Load() != 0 {
		t.Fatal("another session's stop cancelled the child")
	}
	if out := subagentCallAs(t, second, "beta", `{}`); strings.Contains(out, id) {
		t.Errorf("another session lists the run:\n%s", out)
	}
	if out := subagentCallAs(t, first, "alpha", `{}`); !strings.Contains(out, id) {
		t.Errorf("the owner does not list its run:\n%s", out)
	}
	if out := subagentCallAs(t, first, "alpha", `{"id":"`+id+`","wait_ms":0}`); !strings.Contains(out, "still running") {
		t.Errorf("the owner cannot read its run:\n%s", out)
	}
}

func TestSubagentCancelOwnerStopsOnlyThatSessionsActiveRuns(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var cancelled atomic.Int64
	first, _, manager := sharedManagerCatalogs(t, blockedLauncher(release, &cancelled))

	mine := runIDOf(t, subagentCallAs(t, first, "alpha", `{"task":"one","wait_ms":5}`))
	other := runIDOf(t, subagentCallAs(t, first, "beta", `{"task":"two","wait_ms":5}`))

	if got := manager.CancelOwner("alpha"); got != 1 {
		t.Fatalf("CancelOwner(alpha) = %d, want 1", got)
	}
	if out := subagentCallAs(t, first, "alpha", `{"id":"`+mine+`","wait_ms":2000}`); !strings.Contains(out, "was stopped") {
		t.Fatalf("the cancelled run did not end as stopped:\n%s", out)
	}
	if cancelled.Load() != 1 {
		t.Fatalf("%d children saw a cancellation, want 1", cancelled.Load())
	}
	if out := subagentCallAs(t, first, "beta", `{"id":"`+other+`","wait_ms":0}`); !strings.Contains(out, "still running") {
		t.Fatalf("the other session's run was touched:\n%s", out)
	}
	// An ended run is not counted again, and a session with no runs is a no-op.
	if got := manager.CancelOwner("alpha"); got != 0 {
		t.Errorf("second CancelOwner(alpha) = %d, want 0 (already ended)", got)
	}
	if got := manager.CancelOwner("nobody"); got != 0 {
		t.Errorf("CancelOwner(nobody) = %d, want 0", got)
	}
}

func TestSubagentCancelOwnerOnANilManagerIsHarmless(t *testing.T) {
	var manager *SubagentManager
	if got := manager.CancelOwner("alpha"); got != 0 {
		t.Fatalf("CancelOwner on nil = %d, want 0", got)
	}
}

func TestSubagentCancelOwnerReachesAQueuedRun(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	settings := subagentSettings(agentNamed("explore"))
	settings.MaxConcurrent = 1
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := NewSubagentToolWithManager(settings, parent, blockedLauncher(release, nil), nil, manager)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	subagentCallAs(t, catalog, "alpha", `{"task":"holds the slot","wait_ms":5}`)
	queued := runIDOf(t, subagentCallAs(t, catalog, "alpha", `{"task":"waits","wait_ms":5}`))
	if out := subagentCallAs(t, catalog, "alpha", `{"id":"`+queued+`","wait_ms":0}`); !strings.Contains(out, "still queued") {
		t.Fatalf("second run is not queued:\n%s", out)
	}
	if got := manager.CancelOwner("alpha"); got != 2 {
		t.Fatalf("CancelOwner = %d, want 2 (one running, one queued)", got)
	}
	if out := subagentCallAs(t, catalog, "alpha", `{"id":"`+queued+`","wait_ms":2000}`); !strings.Contains(out, "was stopped") {
		t.Fatalf("the queued run did not end as stopped:\n%s", out)
	}
}

func stateContextSettings(states ...string) sessionfs.NativeSubagentStateContextSettings {
	return sessionfs.NativeSubagentStateContextSettings{States: states, Limit: 20}
}

func TestSubagentStateContextListsOnlyTheSessionsRunsInTheAllowedStates(t *testing.T) {
	release := make(chan struct{})
	first, _, manager := sharedManagerCatalogs(t, blockedLauncher(release, nil))

	running := runIDOf(t, subagentCallAs(t, first, "alpha", `{"task":"still going","wait_ms":5}`))
	subagentCallAs(t, first, "beta", `{"task":"belongs to beta","wait_ms":5}`)
	close(release)
	// Both are done now.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(subagentCallAs(t, first, "alpha", `{"id":"`+running+`","wait_ms":0}`), "finished") {
		time.Sleep(5 * time.Millisecond)
	}

	section, err := CollectSubagentStateContext(manager, "alpha", stateContextSettings("done"))
	if err != nil {
		t.Fatal(err)
	}
	if len(section.Items) != 1 || section.Items[0].ID != running || section.Items[0].State != "done" || section.Items[0].Agent != "explore" {
		t.Fatalf("items = %#v; want only alpha's done run %s", section.Items, running)
	}
	item := section.Items[0]
	if item.Task != "still going" || item.Rounds != 4 || item.Tokens != 900 || item.StartedAt == "" || item.EndedAt == "" {
		t.Errorf("item = %#v", item)
	}
	encoded, _ := json.Marshal(section)
	if strings.Contains(string(encoded), "late answer") {
		t.Errorf("the answer was copied into the snapshot: %s", encoded)
	}
	none, err := CollectSubagentStateContext(manager, "alpha", stateContextSettings("running", "queued"))
	if err != nil || len(none.Items) != 0 || none.HasMore {
		t.Fatalf("running states = %#v, err %v; want none", none, err)
	}
}

func TestSubagentStateContextSortsNewestFirstAndLimits(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	first, _, manager := sharedManagerCatalogs(t, blockedLauncher(release, nil))
	var ids []string
	for _, task := range []string{"one", "two", "three"} {
		ids = append(ids, runIDOf(t, subagentCallAs(t, first, "alpha", `{"task":"`+task+`","wait_ms":2}`)))
		time.Sleep(3 * time.Millisecond)
	}
	// Max concurrent is 2, so the third is queued; both states are listed.
	settings := sessionfs.NativeSubagentStateContextSettings{States: []string{"queued", "running"}, Limit: 2}
	section, err := CollectSubagentStateContext(manager, "alpha", settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(section.Items) != 2 || section.Items[0].ID != ids[2] || section.Items[1].ID != ids[1] || !section.HasMore {
		t.Fatalf("items = %#v, has_more = %v; want %s then %s, more", section.Items, section.HasMore, ids[2], ids[1])
	}
	if section.Items[0].State != "queued" || section.Items[1].State != "running" {
		t.Errorf("states = %s, %s", section.Items[0].State, section.Items[1].State)
	}
}

func TestSubagentStateContextTrimsItsOldestItemFirst(t *testing.T) {
	section := &SubagentStateContext{Items: []SubagentStateContextItem{{ID: "sub_new"}, {ID: "sub_old"}}}
	if section.StateContextItemCount() != 2 {
		t.Fatalf("count = %d", section.StateContextItemCount())
	}
	if !section.TrimStateContextItem() || len(section.Items) != 1 || section.Items[0].ID != "sub_new" || section.OmittedBySnapshot != 1 {
		t.Fatalf("after trim: %#v", section)
	}
	section.TrimStateContextItem()
	if section.TrimStateContextItem() {
		t.Error("trimming an empty section reported progress")
	}
	var empty *SubagentStateContext
	if empty.StateContextItemCount() != 0 || empty.TrimStateContextItem() {
		t.Error("a nil section is not empty")
	}
}

func TestSubagentStateContextNeedsAManager(t *testing.T) {
	if _, err := CollectSubagentStateContext(nil, "alpha", stateContextSettings("running")); err == nil {
		t.Fatal("a missing manager was accepted")
	}
}
