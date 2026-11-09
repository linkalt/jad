package app

import (
	"strings"
	"testing"
)

// A child that lists subagent starts a grandchild through the real launcher. The run
// limits are checked against the session key and the depth the launcher carried into
// the child's execution context, so a launcher that dropped either would let the third
// level start.
func TestNestedSubagentThroughTheRealLauncherCarriesDepthAndSessionKey(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"skill", "subagent"},
		"__subagent_settings": map[string]any{"max_depth": 3, "max_runs_per_session": 2, "max_runs_per_root": 8},
	})
	f.provider.childFirstCall = [2]string{"subagent", `{"task":"go deeper"}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	var all strings.Builder
	for _, request := range f.provider.forModel("child-model") {
		all.WriteString(request.Text)
	}
	if !strings.Contains(all.String(), "subagent_limit_reached") {
		t.Fatalf("no child saw subagent_limit_reached; the third level started or the launcher lost the session key:\n%s", all.String())
	}
}

// A child at the deepest level is not offered the subagent tool (a call to it could only be refused),
// and the level it is at is the one the launcher carried: a launcher that lost AgentDepth would hand the
// grandchild the nesting catalog.
func TestNestedSubagentThroughTheRealLauncherStopsAtMaxDepth(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"skill", "subagent"},
	})
	f.provider.childFirstCall = [2]string{"subagent", `{"task":"go deeper"}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	nesting, leaf := 0, 0
	for _, request := range f.provider.forModel("child-model") {
		hasSubagent := false
		for _, name := range request.Tools {
			hasSubagent = hasSubagent || name == "subagent"
		}
		switch {
		case hasSubagent:
			nesting++
		case strings.Contains(request.Text, "go deeper"):
			leaf++
		}
	}
	if nesting == 0 || leaf == 0 {
		t.Fatalf("child requests with the subagent tool: %d, grandchild requests without it: %d; the launcher lost AgentDepth", nesting, leaf)
	}
}
