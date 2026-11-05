package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/tools"
)

// profileMessageDeadlockFixture is a session whose default profile can call profile_message,
// with the leaf budget at one. The parent's first model response calls profile_message, so the
// call runs in the parent's batch through the real HTTP run.
func profileMessageDeadlockFixture(t *testing.T, toolset string) *layoutFixture {
	t.Helper()
	f := newLayoutFixtureWithBudget(t, nil, tools.NewLeafBudget(1))
	f.overwrite(t, "tools/coding/none.json", toolset)
	if err := os.WriteFile(filepath.Join(f.cwd, "notes.txt"), []byte("the secret is 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.captureBodies = true
	return f
}

// requireParentReceivedProfileMessageOutput fails unless the run completed and the parent's
// last request carries the profile_message output, so the nested call really ran in the batch.
func requireParentReceivedProfileMessageOutput(t *testing.T, f *layoutFixture, body, want string) {
	t.Helper()
	if !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	// The parent's request that carries the call's output is the one a handled message
	// does not answer again; a request of the target profile may come after it.
	// Only the output items are searched: a request body also carries the tool definitions,
	// whose text could hold want.
	f.mu.Lock()
	defer f.mu.Unlock()
	var outputs []string
	for _, body := range f.bodies {
		var request struct {
			Input []struct {
				Type   string `json:"type"`
				Output string `json:"output"`
			} `json:"input"`
		}
		if json.Unmarshal([]byte(body), &request) != nil {
			continue
		}
		for _, item := range request.Input {
			if item.Type == "function_call_output" {
				if strings.Contains(item.Output, want) {
					return
				}
				outputs = append(outputs, item.Output)
			}
		}
	}
	t.Fatalf("no request carries the profile_message output %q; outputs:\n%s", want, strings.Join(outputs, "\n---\n"))
}

// targetReadsThenAnswers scripts the target profile (model-large) to call read on notes.txt
// before it answers, and the parent (model-small) to call profile_message on the target.
// The target's read is a leaf that needs a token while the parent's call waits on the target.
func targetReadsThenAnswers(f *layoutFixture, message map[string]any) {
	call := scriptFunctionCall("profile_message", message)
	read := scriptFunctionCall("skill", map[string]any{"name": "notes"})
	f.script = func(model, body string) string {
		if model == "model-large" {
			if strings.Contains(body, "function_call_output") {
				return ""
			}
			return read(model, body)
		}
		return call(model, body)
	}
}

// TestDeadlockProfileMessageNotHandledFinishesWithBudgetOne: no async tool, so the call waits
// for the target's answer inside the parent's call and gets it inline.
func TestDeadlockProfileMessageNotHandledFinishesWithBudgetOne(t *testing.T) {
	f := profileMessageDeadlockFixture(t, `{"native":{"profile_message":{},"skill":{}},"mcp":{}}`)
	targetReadsThenAnswers(f, map[string]any{"profile": "review/reviewer", "message": "first", "block_ms": 0})
	requireParentReceivedProfileMessageOutput(t, f, postRunWithDeadlockTimeout(t, f.api.URL), "Profile review/reviewer answered")
	requireTargetReadTheNotes(t, f)
}

// requireTargetReadTheNotes fails unless the target profile's read of notes.txt reached the
// target's next round, so the nested leaf really ran through the budget.
// A handled message runs the target in the background, so its read may land after the
// parent's call returns: the check waits for it up to the deadline.
func requireTargetReadTheNotes(t *testing.T, f *layoutFixture) {
	t.Helper()
	deadline := time.Now().Add(deadlockTimeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, body := range f.bodies {
			if strings.Contains(body, `"model":"model-large"`) && strings.Contains(body, "the secret is 42") {
				f.mu.Unlock()
				return
			}
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the target's read of notes.txt never reached the target: its leaf did not run")
}

// TestDeadlockProfileMessageHandledFinishesWithBudgetOne: the parent's async tool is enabled, so
// the message is a handle and the target runs in the background with the budget at one.
// The parent's call takes no token that the target needs: with block_ms 0 it returns the handle
// at once, and the target's read is the only leaf that waits for the budget. Setting LeafBudget
// on profile_message therefore does not deadlock this path, so this test is a completion check
// only; the structural guard in internal/tools/native is what keeps the wrapper off the budget.
func TestDeadlockProfileMessageHandledFinishesWithBudgetOne(t *testing.T) {
	f := profileMessageDeadlockFixture(t, `{"native":{"profile_message":{},"skill":{},"async":{}},"mcp":{}}`)
	targetReadsThenAnswers(f, map[string]any{"profile": "review/reviewer", "message": "first", "block_ms": 0})
	requireParentReceivedProfileMessageOutput(t, f, postRunWithDeadlockTimeout(t, f.api.URL), "use the async tool with id pm_")
	requireTargetReadTheNotes(t, f)
}
