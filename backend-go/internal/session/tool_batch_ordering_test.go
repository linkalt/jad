package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/tools"
)

// orderingTools are an edit that replaces oldText with newText in a file (failing when the
// file does not hold oldText) and a read-only read that returns the file: the scheduler
// decides their order only from ReadOnly, so they stand for any changing and reading tool.
func orderingTools(t *testing.T) []tools.Tool {
	t.Helper()
	validator, err := tools.CompileSchema([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	result := func(id string, status tools.Status, text string) (string, error) {
		return tools.RenderToolResult(tools.ResultOptions{ID: id, Status: status, Summary: "done", Content: &tools.Content{Text: text}})
	}
	edit := func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
		path := filepath.Join(execution.WorkspaceRoot, arguments["path"].(string))
		content, err := os.ReadFile(path)
		oldText, newText := arguments["oldText"].(string), arguments["newText"].(string)
		if err != nil || !strings.Contains(string(content), oldText) {
			return result(execution.CallID, tools.StatusError, "oldText not found")
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(content), oldText, newText, 1)), 0o644); err != nil {
			return result(execution.CallID, tools.StatusError, err.Error())
		}
		return result(execution.CallID, tools.StatusOK, "")
	}
	read := func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
		content, err := os.ReadFile(filepath.Join(execution.WorkspaceRoot, arguments["path"].(string)))
		if err != nil {
			return result(execution.CallID, tools.StatusError, err.Error())
		}
		return result(execution.CallID, tools.StatusOK, string(content))
	}
	schema := json.RawMessage(`{"type":"object"}`)
	return []tools.Tool{
		{Definition: tools.Definition{ID: "test:edit", Source: "native", Name: "edit", InputSchema: schema}, Validator: validator, Execute: edit},
		{Definition: tools.Definition{ID: "test:read", Source: "native", Name: "read", InputSchema: schema}, Validator: validator, ReadOnly: true, Execute: read},
	}
}

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
	catalog, err := tools.NewCatalog(orderingTools(t))
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
		tools.ExecutionContext{WorkspaceRoot: workspace, SessionDir: workspace}, limit, scheduling, nil,
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
// model's order. The removed TypeScript backend (tag legacy-final) had the same test.
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
