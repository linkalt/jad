package native

import "jad/backend-go/internal/tools"

// CatalogSections are the parts one agent's catalog is composed from, in the order its model sees
// them: native and extension tools, the subagent tool, profile_message, load_tool, then MCP tools.
// The owner's registered catalog and every child catalog are composed by Compose, so the order is
// written once. Which tools fill a section is each side's policy, decided before composing:
//
//   - the owner: its native tools, its subagent tool and profile_message (hooked with its own
//     hooks), its load_tool and its MCP copies, unfiltered; its allow list, plan mode and strict
//     entries narrow the composed catalog per prepare (applyProfileTools).
//   - a child: its agent's native pool, already shown by the agent's filter (the owner's deny plus
//     the agent's own tools) and described for its sandbox, with profile_message in it; the owner's
//     subagent tool unless the child is at the deepest level; the owner's own load_tool when the
//     agent's filter shows it; the agent's MCP copies (bindMCP).
type CatalogSections struct {
	Native         []tools.Tool
	Subagent       *tools.Tool
	ProfileMessage *tools.Tool
	LoadTool       *tools.Tool
	MCP            []tools.Tool
}

// Tools lists the sections' tools in their order.
func (c CatalogSections) Tools() []tools.Tool {
	list := make([]tools.Tool, 0, len(c.Native)+3+len(c.MCP))
	list = append(list, c.Native...)
	for _, tool := range []*tools.Tool{c.Subagent, c.ProfileMessage, c.LoadTool} {
		if tool != nil {
			list = append(list, *tool)
		}
	}
	return append(list, c.MCP...)
}

// Compose is the catalog of the sections' tools in their order.
func (c CatalogSections) Compose() (*tools.Catalog, error) {
	return tools.NewCatalog(c.Tools())
}
