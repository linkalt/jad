package native

import (
	"fmt"
	"sync"

	"jad/backend-go/internal/tools"
)

// A child's catalog is a function of the agent's resolved tools (its native tools, which the agent's
// own sandbox and isolation describe, the MCP tools bindMCP gave it, the owner's load_tool and the
// owner's hooked subagent tool), the sandbox choice of the call, and whether the child is at the
// deepest level, where it gets no subagent tool. childCatalogSet is the one place those catalogs
// are built and kept, for every variant: the agent's own, the deepest level's, and each sandbox
// choice's (design section 3.6, the catalog cache keyed by template and environment, here keyed by
// agent, choice and depth class). Building is side-effect free: every input was prepared by the
// owner's catalog build (newSubagentToolDescribed, SetLazy with a load_tool, bindMCP; or, for the
// resolver's builds, AssembleChildren alone, which builds them once), which gives
// each agent a new set and builds its own and deepest-level catalogs at once (buildCatalogs). A
// SetLazy without a load_tool clears the owner's load_tool and rebuilds nothing, as before R4: the
// catalogs already built keep it, and a choice's catalog built later has none.

// childCatalogKey is one variant of an agent's catalog: the sandbox choice ("" for none) and leaf,
// true for a child at the deepest level. An agent that does not nest has one catalog per choice,
// kept under leaf true.
type childCatalogKey struct {
	choice string
	leaf   bool
}

// childCatalogSet holds the catalogs built for one agent from one set of inputs. A call that copied
// the agent before its inputs changed keeps using the set it copied, as it keeps the inputs.
type childCatalogSet struct {
	mu    sync.Mutex
	byKey map[childCatalogKey]*tools.Catalog
}

func newChildCatalogSet() *childCatalogSet {
	return &childCatalogSet{byKey: make(map[childCatalogKey]*tools.Catalog)}
}

// childCatalog is the catalog a child of agent gets for sandbox choice (agent is then the agent
// narrowed to it, chooseSandbox) at the deepest level or not. Each variant is built once and the
// same catalog is returned to every later call, also from parallel calls. It is nil, without an
// error, for an agent whose MCP tools were not bound.
func (s *subagentTool) childCatalog(agent subagentAgent, choice string, leaf bool) (*tools.Catalog, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.childCatalogLocked(agent, choice, leaf)
}

// childCatalogLocked is childCatalog for a caller that holds s.mu, which guards the owner's
// load_tool and subagent tool the catalog takes.
func (s *subagentTool) childCatalogLocked(agent subagentAgent, choice string, leaf bool) (*tools.Catalog, error) {
	set := agent.catalogs
	if set == nil {
		return nil, nil
	}
	key := childCatalogKey{choice: choice, leaf: leaf || !agent.nests}
	set.mu.Lock()
	defer set.mu.Unlock()
	if catalog, found := set.byKey[key]; found {
		return catalog, nil
	}
	catalog, err := s.childCatalogSections(agent, key).Compose()
	if err != nil {
		return nil, fmt.Errorf("tools.native.subagent.agents.%s: %w", agent.config.Name, err)
	}
	set.byKey[key] = catalog
	return catalog, nil
}

// childCatalogSections are a child's sections (CatalogSections, the order the owner's catalog has
// too): the agent's native tools (its pool, profile_message included), the owner's subagent tool
// when the child may nest and is not at the deepest level, the owner's load_tool when the agent's
// settings show it, then the agent's MCP tools. A sandbox choice tells the native tools the chosen
// environment (exec and tool_smith's sandbox sentence, and how git works for a run the choice puts
// in a worktree); the tools themselves are the same.
func (s *subagentTool) childCatalogSections(agent subagentAgent, key childCatalogKey) CatalogSections {
	native := agent.nativeTools
	if key.choice != "" {
		native = describeAgentSandbox(native, agent.config.Sandbox, agent.descriptions)
		if agent.config.Isolated() {
			native = describeAgentIsolation(native)
		}
	}
	sections := CatalogSections{Native: native, MCP: agent.mcpTools}
	if !key.leaf {
		nested := s.nestedSelf()
		sections.Subagent = &nested
	}
	if s.loadTool != nil && tools.ProfileShows(agent.filter, s.loadTool.Definition) {
		sections.LoadTool = s.loadTool
	}
	return sections
}

// catalogsFor is the catalog a call to agent in sandbox choice fingerprints (the one a child that
// may nest gets) and the one its child gets: the same, or the deepest level's when deepest. A
// choice's catalogs are built from the same tools as the agent's own, which the owner's prepare
// built, so they do not fail; if they did, the run reads the agent's own descriptions rather than
// not starting. Both are nil for an agent whose MCP tools were not bound.
func (s *subagentTool) catalogsFor(agent subagentAgent, choice string, deepest bool) (catalog, child *tools.Catalog) {
	variant := func(leaf bool) *tools.Catalog {
		built, err := s.childCatalog(agent, choice, leaf)
		if err != nil && choice != "" {
			built, _ = s.childCatalog(agent, "", leaf)
		}
		return built
	}
	catalog = variant(false)
	if catalog == nil || !deepest {
		return catalog, catalog
	}
	if leaf := variant(true); leaf != nil {
		return catalog, leaf
	}
	return catalog, catalog
}
