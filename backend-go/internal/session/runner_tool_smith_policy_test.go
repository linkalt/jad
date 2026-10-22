package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/checkpoint"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

func toolSmithPolicyCatalog(t *testing.T, approval string) *tools.Catalog {
	t.Helper()
	catalog, err := native.BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "sample", "tools", "native", "tool_smith.json")},
		NativeToolSmith:     &sessionfs.NativeToolSmithSettings{Approval: approval},
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestToolSmithRunIsSerializedAndCheckpointedWhileListIsNot(t *testing.T) {
	catalog := toolSmithPolicyCatalog(t, "")
	run := checkpoint.NewRun(checkpoint.NewService(checkpoint.Options{Settings: sessionfs.CheckpointSettings{Enabled: true, SnapshotOn: "first_mutation"}}), "turn", 1)
	if run == nil {
		t.Fatal("no checkpoint run")
	}
	runCall := call("tool_smith", `{"run":{"function":"return 1"}}`)
	listCall := call("tool_smith", `{}`)
	for name, tc := range map[string]struct {
		call  json.RawMessage
		state bool
	}{"run": {runCall, true}, "list": {listCall, false}} {
		if got := catalog.ChangesState(tc.call); got != tc.state {
			t.Errorf("%s: ChangesState = %v, want %v", name, got, tc.state)
		}
		if got := roundChangesWorkspace(catalog, []json.RawMessage{tc.call}, run); got != tc.state {
			t.Errorf("%s: roundChangesWorkspace = %v, want %v", name, got, tc.state)
		}
	}
	if got := toolBatchConcurrency([]json.RawMessage{listCall, runCall, listCall}, 8, "ordered", catalog.ChangesState); got != 1 {
		t.Errorf("a batch holding a run ran with concurrency %d, want 1", got)
	}
	if got := toolBatchConcurrency([]json.RawMessage{listCall, listCall}, 8, "ordered", catalog.ChangesState); got != 8 {
		t.Errorf("a batch of lists ran with concurrency %d, want 8", got)
	}
}

func TestToolSmithApprovalAskGatesRunAndOffDoesNot(t *testing.T) {
	for _, tc := range []struct {
		approval string
		prompts  int
	}{{"ask", 1}, {"", 0}} {
		catalog := toolSmithPolicyCatalog(t, tc.approval)
		prompts := 0
		execution := tools.ExecutionContext{RequestApproval: func(_ context.Context, call tools.ApprovalCall) tools.ApprovalVerdict {
			prompts++
			return tools.ApprovalVerdict{Code: tools.ApprovalDenied, Reason: "no"}
		}}
		raw, err := catalog.ExecuteFunctionCall(context.Background(), call("tool_smith", `{"run":{"function":"return 1"}}`), execution)
		if err != nil {
			t.Fatal(err)
		}
		if prompts != tc.prompts {
			t.Errorf("approval %q: %d prompts, want %d", tc.approval, prompts, tc.prompts)
		}
		if blocked := strings.Contains(string(raw), "blocked"); blocked != (tc.prompts == 1) {
			t.Errorf("approval %q: blocked = %v in %s", tc.approval, blocked, raw)
		}
	}
}

func TestToolSmithListPromptsUnderAskToday(t *testing.T) {
	catalog := toolSmithPolicyCatalog(t, "ask")
	prompts := 0
	execution := tools.ExecutionContext{RequestApproval: func(context.Context, tools.ApprovalCall) tools.ApprovalVerdict {
		prompts++
		return tools.ApprovalVerdict{Code: tools.ApprovalDenied, Reason: "no"}
	}}
	if _, err := catalog.ExecuteFunctionCall(context.Background(), call("tool_smith", `{}`), execution); err != nil {
		t.Fatal(err)
	}
	if prompts != 1 {
		t.Errorf("list under ask prompted %d times; this test records the current behavior (1)", prompts)
	}
}
