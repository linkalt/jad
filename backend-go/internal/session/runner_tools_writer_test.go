package session

import (
	"context"
	"encoding/json"
	"testing"

	"example.com/agent-harness/backend-go/internal/checkpoint"
	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

// hookedCatalog holds a read-only tool that can still say a call changes files, the
// way the subagent tool does for an agent that may write, and a plain read tool.
func hookedCatalog(t *testing.T) *tools.Catalog {
	t.Helper()
	validator, err := tools.CompileSchema([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	execute := func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "", nil }
	catalog, err := tools.NewCatalog([]tools.Tool{
		{
			Definition: tools.Definition{ID: "test:subagent", Source: "native", Name: "subagent", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Validator:  validator, ReadOnly: true, Execute: execute,
			MayChangeFiles: func(arguments map[string]any) bool { return arguments["agent"] == "fixer" },
		},
		{
			Definition: tools.Definition{ID: "test:read", Source: "native", Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Validator:  validator, ReadOnly: true, Execute: execute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func call(name, arguments string) json.RawMessage {
	encoded, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c-" + name, "name": name, "arguments": arguments})
	return encoded
}

// A call that can change files must take the checkpoint, though the tool is ReadOnly
// and its name is not one the checkpoint knows.
func TestRoundChangesWorkspaceCountsAWriterSubagentCall(t *testing.T) {
	run := checkpoint.NewRun(checkpoint.NewService(checkpoint.Options{Settings: sessionfs.CheckpointSettings{Enabled: true, SnapshotOn: "first_mutation"}}), "turn", 1)
	if run == nil {
		t.Fatal("no checkpoint run")
	}
	catalog := hookedCatalog(t)
	for name, tc := range map[string]struct {
		calls []json.RawMessage
		want  bool
	}{
		"writer agent":             {[]json.RawMessage{call("subagent", `{"agent":"fixer","task":"x"}`)}, true},
		"reader agent":             {[]json.RawMessage{call("subagent", `{"agent":"reader","task":"x"}`)}, false},
		"read only":                {[]json.RawMessage{call("read", `{}`)}, false},
		"reader then writer":       {[]json.RawMessage{call("subagent", `{"agent":"reader","task":"x"}`), call("subagent", `{"agent":"fixer","task":"y"}`)}, true},
		"writer among plain reads": {[]json.RawMessage{call("read", `{}`), call("subagent", `{"agent":"fixer","task":"y"}`)}, true},
	} {
		if got := roundChangesWorkspace(catalog, tc.calls, run); got != tc.want {
			t.Errorf("%s: roundChangesWorkspace = %v, want %v", name, got, tc.want)
		}
	}
	if roundChangesWorkspace(catalog, []json.RawMessage{call("subagent", `{"agent":"fixer","task":"x"}`)}, nil) {
		t.Error("with checkpoints off a writer call must not report a change")
	}
}

func TestOrderedSchedulingRunsAWriterSubagentAloneAndReadersTogether(t *testing.T) {
	catalog := hookedCatalog(t)
	writer := []json.RawMessage{call("subagent", `{"agent":"reader","task":"a"}`), call("subagent", `{"agent":"fixer","task":"b"}`), call("subagent", `{"agent":"reader","task":"c"}`)}
	if got := toolBatchConcurrency(writer, 8, "ordered", catalog.ChangesState); got != 1 {
		t.Errorf("a batch holding a writer ran with concurrency %d, want 1", got)
	}
	readers := []json.RawMessage{call("subagent", `{"agent":"reader","task":"a"}`), call("subagent", `{"agent":"reader","task":"c"}`)}
	if got := toolBatchConcurrency(readers, 8, "ordered", catalog.ChangesState); got != 8 {
		t.Errorf("a batch of readers ran with concurrency %d, want 8", got)
	}
}
