package app

import (
	"strings"
	"testing"
)

// A child's spend counts toward the calling run's max_run_cost_usd: after one child
// spends past the cap, the parent's next call is not started.
func TestSubagentSpendCountsTowardTheCallersCostCap(t *testing.T) {
	price := map[string]any{"input_per_mtok": 100_000, "output_per_mtok": 100_000}
	f := newSubagentFixture(t, map[string]any{
		"description":    "Finds facts.",
		"model":          "child.json",
		"__child_model":  map[string]any{"pricing": price},
		"__parent_model": map[string]any{"pricing": map[string]any{"input_per_mtok": 0, "output_per_mtok": 0}},
		"__session":      map[string]any{"max_run_cost_usd": 1},
	})
	f.provider.parentSteps = []subagentStep{
		{"subagent", `{"task":"find the answer"}`},
		{"subagent", `{"task":"find another answer"}`},
	}
	body := f.run(t)
	first, second := false, false
	for _, request := range f.provider.forModel("child-model") {
		first = first || strings.Contains(request.Text, "find the answer")
		second = second || strings.Contains(request.Text, "find another answer")
	}
	if !first || second {
		t.Fatalf("first child ran = %v, second child ran = %v; want only the first: it spent past the cap", first, second)
	}
	if !strings.Contains(body, "reached the max_run_cost_usd limit") {
		t.Fatalf("the run did not stop at the cost cap:\n%s", body)
	}
}
