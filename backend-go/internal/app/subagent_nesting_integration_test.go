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

func TestNestedSubagentThroughTheRealLauncherStopsAtMaxDepth(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"skill", "subagent"},
	})
	f.provider.childFirstCall = [2]string{"subagent", `{"task":"go deeper"}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	var all strings.Builder
	for _, request := range f.provider.forModel("child-model") {
		all.WriteString(request.Text)
	}
	if !strings.Contains(all.String(), "subagent_depth_exceeded") {
		t.Fatalf("the grandchild did not hit the depth cap; the launcher lost AgentDepth:\n%s", all.String())
	}
}
