package session

import (
	"context"
	"encoding/json"
	"testing"

	"jad/backend-go/internal/checkpoint"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// planCheckpointCatalog holds an exec-like tool (ReadOnly, a call with code and no read_only
// may change files) and a subagent-like one whose fixer agent may write.
func planCheckpointCatalog(t *testing.T) *tools.Catalog {
	t.Helper()
	validator, err := tools.CompileSchema([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	execute := func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "ran", nil }
	execTool := tools.Tool{
		Definition: tools.Definition{ID: "native:exec", Source: "native", Name: "exec", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Validator:  validator, ReadOnly: true, Execute: execute,
		MayChangeFiles: func(arguments map[string]any) bool {
			readOnly, _ := arguments["read_only"].(bool)
			return arguments["code"] != nil && !readOnly
		},
	}
	subagentTool := tools.Tool{
		Definition: tools.Definition{ID: "native:subagent", Source: "native", Name: "subagent", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Validator:  validator, ReadOnly: true, Execute: execute,
		MayChangeFiles: func(arguments map[string]any) bool { return arguments["agent"] == "fixer" },
	}
	catalog, err := tools.NewCatalog([]tools.Tool{execTool, subagentTool})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// A plan-mode run takes no checkpoint for the calls of a guarded tool: the ones that could
// change files are refused before they run, the others change none. A tool an allow entry
// keeps whole still takes it, and outside plan mode every exec call still does.
func TestRoundChangesWorkspaceSkipsPlanGuardedCalls(t *testing.T) {
	run := checkpoint.NewRun(checkpoint.NewService(checkpoint.Options{Settings: sessionfs.CheckpointSettings{Enabled: true, SnapshotOn: "first_mutation"}}), "turn", 1)
	if run == nil {
		t.Fatal("no checkpoint run")
	}
	catalog := planCheckpointCatalog(t)
	plan, err := catalog.PlanMode(nil)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := catalog.PlanMode([]string{"exec"})
	if err != nil {
		t.Fatal(err)
	}
	readOnlyExec := call("exec", `{"code":"ls","read_only":true}`)
	writingExec := call("exec", `{"code":"touch x"}`)
	writer := call("subagent", `{"agent":"fixer","task":"x"}`)
	for name, tc := range map[string]struct {
		catalog *tools.Catalog
		calls   []json.RawMessage
		want    bool
	}{
		"normal run, read_only exec":       {catalog, []json.RawMessage{readOnlyExec}, true},
		"normal run, writer agent":         {catalog, []json.RawMessage{writer}, true},
		"plan mode, read_only exec":        {plan, []json.RawMessage{readOnlyExec}, false},
		"plan mode, refused exec":          {plan, []json.RawMessage{writingExec}, false},
		"plan mode, refused writer agent":  {plan, []json.RawMessage{writer}, false},
		"plan mode, exec kept by an allow": {allowed, []json.RawMessage{readOnlyExec}, true},
	} {
		if got := roundChangesWorkspace(tc.catalog, tc.calls, run); got != tc.want {
			t.Errorf("%s: roundChangesWorkspace = %v, want %v", name, got, tc.want)
		}
	}
	if !plan.IsPlanGuardedCall(readOnlyExec) || allowed.IsPlanGuardedCall(readOnlyExec) || catalog.IsPlanGuardedCall(readOnlyExec) {
		t.Fatal("only the plan-mode exec without an allow entry is guarded")
	}
}
