package native

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// childCatalogFixture is one subagent tool built the way the resolver builds it (the tool, then
// SetLazy with the owner's lazy set and load_tool, then BindToolsets with the session's and a
// toolset's MCP tools), over agents that cover every input of a child's catalog: nesting,
// load_tool, session and toolset MCP tools, a lazy MCP tool, an agent sandbox, worktree
// isolation, no tools list, and a toolset with profile_message. ownerMarker and toolsetMarker
// count the pre_tool hooks of the owner and of the toolset.
type childCatalogFixture struct {
	// workspace is the calling run's workspace: a git repository for a test, so a run that a choice or
	// its agent puts in a worktree of its own starts and its catalog reaches the launcher.
	workspace                  string
	catalog                    *tools.Catalog
	launcher                   *fakeLauncher
	manager                    *SubagentManager
	ownerMarker, toolsetMarker string
	mcp                        *SubagentMCP
	// builtBefore names the agents that had catalogs before AssembleChildren (assembled fixture).
	builtBefore []string
}

// stubbedTools gives stub tools an executor and a schema, for tests and benchmarks.
func stubbedTools(tb testing.TB, list ...tools.Tool) []tools.Tool {
	tb.Helper()
	validator, err := tools.CompileSchema(json.RawMessage(`{"type":"object"}`))
	if err != nil {
		tb.Fatal(err)
	}
	out := make([]tools.Tool, 0, len(list))
	for _, tool := range list {
		tool.Validator = validator
		tool.Execute = func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "ok", nil }
		out = append(out, tool)
	}
	return out
}

func childCatalogMCP(tb testing.TB, server, name string) tools.Tool {
	return stubbedTools(tb, tools.Tool{Definition: tools.Definition{
		ID: "mcp:" + server + ":" + name, Source: "mcp", Server: server, Name: "mcp_" + server + "_" + name + "_0123456789", Description: name,
	}, ReadOnly: true})[0]
}

// newChildCatalogFixture builds the fixture the way a caller without a catalog assembly does: the
// tool builds its children's catalogs when it is made and again as SetLazy and BindToolsets arrive.
func newChildCatalogFixture(tb testing.TB, workspace string) childCatalogFixture {
	tb.Helper()
	return buildChildCatalogFixture(tb, workspace, false)
}

// newAssembledChildCatalogFixture builds the same fixture the way the resolver's catalog assembly
// does: NewAssembledSubagentMCP, then one AssembleChildren with the copies, the lazy set and the
// load_tool. builtBefore names the agents that had a catalog before AssembleChildren.
func newAssembledChildCatalogFixture(tb testing.TB, workspace string) childCatalogFixture {
	tb.Helper()
	return buildChildCatalogFixture(tb, workspace, true)
}

func buildChildCatalogFixture(tb testing.TB, workspace string, assembled bool) childCatalogFixture {
	tb.Helper()
	dir := tb.TempDir()
	ownerMarker, toolsetMarker := filepath.Join(dir, "owner"), filepath.Join(dir, "toolset")
	ownerHooks := eventSettings([]sessionfs.ToolEventHook{eventHook("owner", "*", "echo fired >> "+ownerMarker)}, nil)
	toolsetHooks := eventSettings([]sessionfs.ToolEventHook{eventHook("toolset", "*", "echo fired >> "+toolsetMarker)}, nil)

	nester := agentNamed("nester")
	nester.Tools, nester.HasTools = []string{"read", "exec", "subagent", "load_tool", "mcp:srv:look", "mcp:srv:slow"}, true
	reader := agentNamed("reader")
	reader.Tools, reader.HasTools = []string{"read"}, true
	boxed := agentNamed("boxed")
	boxed.Tools, boxed.HasTools = []string{"read", "exec", "subagent"}, true
	boxed.Sandbox = bwrapSandbox()
	boxed.Sandbox.Template = "own"
	boxed.Sandbox.WritablePaths, boxed.Sandbox.PlanWritablePaths = []string{"{tmp}"}, []string{"{tmp}"}
	isolated := agentNamed("isolated")
	isolated.Tools, isolated.HasTools = []string{"read", "exec"}, true
	isolated.Isolation = sessionfs.SubagentIsolationWorktree
	finder := agentNamed("finder")
	finder.Toolset = []string{searchToolset}
	finder.Tools, finder.HasTools = []string{"grep", "profile_message", "subagent", "mcp:zvec:search"}, true
	plain := agentNamed("plain")

	settings := choiceSettings(nester, reader, boxed, isolated, finder, plain)
	settings.MaxDepth = 3
	settings.StorageDir = filepath.Join(dir, "runs")

	execTool := tools.Tool{Definition: tools.Definition{ID: execToolID, Source: "native", Name: "exec", Description: "exec."}}
	profileMessage := tools.Tool{Definition: tools.Definition{ID: profileMessageToolID, Source: "native", Name: "profile_message", Description: "profile_message."}, ReadOnly: true}
	parent := stubbedTools(tb, tools.Tool{Definition: tools.Definition{ID: "native.read", Source: "native", Name: "read", Description: "read."}, ReadOnly: true}, execTool, profileMessage)
	toolsetNative := stubbedTools(tb, tools.Tool{Definition: tools.Definition{ID: "native.grep", Source: "native", Name: "grep", Description: "grep."}, ReadOnly: true})

	manager := NewSubagentManager()
	tb.Cleanup(func() { _ = manager.Close() })
	launcher := &fakeLauncher{}
	mcp := NewSubagentMCP(ownerHooks, "")
	if assembled {
		mcp = NewAssembledSubagentMCP(ownerHooks, "")
	}
	tool, err := newSubagentToolDescribed(settings, parent, map[string]string{execToolID: "exec."},
		map[string]subagentToolset{searchToolset: {native: toolsetNative, descriptions: map[string]string{}}}, launcher, mcp, manager, nil, nil)
	if err != nil {
		tb.Fatal(err)
	}
	look, slow := childCatalogMCP(tb, "srv", "look"), childCatalogMCP(tb, "srv", "slow")
	set := tools.NewLazySet([]string{"mcp:srv:slow"}, 256, nil)
	set.Bind([]tools.Tool{look, slow})
	loadTool, err := NewLoadToolTool(set, 16)
	if err != nil {
		tb.Fatal(err)
	}
	toolsetMCP := map[string]ToolsetMCP{searchToolset: {Tools: []tools.Tool{childCatalogMCP(tb, "zvec", "search")}, Hooks: toolsetHooks}}
	var builtBefore []string
	if assembled {
		for _, agent := range mcp.tool.agents {
			if agent.catalogs != nil {
				builtBefore = append(builtBefore, agent.config.Name)
			}
		}
		if err := mcp.AssembleChildren(ChildParts{Lazy: set, LoadTool: &loadTool, MCP: NewMCPCopies([]tools.Tool{look, slow}, toolsetMCP, set, ownerHooks, "")}); err != nil {
			tb.Fatal(err)
		}
	} else {
		if err := mcp.SetLazy(set, &loadTool); err != nil {
			tb.Fatal(err)
		}
		if err := mcp.BindToolsets([]tools.Tool{look, slow}, toolsetMCP); err != nil {
			tb.Fatal(err)
		}
	}
	catalog, err := tools.NewCatalog(append(parent, WithToolEventHooks(tool, ownerHooks, "")))
	if err != nil {
		tb.Fatal(err)
	}
	catalog, err = mcp.Redescribe(catalog)
	if err != nil {
		tb.Fatal(err)
	}
	return childCatalogFixture{workspace: workspace, catalog: catalog, launcher: launcher, manager: manager, ownerMarker: ownerMarker, toolsetMarker: toolsetMarker, mcp: mcp, builtBefore: builtBefore}
}

// call makes one subagent call at depth and returns its output, the catalog the launcher got
// (nil when no run reached it) and the fingerprint the run was recorded with ("" when none).
func (f childCatalogFixture) call(tb testing.TB, catalog *tools.Catalog, arguments string, depth int) (string, *tools.Catalog, string) {
	tb.Helper()
	f.launcher.mu.Lock()
	before := len(f.launcher.requests)
	f.launcher.mu.Unlock()
	known := map[*subagentRun]bool{}
	for _, run := range f.manager.allRuns() {
		known[run] = true
	}
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_9", "name": "subagent", "arguments": arguments})
	execution := nestedExecution(tb, f.manager, tools.ExecutionContext{WorkspaceRoot: f.workspace, SessionDir: "/sess", SessionKey: testSessionKey, AgentDepth: depth})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, execution)
	if err != nil {
		tb.Fatal(err)
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		tb.Fatal(err)
	}
	var child *tools.Catalog
	f.launcher.mu.Lock()
	if len(f.launcher.requests) > before {
		child = f.launcher.requests[len(f.launcher.requests)-1].Tools
	}
	f.launcher.mu.Unlock()
	fingerprint := ""
	for _, run := range f.manager.allRuns() {
		if !known[run] {
			fingerprint = run.config
		}
	}
	return item.Output, child, fingerprint
}

// contentTokenPattern is the random token of a tool result's content block.
var contentTokenPattern = regexp.MustCompile(`token=r_[A-Za-z0-9_-]+`)

// hookTimePattern is the time a hook report gives a hook, which varies from run to run.
var hookTimePattern = regexp.MustCompile(`\(\d+ ms,`)

func shortHash(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:6])
}

// TestChildCatalogsAreUnchanged pins, byte for byte, the catalog every child gets for every
// agent of the fixture, every sandbox choice (none, reader, online, isolate, in-place) and both
// depths (0, where the subagent tool is offered to an agent that nests, and 2 of max_depth 3,
// the deepest level, where it is not): each tool's name, the hash of its route definition and
// its description, plus the run's fingerprint, whether a second identical call got the same
// catalog object, and whether the deepest level got the same object as depth 0. A call that starts
// no run records its result, with the content token and hook times masked. Generated on the pre-R4
// code (choiceCatalogs and the agent's catalog and leaf fields) and passing on HEAD a18f9f47;
// JAD_UPDATE_RUN_BASELINE=1 rewrites it.
// childCatalogBuilds are the two ways a subagent tool's child catalogs are built: by the tool as its
// inputs arrive, and once by a catalog assembly. Both must give every catalog the same.
var childCatalogBuilds = []struct {
	name  string
	build func(testing.TB, string) childCatalogFixture
}{{"incremental", newChildCatalogFixture}, {"assembled", newAssembledChildCatalogFixture}}

func TestChildCatalogsAreUnchanged(t *testing.T) {
	for _, build := range childCatalogBuilds {
		t.Run(build.name, func(t *testing.T) { checkChildCatalogsAreUnchanged(t, build.build(t, gitRepository(t))) })
	}
}

func checkChildCatalogsAreUnchanged(t *testing.T, f childCatalogFixture) {
	var out strings.Builder
	for _, agent := range []string{"nester", "reader", "boxed", "isolated", "finder", "plain"} {
		for _, choice := range []string{"", "reader", "online", "isolate", "in-place"} {
			arguments := `{"task":"x","agent":"` + agent + `"`
			if choice != "" {
				arguments += `,"sandbox":"` + choice + `"`
			}
			arguments += `}`
			var depthZero *tools.Catalog
			for _, depth := range []int{0, 2} {
				output, child, fingerprint := f.call(t, f.catalog, arguments, depth)
				_, again, againFingerprint := f.call(t, f.catalog, arguments, depth)
				fmt.Fprintf(&out, "== agent=%s choice=%q depth=%d\n", agent, choice, depth)
				fmt.Fprintf(&out, "fingerprint=%s repeat_same_fingerprint=%v\n", fingerprint, fingerprint == againFingerprint)
				if child == nil {
					fmt.Fprintf(&out, "no run: %s\n", hookTimePattern.ReplaceAllString(contentTokenPattern.ReplaceAllString(output, "token=<token>"), "(<n> ms,"))
					continue
				}
				fmt.Fprintf(&out, "repeat_same_catalog=%v", again == child)
				if depth == 0 {
					depthZero = child
					out.WriteString("\n")
				} else {
					fmt.Fprintf(&out, " same_as_depth0=%v\n", child == depthZero)
				}
				routes, err := json.Marshal(child.RouteDefinitions())
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&out, "routes=%s\n", shortHash(routes))
				for _, definition := range child.Definitions() {
					encoded, _ := json.Marshal(definition)
					fmt.Fprintf(&out, "  %s %s %q\n", definition.Name, shortHash(encoded), definition.Description)
				}
			}
		}
	}
	compareChildCatalogGolden(t, "child_catalogs.txt", out.String())
}

func compareChildCatalogGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("JAD_UPDATE_RUN_BASELINE") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (JAD_UPDATE_RUN_BASELINE=1 writes it)", path, err)
	}
	if string(want) != got {
		gotPath := filepath.Join(t.TempDir(), name)
		_ = os.WriteFile(gotPath, []byte(got), 0o644)
		t.Fatalf("%s changed; got written to %s", path, gotPath)
	}
}

// TestChildCatalogHookOwnersAreKept: in every catalog variant a child gets (none, a sandbox choice,
// the deepest level), its nested subagent tool and its session MCP tools fire the owner's
// pre_tool hooks, and a toolset agent's MCP tool fires its toolset's hooks and never the owner's.
func TestChildCatalogHookOwnersAreKept(t *testing.T) {
	for _, build := range childCatalogBuilds {
		t.Run(build.name, func(t *testing.T) { checkChildCatalogHookOwnersAreKept(t, build.build(t, "/work")) })
	}
}

func checkChildCatalogHookOwnersAreKept(t *testing.T, f childCatalogFixture) {
	type step struct {
		agent, choice string
		depth         int
		tool          string
		owner, set    int
	}
	steps := []step{
		{"nester", "", 0, "mcp_srv_look_0123456789", 1, 0},
		{"nester", "", 0, "subagent", 1, 0},
		{"nester", "reader", 0, "mcp_srv_look_0123456789", 1, 0},
		{"nester", "reader", 0, "subagent", 1, 0},
		{"nester", "", 2, "mcp_srv_look_0123456789", 1, 0},
		{"finder", "", 0, "mcp_zvec_search_0123456789", 0, 1},
		{"finder", "", 0, "subagent", 1, 0},
		{"finder", "online", 0, "mcp_zvec_search_0123456789", 0, 1},
		{"finder", "online", 0, "subagent", 1, 0},
		{"finder", "", 2, "mcp_zvec_search_0123456789", 0, 1},
	}
	for _, s := range steps {
		arguments := `{"task":"x","agent":"` + s.agent + `"`
		if s.choice != "" {
			arguments += `,"sandbox":"` + s.choice + `"`
		}
		_, child, _ := f.call(t, f.catalog, arguments+`}`, s.depth)
		if child == nil {
			t.Fatalf("%+v: no run", s)
		}
		owner, set := countLines(t, f.ownerMarker), countLines(t, f.toolsetMarker)
		callArguments := `{}`
		if s.tool == "subagent" {
			callArguments = `{"task":"y","agent":"reader"}`
		}
		call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_c", "name": s.tool, "arguments": callArguments})
		if _, err := child.ExecuteFunctionCall(context.Background(), call, nestedExecution(t, f.manager, tools.ExecutionContext{SessionKey: testSessionKey, AgentDepth: s.depth + 1})); err != nil {
			t.Fatalf("%+v: %v", s, err)
		}
		if gotOwner, gotSet := countLines(t, f.ownerMarker)-owner, countLines(t, f.toolsetMarker)-set; gotOwner != s.owner || gotSet != s.set {
			t.Errorf("%+v: owner hooks fired %d, toolset hooks %d", s, gotOwner, gotSet)
		}
	}
}

// BenchmarkSubagentToolPrepare is the catalog cost a prepare pays for the subagent tool: build
// it, give it the lazy set and load_tool, bind the MCP tools.
func BenchmarkSubagentToolPrepare(b *testing.B) {
	for _, build := range childCatalogBuilds {
		b.Run(build.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				build.build(b, "/work")
			}
		})
	}
}

// BenchmarkSubagentChoiceCall is one call with a sandbox choice whose catalog is already built.
func BenchmarkSubagentChoiceCall(b *testing.B) {
	f := newChildCatalogFixture(b, "/work")
	f.call(b, f.catalog, `{"task":"x","agent":"nester","sandbox":"reader"}`, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		f.call(b, f.catalog, `{"task":"x","agent":"nester","sandbox":"reader"}`, 0)
	}
}
