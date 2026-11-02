package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// deadlockTimeout bounds each nesting-path run. A wrapper that holds the only leaf token
// while its nested leaf waits for one never completes; the request is cancelled at this
// bound, so the test fails with its own message and the fixture can shut down.
const deadlockTimeout = 20 * time.Second

// postRunWithDeadlockTimeout posts a session run to baseURL under a deadline, and fails the
// test with a deadlock message when the run does not return in time.
func postRunWithDeadlockTimeout(t *testing.T, baseURL string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadlockTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"go"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if ctx.Err() != nil {
		t.Fatalf("the run did not finish within %s with a leaf budget of one: a wrapper holds the token its nested leaf needs", deadlockTimeout)
	}
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestDeadlockSubagentWithHandleFinishesWithBudgetOne: the parent's batch runs a subagent
// call that gets an async handle; the child's read is a leaf that needs the only token.
func TestDeadlockSubagentWithHandleFinishesWithBudgetOne(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"read", "subagent", "async"}, "__leaf_budget": 1,
	})
	f.provider.parentFirstCall = [2]string{"subagent", `{"task":"read it"}`}
	f.provider.childFirstCall = [2]string{"read", `{"path":"notes.txt"}`}
	requireChildReadTheNotes(t, f, postRunWithDeadlockTimeout(t, f.apiURL))
}

// TestDeadlockSubagentWithoutHandleFinishesWithBudgetOne: the same call without the async
// tool, so the subagent runs to its end inside the parent's call and has no handle.
func TestDeadlockSubagentWithoutHandleFinishesWithBudgetOne(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"read", "subagent"}, "__leaf_budget": 1,
	})
	f.provider.parentFirstCall = [2]string{"subagent", `{"task":"read it"}`}
	f.provider.childFirstCall = [2]string{"read", `{"path":"notes.txt"}`}
	requireChildReadTheNotes(t, f, postRunWithDeadlockTimeout(t, f.apiURL))
}

// requireChildReadTheNotes fails unless the run completed and the child's read of notes.txt
// reached the child's next round, so the nested leaf really ran through the budget.
func requireChildReadTheNotes(t *testing.T, f subagentFixture, body string) {
	t.Helper()
	if !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	var child strings.Builder
	for _, request := range f.provider.forModel("child-model") {
		child.WriteString(request.Text)
	}
	if !strings.Contains(child.String(), "the secret is 42") {
		t.Fatalf("the child's read of notes.txt never reached it:\n%s", child.String())
	}
}
