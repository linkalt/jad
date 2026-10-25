package native

import (
	"context"
	"sort"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func toolsetAgentChildTools(t *testing.T, listed ...string) ([]string, error) {
	t.Helper()
	pm, err := NewProfileMessageTool(sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024, MaxChain: 3}, profileMessageTargets, &childMessenger{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := append(withExecutors(t, readOnlyTool("read")), pm)
	agent := nestingAgent("finder", listed...)
	agent.Toolset = []string{"extra/search"}
	settings := subagentSettings(agent)
	settings.MaxDepth = 2
	toolsets := map[string]subagentToolset{"extra/search": {native: withExecutors(t, readOnlyTool("grep"))}}
	var got []string
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	launcher := &fakeLauncher{run: func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
		got = toolNames(request.Tools)
		return SubagentResult{Text: "done"}, nil
	}}
	tool, err := newSubagentToolAsync(settings, parent, toolsets, launcher, nil, manager, nil, nil)
	if err != nil {
		return nil, err
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x"}`)
	sort.Strings(got)
	return got, nil
}

func TestToolsetAgentGetsSubagentAndProfileMessageTogetherOrAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		listed []string
		want   string
	}{
		"both":                  {[]string{"grep", "subagent", "profile_message"}, "grep,profile_message,subagent"},
		"subagent alone":        {[]string{"grep", "subagent"}, "grep,subagent"},
		"profile_message alone": {[]string{"grep", "profile_message"}, "grep,profile_message"},
		"neither":               {[]string{"grep"}, "grep"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := toolsetAgentChildTools(t, tc.listed...)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != tc.want {
				t.Fatalf("child tools = %v, want %s", got, tc.want)
			}
		})
	}
}

func TestToolsetAgentCannotListProfileMessageTheSessionDoesNotEnable(t *testing.T) {
	agent := nestingAgent("finder", "grep", "profile_message")
	agent.Toolset = []string{"extra/search"}
	toolsets := map[string]subagentToolset{"extra/search": {native: withExecutors(t, readOnlyTool("grep"))}}
	_, err := newSubagentToolAsync(subagentSettings(agent), withExecutors(t, readOnlyTool("read")), toolsets, &fakeLauncher{}, nil, NewSubagentManager(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "does not enable tools.native.profile_message") {
		t.Fatalf("error = %v, want the session-does-not-enable error", err)
	}
}
