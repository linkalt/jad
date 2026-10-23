package session

import (
	"encoding/json"
	"path/filepath"
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
	runCall := call("tool_smith", `{"function":"return 1"}`)
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
