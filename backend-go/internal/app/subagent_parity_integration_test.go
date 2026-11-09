package app

import (
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// A subagent is just another profile: the tools a child gets are the tools a profile gets from the same
// tools config. Each case builds the profile twice over the same registry: once with the config as
// the profile's own tools block (its model's first request lists the tools), once as the agent's own tools
// key (the child's first request lists them), and the two lists must be the same.
func TestChildToolsEqualTheToolsOfAProfileBuiltFromTheSameConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		// profile is the tools block of the profile in the first build; agent is the tools key of the agent in the second.
		// An agent's config is the profile's block, except that an array is an allow list.
		profile map[string]any
		agent   any
		mcp     bool
		want    string
	}{
		"no tools config": {nil, nil, false, "async,skill,subagent"},
		"allow list": {
			map[string]any{"allow": []string{"skill", "subagent"}}, []string{"skill", "subagent"}, false, "skill,subagent",
		},
		"allow object": {
			map[string]any{"allow": []string{"async", "subagent"}}, map[string]any{"allow": []string{"async", "subagent"}}, false, "async,subagent",
		},
		"deny list": {
			map[string]any{"deny": []string{"skill"}}, map[string]any{"deny": []string{"skill"}}, false, "async,subagent",
		},
		"mcp tools present": {nil, nil, true, "async,skill,subagent," + mcpToolNameFor("fixture", "lookup")},
		"mcp allow entry": {
			map[string]any{"allow": []string{"subagent", "skill", "mcp:fixture:lookup"}}, []string{"subagent", "skill", "mcp:fixture:lookup"}, true, "skill,subagent," + mcpToolNameFor("fixture", "lookup"),
		},
		"mcp deny entry": {
			map[string]any{"deny": []string{"mcp:fixture:lookup"}}, map[string]any{"deny": []string{"mcp:fixture:lookup"}}, true, "async,skill,subagent",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := func(agent map[string]any, session map[string]any) subagentFixture {
				agent["model"], agent["description"] = "child.json", "Reads files."
				if session != nil {
					agent["__session"] = session
				}
				var mcp *mcpFixture
				if tc.mcp {
					mcp = &mcpFixture{calls: &atomic.Int32{}}
				}
				return buildSubagentFixtureWithMCP(t, agent, false, nil, mcp)
			}
			// The profile's own model: the config is its tools block, the agent has none.
			var session map[string]any
			if tc.profile != nil {
				session = profileSession(tc.profile)
			}
			profile := fixture(map[string]any{}, session)
			profile.run(t)
			parent := profile.provider.forModel("parent-model")
			if len(parent) == 0 {
				t.Fatal("the profile's model made no request")
			}
			// The child: the config is the agent's own tools key, the profile has no tools block.
			agent := map[string]any{}
			if tc.agent != nil {
				agent["tools"] = tc.agent
			}
			child := fixture(agent, nil)
			child.run(t)
			sub := child.provider.forModel("child-model")
			if len(sub) == 0 {
				t.Fatal("the child made no request")
			}
			profileTools, childTools := slices.Clone(parent[0].Tools), slices.Clone(sub[0].Tools)
			slices.Sort(profileTools)
			slices.Sort(childTools)
			wanted := strings.Split(tc.want, ",")
			slices.Sort(wanted)
			if !slices.Equal(profileTools, childTools) || !slices.Equal(childTools, wanted) {
				t.Fatalf("profile tools %v, child tools %v, want both %v", profileTools, childTools, wanted)
			}
		})
	}
}

// The profile's tools.deny (global to its children) takes a tool from a child that has no tools config of
// its own, exactly as it takes it from the profile; the child's tool set is the profile's then.
func TestProfileToolsDenyTakesTheToolFromTheChildToo(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "description": "Reads files.",
		"__session": profileSession(map[string]any{"deny": []string{"skill"}}),
	})
	f.run(t)
	parent, child := f.provider.forModel("parent-model"), f.provider.forModel("child-model")
	if len(parent) == 0 || len(child) == 0 {
		t.Fatalf("requests: parent %d, child %d", len(parent), len(child))
	}
	if strings.Join(parent[0].Tools, ",") != "async,subagent" || strings.Join(child[0].Tools, ",") != "async,subagent" {
		t.Fatalf("parent %v, child %v, want async,subagent for both", parent[0].Tools, child[0].Tools)
	}
}
