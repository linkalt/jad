package app

import (
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
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, body := range f.bodies {
		if strings.Contains(body, `"type":"function_call_output"`) && strings.Contains(body, want) {
			return
		}
	}
	t.Fatalf("no request carries the profile_message output %q", want)
}

// targetReadsThenAnswers scripts the target profile (model-large) to call read on notes.txt
// before it answers, and the parent (model-small) to call profile_message on the target.
// The target's read is a leaf that needs a token while the parent's call waits on the target.
func targetReadsThenAnswers(f *layoutFixture, message map[string]any) {
	call := scriptFunctionCall("profile_message", message)
	read := scriptFunctionCall("read", map[string]any{"path": "notes.txt"})
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
	f := profileMessageDeadlockFixture(t, `{"native":{"profile_message":{},"read":{}},"mcp":{}}`)
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
