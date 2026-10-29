package native

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// filledRunScope returns a context whose fields are all set, so a copy that drops one is
// visible. The hooks are no-ops; every other field gets a value a nested call can compare.
func filledRunScope(root string) tools.ExecutionContext {
	return tools.ExecutionContext{
		CallID:        "outer-call",
		ToolName:      "exec",
		WorkspaceRoot: root,
		SessionDir:    root,
		SessionKey:    "session-key",
		RequestInteraction: func(context.Context, tools.InteractionIdentity, tools.InteractionQuestion) (tools.InteractionAnswer, error) {
			return tools.InteractionAnswer{}, nil
		},
		OnInteractionResolved: func(tools.InteractionIdentity, tools.InteractionAnswer, json.RawMessage) error {
			return nil
		},
		RawArguments:      json.RawMessage(`{"filled":true}`),
		PathScope:         &tools.PathScope{Allow: []string{"src"}},
		OnFileChange:      func(string, string) {},
		ProcessOwner:      "sub_00000001",
		ProcessOwnerLimit: 3,
		ProcessAsync:      true,
		AgentName:         "coder",
		AgentDepth:        2,
		RootRun:           "sub_00000001",
		RemoteHops:        1,
	}
}

// reportRunScope compares got with want field by field and reports every field that is
// neither equal nor named in notInherited. A field added to ExecutionContext and silently
// dropped by a copy fails here.
func reportRunScope(t *testing.T, got, want tools.ExecutionContext, notInherited map[string]string) {
	t.Helper()
	gotValue := reflect.ValueOf(got)
	wantValue := reflect.ValueOf(want)
	for index := 0; index < gotValue.NumField(); index++ {
		name := gotValue.Type().Field(index).Name
		want := wantValue.Field(index)
		got := gotValue.Field(index)
		if reason, listed := notInherited[name]; listed {
			if reason == "" {
				t.Errorf("%s is listed as not inherited without a reason", name)
			}
			continue
		}
		if got.Kind() == reflect.Func {
			if got.IsNil() != want.IsNil() {
				t.Errorf("%s was dropped: got nil=%t, want nil=%t", name, got.IsNil(), want.IsNil())
			}
			continue
		}
		if !reflect.DeepEqual(got.Interface(), want.Interface()) {
			t.Errorf("%s was not carried over: got %v, want %v", name, got.Interface(), want.Interface())
		}
	}
	for name := range notInherited {
		if gotValue.FieldByName(name).IsValid() == false {
			t.Errorf("%s is listed as not inherited but ExecutionContext has no such field", name)
		}
	}
}

// TestToolSmithBridgeKeepsTheWholeRunScopeOfTheOuterCall is the check the hand-written
// literal at the bridge could not pass: a nested call must see everything this call sees
// (AgentDepth, RootRun, RemoteHops above all) except the per-call identity the catalog sets.
func TestToolSmithBridgeKeepsTheWholeRunScopeOfTheOuterCall(t *testing.T) {
	root := t.TempDir()
	dir, err := os.MkdirTemp(shortTempDir(t), "tool-smith-")
	if err != nil {
		t.Fatal(err)
	}
	parent := filledRunScope(root)
	bridge, err := startToolSmithBridge(context.Background(), dir, bridgeSettings(t, root, "read"),
		[]tools.Tool{bridgeTestTool(t, "read", true, func(map[string]any) string { return "r" })}, parent)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	reportRunScope(t, bridge.execution, parent, map[string]string{
		"CallID":       "the catalog sets the call id of each nested call",
		"ToolName":     "the catalog sets the tool name of each nested call",
		"RawArguments": "the catalog sets the arguments of each nested call",
	})
	for _, check := range []struct {
		name      string
		got, want any
	}{
		{"AgentDepth", bridge.execution.AgentDepth, parent.AgentDepth},
		{"RootRun", bridge.execution.RootRun, parent.RootRun},
		{"RemoteHops", bridge.execution.RemoteHops, parent.RemoteHops},
		{"ProcessOwner", bridge.execution.ProcessOwner, parent.ProcessOwner},
	} {
		if check.got != check.want {
			t.Fatalf("%s of a nested call = %v, this call has %v", check.name, check.got, check.want)
		}
	}
}

// TestSubagentChildExecutionContextKeepsTheRunScope checks the second copy: the child keeps
// the run it belongs to and does not inherit what belongs to the parent's call.
func TestSubagentChildExecutionContextKeepsTheRunScope(t *testing.T) {
	root := t.TempDir()
	parent := filledRunScope(root)
	child := childExecutionContext(parent, sessionfs.NativeSubagentAgent{Name: "fixer"})
	reportRunScope(t, child, parent, map[string]string{
		"CallID":                "the catalog sets the child call's own id",
		"ToolName":              "the catalog sets the child call's own tool name",
		"RawArguments":          "the catalog sets the child call's own arguments",
		"AgentName":             "the child is the agent its run names",
		"PathScope":             "the child's scope is narrowed to its own paths",
		"RequestInteraction":    "the child asks its own questions, not the parent's call",
		"OnInteractionResolved": "it belongs to the parent's interaction handler",
		"OnFileChange":          "the manager names the child's own run when it starts it",
	})
	if child.AgentName != "fixer" {
		t.Fatalf("the child runs as %q", child.AgentName)
	}
	if child.RequestInteraction != nil || child.OnInteractionResolved != nil || child.OnFileChange != nil {
		t.Fatalf("the child inherited a hook of the parent's call: %+v", child)
	}
	// The narrowed scope: the parent allowed src, the agent allows nothing of its own.
	if child.PathScope == nil || len(child.PathScope.Allow) != 1 || child.PathScope.Allow[0] != root+"/src" {
		t.Fatalf("the child's path scope = %+v", child.PathScope)
	}
}
