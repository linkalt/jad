package native

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// writerAgent is an agent whose tools list names a tool that changes files.
func writerAgent(name string, names ...string) sessionfs.NativeSubagentAgent {
	agent := agentNamed(name)
	agent.Tools, agent.HasTools = names, true
	return agent
}

func writeSettings(agents ...sessionfs.NativeSubagentAgent) sessionfs.NativeSubagentSettings {
	return subagentSettings(agents...)
}

func TestSubagentChildGetsExactlyTheChangeToolsItLists(t *testing.T) {
	launcher := &fakeLauncher{}
	catalog, _ := buildSubagentCatalog(t, writeSettings(writerAgent("fixer", "read", "exec")), launcher,
		readOnlyTool("read"), writeTool("exec"), writeTool("tool_smith"))
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := strings.Join(toolNames(launcher.requests[0].Tools), ","); got != "read,exec" {
		t.Fatalf("child tools = %q, want exactly the listed read and exec", got)
	}

	// The session must still enable a change tool the list names.
	_, err := NewSubagentTool(subagentSettings(writerAgent("fixer", "read", "exec")), withExecutors(t, readOnlyTool("read")), nil)
	if err == nil || !strings.Contains(err.Error(), `lists "exec", which is not an enabled native tool`) {
		t.Fatalf("error = %v", err)
	}
}

func TestSubagentAgentWithoutAToolsListGetsTheChangeToolsToo(t *testing.T) {
	launcher := &fakeLauncher{}
	catalog, _ := buildSubagentCatalog(t, writeSettings(agentNamed("plain")), launcher,
		readOnlyTool("read"), writeTool("exec"), writeTool("tool_smith"))
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := strings.Join(toolNames(launcher.requests[0].Tools), ","); got != "read,exec,tool_smith" {
		t.Fatalf("an agent with no tools list got %q, want every tool", got)
	}
}

// A tool that is not one of exec, tool_smith, subagent and profile_message is not a change tool: an agent
// that has only such tools is a reader.
func TestSubagentToolsThatAreNotChangeToolsLeaveAnAgentAReader(t *testing.T) {
	built, err := newSubagentAgent(writerAgent("fixer", "read", "task"), nil, childNativePool(withExecutors(t, readOnlyTool("read"), writeTool("task"), writeTool("exec"))), true)
	if err != nil {
		t.Fatal(err)
	}
	if built.config.ChangesFiles() {
		t.Fatalf("an agent with read and task counts as a file changer: %v", poolNames(built.nativeTools))
	}
	// Nesting alone does not make an agent a writer: a child it starts that changes files is a call of the
	// agent's own run. exec does, for an agent that is not read_only.
	if built, err = newSubagentAgent(agentNamed("nester"), nil, childNativePool(withExecutors(t, readOnlyTool("read"))), true); err != nil || built.config.ChangesFiles() {
		t.Fatalf("an agent that may only nest is a writer: %v", err)
	}
	if built, err = newSubagentAgent(agentNamed("any"), nil, childNativePool(withExecutors(t, readOnlyTool("read"), writeTool("exec"))), false); err != nil || !built.config.ChangesFiles() {
		t.Fatalf("an agent with exec is a reader: %v", err)
	}
}

func TestSubagentChangeToolsMustBeEnabledInTheSession(t *testing.T) {
	_, err := NewSubagentTool(writeSettings(writerAgent("fixer", "read", "exec")),
		withExecutors(t, readOnlyTool("read")), nil)
	if err == nil || !strings.Contains(err.Error(), `lists "exec", which is not an enabled native tool (enabled: read)`) {
		t.Fatalf("error = %v", err)
	}
}

func TestSubagentCallMayChangeFilesOnlyForAnAgentThatCan(t *testing.T) {
	catalog, tool := buildSubagentCatalog(t, writeSettings(writerAgent("reader", "read"), writerAgent("fixer", "read", "exec")), &fakeLauncher{},
		readOnlyTool("read"), writeTool("exec"))
	if tool.MayChangeFiles == nil || !tool.ReadOnly {
		t.Fatalf("the tool must stay ReadOnly and carry the per-call hook: readOnly=%v hook=%v", tool.ReadOnly, tool.MayChangeFiles != nil)
	}
	for name, tc := range map[string]struct {
		arguments string
		want      bool
	}{
		"writer starts a task":     {`{"agent":"fixer","task":"fix it"}`, true},
		"reader starts a task":     {`{"agent":"reader","task":"look"}`, false},
		"list the agents":          {`{}`, false},
		"unknown agent":            {`{"agent":"nobody","task":"x"}`, false},
		"blank task is the list":   {`{"agent":"fixer","task":"  "}`, false},
		"writer, block_ms ignored": {`{"agent":"fixer","task":"fix","block_ms":0}`, true},
	} {
		call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c", "name": "subagent", "arguments": tc.arguments})
		if got := catalog.CallMayChangeFiles(call); got != tc.want {
			t.Errorf("%s: CallMayChangeFiles = %v, want %v", name, got, tc.want)
		}
		if got := catalog.ChangesState(call); got != tc.want {
			t.Errorf("%s: ChangesState = %v, want %v (it decides the ordered scheduling)", name, got, tc.want)
		}
	}
}

func TestSubagentListMarksTheAgentsThatCanChangeFiles(t *testing.T) {
	agents := []sessionfs.NativeSubagentAgent{writerAgent("reader", "read"), writerAgent("fixer", "read", "exec")}
	list := subagentAgentList(agents, 4096, nil)
	if !strings.Contains(list, "- fixer [can change files; runs to its end]") || strings.Contains(list, "reader [can") {
		t.Fatalf("agent list:\n%s", list)
	}
	catalog, _ := buildSubagentCatalog(t, writeSettings(agents...), &fakeLauncher{}, readOnlyTool("read"), writeTool("exec"))
	if out := subagentCall(t, catalog, `{}`); !strings.Contains(out, "fixer [can change files; runs to its end]") {
		t.Fatalf("the list call does not mark the writer:\n%s", out)
	}
}

func TestSubagentWriterIsNeverHandedOffAndBlockMSIsIgnored(t *testing.T) {
	release := make(chan struct{})
	var cancelled atomic.Int64
	launcher := blockedLauncher(release, &cancelled)
	catalog, _ := buildSubagentCatalog(t, writeSettings(writerAgent("fixer", "read", "exec")), launcher, readOnlyTool("read"), writeTool("exec"))
	done := make(chan string, 1)
	go func() { done <- subagentCall(t, catalog, `{"task":"fix","block_ms":0}`) }()
	select {
	case out := <-done:
		t.Fatalf("a writer was handed off with block_ms 0:\n%s", out)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case out := <-done:
		if strings.Contains(out, "still running") {
			t.Fatalf("the writer's result is a handoff:\n%s", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the call did not return after the child ended")
	}
}

func TestSubagentReaderIsStillHandedOffNextToWriters(t *testing.T) {
	release := make(chan struct{})
	var cancelled atomic.Int64
	catalog, _ := buildSubagentCatalog(t, writeSettings(writerAgent("reader", "read"), writerAgent("fixer", "read", "exec")), blockedLauncher(release, &cancelled), readOnlyTool("read"), writeTool("exec"))
	defer close(release)
	out := subagentCall(t, catalog, `{"agent":"reader","task":"look","block_ms":0}`)
	if !strings.Contains(out, "still running") {
		t.Fatalf("a read-only agent lost its handoff:\n%s", out)
	}
}

func TestSubagentWriterTimeoutStillBoundsTheCallAndWarnsAboutChanges(t *testing.T) {
	var cancelled atomic.Int64
	writer := writerAgent("fixer", "read", "exec")
	writer.TimeoutMS = 1000
	launcher := &fakeLauncher{run: func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
		<-ctx.Done()
		cancelled.Add(1)
		return SubagentResult{Text: "edited a.go, was about to edit b.go"}, ctx.Err()
	}}
	catalog, _ := buildSubagentCatalog(t, writeSettings(writer), launcher, readOnlyTool("read"), writeTool("exec"))
	started := time.Now()
	out := subagentCall(t, catalog, `{"task":"fix"}`)
	if time.Since(started) > 5*time.Second {
		t.Fatalf("the call outlived the agent's timeout: %v", time.Since(started))
	}
	for _, want := range []string{"error_code: subagent_timeout", "It can change files, so some changes may already be made", "edited a.go, was about to edit b.go", "partial: true"} {
		if !strings.Contains(out, want) {
			t.Errorf("timeout result lacks %q:\n%s", want, out)
		}
	}
	if cancelled.Load() != 1 {
		t.Errorf("the child was cancelled %d times, want 1", cancelled.Load())
	}
}

func TestSubagentReadOnlyTimeoutHasNoChangesWarning(t *testing.T) {
	reader := agentNamed("reader")
	reader.TimeoutMS = 1000
	launcher := &fakeLauncher{run: func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
		<-ctx.Done()
		return SubagentResult{}, ctx.Err()
	}}
	catalog, _ := buildSubagentCatalog(t, writeSettings(reader), launcher, readOnlyTool("read"))
	if out := subagentCall(t, catalog, `{"task":"look"}`); strings.Contains(out, "change files") {
		t.Fatalf("a read-only agent's timeout warns about changes:\n%s", out)
	}
}

func TestSubagentWriterFailureAndStopWarnAboutChanges(t *testing.T) {
	boom := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{}, errTest("provider down")
	}}
	catalog, _ := buildSubagentCatalog(t, writeSettings(writerAgent("fixer", "read", "exec")), boom, readOnlyTool("read"), writeTool("exec"))
	if out := subagentCall(t, catalog, `{"task":"fix"}`); !strings.Contains(out, "subagent_failed") || !strings.Contains(out, "some changes may already be made") {
		t.Fatalf("failed writer result:\n%s", out)
	}

	// A stopped writer says so too. The call is held by the test until it asks to stop.
	entered := make(chan struct{})
	slow := &fakeLauncher{run: func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
		close(entered)
		<-ctx.Done()
		return SubagentResult{Text: "half done"}, ctx.Err()
	}}
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	settings := writeSettings(writerAgent("fixer", "read", "exec"))
	parent := withExecutors(t, readOnlyTool("read"), writeTool("exec"))
	tool, err := NewSubagentToolWithManager(settings, parent, slow, nil, manager)
	if err != nil {
		t.Fatal(err)
	}
	stopCatalog, _ := tools.NewCatalog(append(parent, tool))
	result := make(chan string, 1)
	go func() { result <- subagentCall(t, stopCatalog, `{"task":"fix"}`) }()
	<-entered
	views := manager.list(testSessionKey)
	if len(views) != 1 {
		t.Fatalf("runs = %d", len(views))
	}
	manager.stop(manager.get(views[0].id, testSessionKey))
	select {
	case out := <-result:
		if !strings.Contains(out, "was stopped") || !strings.Contains(out, "some changes may already be made") {
			t.Fatalf("stopped writer result:\n%s", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a stopped writer did not return")
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

func TestSubagentWriterRunSurvivesManifestRoundTripWithItsChangesFlag(t *testing.T) {
	root := t.TempDir()
	boom := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{}, errTest("down")
	}}
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = manager.Close() })
	settings := writeSettings(writerAgent("fixer", "read", "exec"))
	settings.StorageDir = root
	parent := withExecutors(t, readOnlyTool("read"), writeTool("exec"))
	tool, err := NewSubagentToolWithManager(settings, parent, boom, nil, manager)
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := tools.NewCatalog(append(parent, tool))
	subagentCall(t, catalog, `{"task":"fix"}`)
	views := manager.list(testSessionKey)
	if len(views) != 1 || !views[0].changes {
		t.Fatalf("views = %+v; the run must remember the agent can change files", views)
	}
	_ = manager.Close()

	again := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = again.Close() })
	restored := again.list(testSessionKey)
	if len(restored) != 1 || !restored[0].changes {
		t.Fatalf("restored views = %+v; the flag was lost with the manifest", restored)
	}
}
