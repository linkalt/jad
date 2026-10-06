package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
	"example.com/agent-harness/backend-go/internal/tools/native"
)

// orderingOutcome is what one edit+edit+edit+read response left behind.
type orderingOutcome struct {
	editsOK     bool
	readSawLast bool
	fileIsV3    bool
	order       string
}

// runChainedResponse runs three chained edits and a read of one file through
// executeToolRound, the way Runner.Run does, with the given concurrency.
func runChainedResponse(t *testing.T, limit int, scheduling string) orderingOutcome {
	t.Helper()
	workspace := t.TempDir()
	backupDir := filepath.Join(workspace, "backups")
	mutation := sessionfs.NativeMutationSettings{BackupDir: &backupDir, BackupMaxCount: 5, BackupMaxTotalBytes: 10 << 20}
	policy := sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}}
	readTool, err := native.NewReadTool(sessionfs.NativeReadSettings{Policy: policy, MaxBytes: 64 << 10, MaxFileBytes: 1 << 20, DefaultLimit: 250})
	if err != nil {
		t.Fatal(err)
	}
	editTool, err := native.NewEditTool(sessionfs.NativeEditSettings{Policy: policy, MaxBytes: 1 << 20, NativeMutationSettings: mutation}, native.NewPathLocks())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{readTool, editTool})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	call := func(id, name, args string) json.RawMessage {
		encoded, err := json.Marshal(map[string]string{"type": "function_call", "call_id": id, "name": name, "arguments": args})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	calls := []json.RawMessage{
		call("e1", "edit", `{"path":"a.txt","oldText":"v0","newText":"v1"}`),
		call("e2", "edit", `{"path":"a.txt","oldText":"v1","newText":"v2"}`),
		call("e3", "edit", `{"path":"a.txt","oldText":"v2","newText":"v3"}`),
		call("r1", "read", `{"path":"a.txt"}`),
	}
	var order []string
	round, err := executeToolRound(context.Background(), catalog, calls,
		tools.ExecutionContext{WorkspaceRoot: workspace, SessionDir: workspace}, limit, scheduling,
		func(output json.RawMessage) { order = append(order, functionCallID(output)) })
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{}
	for _, output := range round.Completed {
		outputs[functionCallID(output)] = string(output)
	}
	outcome := orderingOutcome{editsOK: true, order: strings.Join(order, ",")}
	for _, id := range []string{"e1", "e2", "e3"} {
		if !strings.Contains(outputs[id], `\"status\":\"ok\"`) {
			outcome.editsOK = false
		}
	}
	outcome.readSawLast = strings.Contains(outputs["r1"], "v3")
	content, err := os.ReadFile(filepath.Join(workspace, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	outcome.fileIsV3 = string(content) == "v3\n"
	return outcome
}

// The calls of one response used to start together, so the read did not wait for
// the edits written before it and the edits reached the per-path lock in no fixed
// order (sync.Mutex is not first-in, first-out). Measured at limit 8 over 200 runs
// before the fix: the edits failed or applied out of order in 73, and the read saw
// the final content in 2. With tool_call_scheduling "ordered" (the default) a
// response holding a call that can change state runs one call at a time in the
// model's order. The TypeScript twin is backend/test/tool-batch-ordering.test.ts.
func TestToolRoundRunsEditEditEditReadInTheModelsOrder(t *testing.T) {
	const runs = 20
	readSawFinal, editsInOrder := 0, 0
	for range runs {
		outcome := runChainedResponse(t, 8, "ordered")
		if outcome.readSawLast {
			readSawFinal++
		}
		if outcome.editsOK && outcome.fileIsV3 {
			editsInOrder++
		}
	}
	if readSawFinal != runs || editsInOrder != runs {
		t.Fatalf("over %d runs the read saw the final content in %d and the edits applied in order in %d", runs, readSawFinal, editsInOrder)
	}
}

func TestToolRoundWithLimitOneRunsEditEditEditReadInOrderEvenWhenConcurrent(t *testing.T) {
	for range 20 {
		outcome := runChainedResponse(t, 1, "concurrent")
		if !outcome.editsOK || !outcome.readSawLast || !outcome.fileIsV3 || outcome.order != "e1,e2,e3,r1" {
			t.Fatalf("limit 1 outcome = %+v, want edits ok, read sees v3, file v3, order e1,e2,e3,r1", outcome)
		}
	}
}

func TestToolRoundOrderedRunsReadOnlyCallsTogether(t *testing.T) {
	calls := []string{"read", "grep", "read"}
	changes := func(name string) bool { return name == "edit" }
	if got := toolBatchConcurrency(calls, 8, "ordered", changes); got != 8 {
		t.Fatalf("read-only batch concurrency = %d, want 8", got)
	}
}
