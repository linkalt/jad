package native

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// sectionNames lists the names of tools, joined with commas.
func sectionNames(list []tools.Tool) string {
	names := make([]string, len(list))
	for index, tool := range list {
		names[index] = tool.Definition.Name
	}
	return strings.Join(names, ",")
}

// catalogNames lists the names of a catalog's tools in its order, joined with commas.
func catalogNames(catalog *tools.Catalog) string {
	return strings.Join(toolNames(catalog), ",")
}

// The one order of every agent catalog: native tools, subagent, profile_message, load_tool, MCP
// tools; a section left empty is skipped, and Compose builds the catalog of exactly that list.
func TestCatalogSectionsHaveOneOrder(t *testing.T) {
	named := func(name string) *tools.Tool {
		tool := withExecutors(t, readOnlyTool(name))[0]
		return &tool
	}
	full := CatalogSections{
		Native: []tools.Tool{*named("read"), *named("grep")}, Subagent: named("subagent"), ProfileMessage: named("profile_message"),
		LoadTool: named("load_tool"), MCP: []tools.Tool{*named("mcp_a"), *named("mcp_b")},
	}
	if got := sectionNames(full.Tools()); got != "read,grep,subagent,profile_message,load_tool,mcp_a,mcp_b" {
		t.Fatalf("full sections = %s", got)
	}
	child := CatalogSections{Native: []tools.Tool{*named("read"), *named("profile_message")}, LoadTool: named("load_tool"), MCP: []tools.Tool{*named("mcp_a")}}
	if got := sectionNames(child.Tools()); got != "read,profile_message,load_tool,mcp_a" {
		t.Fatalf("leaf child sections = %s", got)
	}
	catalog, err := full.Compose()
	if err != nil {
		t.Fatal(err)
	}
	if got := catalogNames(catalog); got != sectionNames(full.Tools()) {
		t.Fatalf("composed %s, want %s", got, sectionNames(full.Tools()))
	}
	if empty, err := (CatalogSections{}).Compose(); err != nil || len(empty.Definitions()) != 0 {
		t.Fatalf("empty sections composed %v, %v", empty, err)
	}
	full.MCP = append(full.MCP, *named("read"))
	if _, err := full.Compose(); err == nil || !strings.Contains(err.Error(), "duplicate tool name read") {
		t.Fatalf("a name in two sections composed: %v", err)
	}
}

// sectionsConfig is an owner with read_image, a nesting agent and load_tool, as a resolver build
// loads it; its lazy set binds nothing native, so every tool is eager.
func sectionsConfig(t *testing.T) sessionfs.SessionConfig {
	t.Helper()
	sample := filepath.Join("..", "..", "..", "..", "sample", "tools", "native")
	explore := nestingAgent("explore", "read_image", "subagent", "load_tool", "mcp:docs:look")
	plain := nestingAgent("plain", "read_image", "mcp:docs:look")
	settings := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 2, Agents: []sessionfs.NativeSubagentAgent{explore, plain}}
	return sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join(sample, "read_image.json"), filepath.Join(sample, "subagent.json"), filepath.Join(sample, "load_tool.json")},
		NativeReadImage:     &sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20},
		NativeSubagent:      &settings,
		NativeLoadTool:      &sessionfs.NativeLoadToolSettings{},
		Profile:             "main",
		Profiles:            []string{"main"},
	}
}

// An owner composed from its sections the way the resolver's assembly composes it, and its
// children assembled from the same parts, share their tools: a child that shows load_tool holds
// the owner's very load_tool (no second tool, no second read of its manifest), and every child
// holds the owner's MCP copy. The owner's composed catalog is its native sections then its MCP
// section with the subagent tool redescribed, which is what BuildCatalogWithAsync plus
// Redescribe and a merge with the MCP catalog gave; and the native sections composed alone equal
// BuildCatalogWithAsync's catalog for the callers that build no MCP section.
func TestOwnerAndChildCatalogsAreComposedFromSharedSections(t *testing.T) {
	config := sectionsConfig(t)
	set := tools.NewLazySet(nil, 256, nil)
	handle := NewAssembledSubagentMCP(sessionfs.HooksSettings{}, "")
	sections, err := BuildCatalogSections(config, nil, nil, nil, &fakeLauncher{}, handle, nil, nil, nil, nil, set)
	if err != nil {
		t.Fatalf("BuildCatalogSections: %v", err)
	}
	if sections.LoadTool == nil || sections.Subagent == nil || sections.ProfileMessage != nil || len(sections.MCP) != 0 {
		t.Fatalf("native sections: load_tool %v, subagent %v, profile_message %v, %d MCP tools", sections.LoadTool != nil, sections.Subagent != nil, sections.ProfileMessage != nil, len(sections.MCP))
	}
	whole, err := BuildCatalogWithAsync(config, nil, nil, nil, &fakeLauncher{}, NewSubagentMCP(sessionfs.HooksSettings{}, ""), nil, nil, nil, nil, tools.NewLazySet(nil, 256, nil))
	if err != nil {
		t.Fatal(err)
	}
	native, err := sections.Compose()
	if err != nil {
		t.Fatal(err)
	}
	nativeJSON, _ := json.Marshal(native.Definitions())
	wholeJSON, _ := json.Marshal(whole.Definitions())
	if string(nativeJSON) != string(wholeJSON) {
		t.Fatalf("native sections composed:\n%s\nBuildCatalogWithAsync:\n%s", nativeJSON, wholeJSON)
	}

	look := mcpStub(t, "docs", "look")
	copies := NewMCPCopies([]tools.Tool{look}, nil, set, sessionfs.HooksSettings{}, "")
	owner := sections
	owner.MCP = copies.Session()
	if err := handle.AssembleChildren(ChildParts{Lazy: set, LoadTool: owner.LoadTool, MCP: copies}); err != nil {
		t.Fatal(err)
	}
	composed, err := handle.RedescribeSections(owner).Compose()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := handle.Redescribe(native)
	if err != nil {
		t.Fatal(err)
	}
	mcpCatalog, err := tools.NewCatalog(copies.Session())
	if err != nil {
		t.Fatal(err)
	}
	if merged, err = tools.MergeCatalogs(merged, mcpCatalog); err != nil {
		t.Fatal(err)
	}
	composedJSON, _ := json.Marshal(composed.Definitions())
	mergedJSON, _ := json.Marshal(merged.Definitions())
	if string(composedJSON) != string(mergedJSON) {
		t.Fatalf("owner composed once:\n%s\nnative, redescribed, merged with MCP:\n%s", composedJSON, mergedJSON)
	}
	if got := catalogNames(composed); got != "read_image,subagent,load_tool,"+look.Definition.Name {
		t.Fatalf("owner order = %s", got)
	}
	if description := composed.Definitions()[1].Description; description != handle.tool.description() {
		t.Fatalf("owner subagent description = %q, want the description after binding %q", description, handle.tool.description())
	}

	ownerLoad := closureOf(*owner.LoadTool)
	ownerLook := closureOf(owner.MCP[0])
	wantOrder := map[string]map[bool]string{
		"explore": {false: "read_image,subagent,load_tool," + look.Definition.Name, true: "read_image,load_tool," + look.Definition.Name},
		// plain does not list subagent, so it does not nest: one catalog, without it, at every depth.
		"plain": {false: "read_image," + look.Definition.Name, true: "read_image," + look.Definition.Name},
	}
	for _, agent := range handle.tool.agents {
		for _, leaf := range []bool{false, true} {
			child, err := handle.tool.childCatalog(agent, "", leaf)
			if err != nil || child == nil {
				t.Fatalf("%s leaf=%v: %v", agent.config.Name, leaf, err)
			}
			if got := catalogNames(child); got != wantOrder[agent.config.Name][leaf] {
				t.Errorf("%s leaf=%v order = %s, want %s", agent.config.Name, leaf, got, wantOrder[agent.config.Name][leaf])
			}
			childSections := handle.tool.childCatalogSections(agent, childCatalogKey{leaf: leaf || !agent.nests})
			if got := sectionNames(childSections.Tools()); got != catalogNames(child) {
				t.Errorf("%s leaf=%v: sections %s, catalog %s", agent.config.Name, leaf, got, catalogNames(child))
			}
			if childSections.LoadTool != nil && closureOf(*childSections.LoadTool) != ownerLoad {
				t.Errorf("%s leaf=%v holds a load_tool of its own, not the owner's", agent.config.Name, leaf)
			}
			if len(childSections.MCP) != 1 || closureOf(childSections.MCP[0]) != ownerLook {
				t.Errorf("%s leaf=%v does not hold the owner's MCP copy", agent.config.Name, leaf)
			}
		}
	}
}
