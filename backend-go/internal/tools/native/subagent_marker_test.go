package native

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const (
	writerRunsToEnd = " [can change files; runs to its end]"
	worktreeMarker  = " [can change files in a git worktree of its own; its changes come back as a branch]"
)

// listedAs is the marker the agent list shows after name in description ("" for none), and whether the
// agent is listed at all.
func listedAs(description, name string) (string, bool) {
	for _, line := range strings.Split(description, "\n") {
		if rest, ok := strings.CutPrefix(line, "- "+name); ok && (rest == "" || strings.HasPrefix(rest, " [") || strings.HasPrefix(rest, ":")) {
			marker, _, _ := strings.Cut(rest, ":")
			return marker, true
		}
	}
	return "", false
}

func subagentDescriptionOf(t *testing.T, catalog *tools.Catalog) string {
	t.Helper()
	for _, definition := range catalog.Definitions() {
		if definition.ID == subagentToolID {
			return definition.Description
		}
	}
	t.Fatal("the catalog has no subagent tool")
	return ""
}

// The agent list marks an agent as the tool classifies its calls: an MCP tool that may write, or a
// path to a writer through nesting, is a writer whatever the agent's native tools are. The MCP tools
// are known only after Bind, so the owner describes the tool again (Redescribe).
func TestAgentListMarksMCPAndNestingWritersLikeTheClassification(t *testing.T) {
	f := newPlanMCPFixture(t)
	before := subagentDescriptionOf(t, f.catalog)
	if marker, _ := listedAs(before, "mcponly"); marker != "" {
		t.Fatalf("the description built before Bind cannot know the MCP tools, yet marks mcponly %q", marker)
	}
	catalog, err := f.hook.Redescribe(f.catalog)
	if err != nil {
		t.Fatal(err)
	}
	after := subagentDescriptionOf(t, catalog)
	for name, want := range map[string]string{
		"mcponly": writerRunsToEnd, // an MCP tool that may write
		"plain":   writerRunsToEnd, // no list: every tool, so the same MCP tool
		"nester":  writerRunsToEnd, // a reader of its own that can start those
		"fixer":   writerRunsToEnd, // exec
		"mcpread": "",              // a ReadOnly MCP tool only
	} {
		got, listed := listedAs(after, name)
		if !listed || got != want {
			t.Errorf("agent %s is marked %q (listed %v), want %q:\n%s", name, got, listed, want, after)
		}
	}
	// What the model is told matches what scheduling does.
	for _, name := range []string{"mcponly", "plain", "nester", "fixer", "mcpread"} {
		marker, _ := listedAs(after, name)
		writes := f.catalog.ChangesState(subagentFunctionCall(`{"task":"x","agent":"` + name + `"}`))
		if (marker != "") != writes {
			t.Errorf("agent %s: marker %q but ChangesState %v", name, marker, writes)
		}
	}
	// The original catalog is not changed.
	if got := subagentDescriptionOf(t, f.catalog); got != before {
		t.Error("Redescribe changed the catalog it was given")
	}
	// A call with no task lists the agents with the same markers.
	list := callOutput(t, catalog, subagentFunctionCall(`{}`), tools.ExecutionContext{CallID: "list"})
	for _, line := range []string{"mcponly" + writerRunsToEnd, "nester" + writerRunsToEnd, "fixer" + writerRunsToEnd} {
		if !strings.Contains(list, line) {
			t.Errorf("the agent list lacks %q:\n%s", line, list)
		}
	}
	if strings.Contains(list, "mcpread [") {
		t.Errorf("a reader is marked in the agent list:\n%s", list)
	}
}

// A nested child sees the same description, so the marker does not depend on who reads it.
func TestNestedSubagentToolCarriesTheBoundDescription(t *testing.T) {
	f := newPlanMCPFixture(t)
	call := subagentFunctionCall(`{"task":"x","agent":"nester"}`)
	callOutput(t, f.catalog, call, tools.ExecutionContext{CallID: "c"})
	requests := f.launcher.requests
	if len(requests) == 0 {
		t.Fatal("no child started")
	}
	var seen string
	for _, definition := range requests[len(requests)-1].Tools.Definitions() {
		if definition.ID == subagentToolID {
			seen = definition.Description
		}
	}
	if marker, _ := listedAs(seen, "mcponly"); marker != writerRunsToEnd {
		t.Fatalf("the subagent tool a child gets marks mcponly %q:\n%s", marker, seen)
	}
}

// Nesting alone does not make a marker: a nesting agent with only readers to reach, or with no room to
// nest, is a reader, and so is its marker. An isolated agent keeps its worktree marker unless an MCP
// tool or a nested writer can reach the session's own folder.
func TestAgentListMarkersFollowTheClassificationForReadersAndIsolation(t *testing.T) {
	describe := func(settings sessionfs.NativeSubagentSettings, mcp ...tools.Tool) string {
		t.Helper()
		hook := &SubagentMCP{}
		parent := withExecutors(t, readOnlyTool("read"), writeTool("exec"))
		tool, err := NewSubagentToolWithMCP(settings, parent, &fakeLauncher{}, hook)
		if err != nil {
			t.Fatal(err)
		}
		if err := hook.Bind(mcp); err != nil {
			t.Fatal(err)
		}
		catalog, err := tools.NewCatalog(append(parent, tool))
		if err != nil {
			t.Fatal(err)
		}
		if catalog, err = hook.Redescribe(catalog); err != nil {
			t.Fatal(err)
		}
		return subagentDescriptionOf(t, catalog)
	}
	assertMarker := func(description, name, want string) {
		t.Helper()
		if got, listed := listedAs(description, name); !listed || got != want {
			t.Errorf("agent %s is marked %q (listed %v), want %q:\n%s", name, got, listed, want, description)
		}
	}
	nester, reader := agentWithTools("nester", "read", "subagent"), agentWithTools("reader", "read")
	readers := subagentSettings(nester, reader)
	readers.MaxDepth = 3
	description := describe(readers)
	assertMarker(description, "nester", "")
	assertMarker(description, "reader", "")

	flat := subagentSettings(nester, agentWithTools("fixer", "read", "exec"))
	flat.MaxDepth = 1
	description = describe(flat)
	assertMarker(description, "nester", "") // a writer exists but nothing can nest
	assertMarker(description, "fixer", writerRunsToEnd)

	// An isolated native writer stays in its worktree; an isolated reader that nests is a writer of
	// the session's folder when a writer is reachable.
	isolated := isolatedAgent("isolated")
	isolatedNester := agentWithTools("isonest", "read", "subagent")
	isolatedNester.Isolation = sessionfs.SubagentIsolationWorktree
	deep := subagentSettings(isolated, isolatedNester, agentWithTools("fixer", "read", "exec"))
	deep.MaxDepth = 3
	description = describe(deep)
	assertMarker(description, "isolated", worktreeMarker)
	assertMarker(description, "isonest", writerRunsToEnd)

	// With no writer outside the worktree, the isolated agent is only a worktree writer.
	only := subagentSettings(isolated, isolatedNester)
	only.MaxDepth = 3
	description = describe(only)
	assertMarker(description, "isolated", worktreeMarker)
	assertMarker(description, "isonest", "")
}

// A remote whose server runs on this machine is described as one that runs to its end; one of another
// machine is not named.
func TestRemoteListNamesTheRemotesOfThisMachine(t *testing.T) {
	f := newRemoteSubagentFixtureWith(t, func(peerURL string) map[string]string {
		return map[string]string{"near": peerURL, "far": "http://192.0.2.7:8080"}
	}, func(s *sessionfs.NativeSubagentSettings) {
		s.Remotes = []string{"peer", "near", "far"}
	})
	// The description is built when the tool is, so build it again with the entries in place.
	settings := subagentSettings(agentNamed("explore"))
	settings.Remotes, settings.RemoteTimeoutMs = []string{"peer", "near", "far"}, 20_000
	tool, err := newSubagentToolRemotes(settings, withExecutors(t, readOnlyTool("read")), nil, f.local, nil, NewSubagentManager(), nil, f.peer.directory(f.hub))
	if err != nil {
		t.Fatal(err)
	}
	text := tool.Definition.Description
	_, after, _ := strings.Cut(text, "runs on this machine (")
	if names, _, _ := strings.Cut(after, ")"); names != "near" {
		t.Fatalf("the remotes described as this machine's are %q, want near only:\n%s", names, text)
	}
	if !strings.Contains(text, ") is [can change files; runs to its end]: it is never handed off") {
		t.Fatalf("the remote of this machine is not described as running to its end:\n%s", text)
	}
	// No remote of this machine, no sentence.
	settings.Remotes = []string{"peer", "far"}
	tool, err = newSubagentToolRemotes(settings, withExecutors(t, readOnlyTool("read")), nil, f.local, nil, NewSubagentManager(), nil, f.peer.directory(f.hub))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tool.Definition.Description, "runs on this machine") {
		t.Fatalf("no remote runs on this machine:\n%s", tool.Definition.Description)
	}
}
