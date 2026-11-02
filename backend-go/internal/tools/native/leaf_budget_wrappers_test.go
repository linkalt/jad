package native

import (
	"context"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// TestWrappersDoNotOptIntoTheLeafBudget fails if a tool that waits on a nested run sets
// LeafBudget. Such a call holds a token while the nested run needs one, which is the deadlock
// the budget must not cause. Each wrapper is built with its real constructor.
//
// This is the only check on the opt-in for profile_message (handled and remote) and tool_smith.
// Their completion tests pass whether or not the wrapper opts in, so the mutation cannot catch
// them. For tool_smith the argument is structural: its bridge reaches the catalog only through
// ExecuteFunctionCall, which never takes a token (the budget is taken only in
// session.runToolBatchLeaves), and no runToolBatch call is reachable from the tool_smith files.
// That argument depends on ExecuteFunctionCall not touching the budget, so a change there must be
// caught by review. Known gap: a synchronous tool_smith script can still fan out nested leaf calls
// with no bound, the unbounded nested fan-out accepted for this round.
func TestWrappersDoNotOptIntoTheLeafBudget(t *testing.T) {
	host, err := NewAsyncHost(filepath.Join(t.TempDir(), ".async"), asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	subagent, err := NewSubagentToolWithManager(subagentSettings(agentNamed("explore")), withExecutors(t, readOnlyTool("read")), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	profileMessage, err := NewProfileMessageTool(sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024, MaxQueued: 4}, []string{"review/reviewer"}, nopProfileMessenger{}, host)
	if err != nil {
		t.Fatal(err)
	}
	toolSmith, err := NewToolSmithTool(sessionfs.NativeToolSmithSettings{}, "", nil, host)
	if err != nil {
		t.Fatal(err)
	}
	async, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	execTool, err := NewExecTool(sessionfs.NativeExecSettings{}, "", host)
	if err != nil {
		t.Fatal(err)
	}
	for _, wrapper := range []struct {
		name string
		tool tools.Tool
	}{
		{"subagent", subagent},
		{"profile_message", profileMessage},
		{"tool_smith", toolSmith},
		{"async", async},
		{"exec", execTool},
	} {
		if wrapper.tool.LeafBudget {
			t.Errorf("%s sets LeafBudget: it waits on a nested run, so it must not hold a leaf token", wrapper.name)
		}
	}
}

// nopProfileMessenger is a messenger the guard never calls: the guard only builds the tool.
type nopProfileMessenger struct{}

func (nopProfileMessenger) Prepare(_ context.Context, _ ProfileMessageRequest) (ProfileMessagePlan, error) {
	return nil, nil
}
