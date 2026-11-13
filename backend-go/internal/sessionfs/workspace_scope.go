package sessionfs

import (
	"strings"
)

// Catalog route scopes of the workspace layout (design §4.3, decider fix D): "global" is G's
// models, toolsets, MCP servers and sandbox templates; "workspace:<id>" is that workspace's
// profiles, models and toolsets. A request without a scope keeps the v1 target: in v2 the
// start workspace's profiles and the global models, toolsets, MCP servers and templates.

// CatalogScopeGlobal is the scope of G's catalogs.
const CatalogScopeGlobal = "global"

// CatalogScopeWorkspacePrefix starts the scope of one workspace's catalogs.
const CatalogScopeWorkspacePrefix = "workspace:"

type catalogScope int

const (
	scopeNone catalogScope = iota
	scopeGlobal
	scopeWorkspace
)

// scopeUser is a workspace whose profiles and sessions may use a global catalog file: a
// global delete is refused while any of them does.
type scopeUser struct {
	layout Layout
	roots  []Root
}

// ScopedCatalogLayout is the layout the catalog routes use for kind in scope, and the roots
// whose sessions count for its in-use checks. A scope that names an unknown or
// unserved workspace, or a kind the scope has no folder for, is CatalogNotFound.
func ScopedCatalogLayout(roots []Root, scope string, kind CatalogKind) (Layout, []Root, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = CatalogScopeGlobal
		if kind == CatalogProfiles {
			scope = CatalogScopeWorkspacePrefix + roots[0].ID
		}
	}
	if scope == CatalogScopeGlobal {
		layout := *roots[0].Layout
		// Only G's folders: no workspace's own catalogs, profiles or base.
		layout.Profiles, layout.WorkspaceModels, layout.WorkspaceTools, layout.Workspace, layout.WorkspaceSetting = "", "", "", "", ""
		layout.scope = scopeGlobal
		for _, root := range roots {
			if root.Layout != nil {
				layout.scopeUsers = append(layout.scopeUsers, scopeUser{layout: *root.Layout, roots: []Root{root}})
			}
		}
		return layout, roots, nil
	}
	id, ok := strings.CutPrefix(scope, CatalogScopeWorkspacePrefix)
	if !ok || id == "" {
		return Layout{}, nil, &CatalogError{Kind: CatalogBadID, Message: "scope must be global or workspace:<id>"}
	}
	for _, root := range roots {
		if root.ID == id && root.Layout != nil {
			layout := *root.Layout
			layout.scope = scopeWorkspace
			return layout, []Root{root}, nil
		}
	}
	return Layout{}, nil, &CatalogError{Kind: CatalogNotFound, Message: "workspace not found"}
}

// scopedCatalogDir is the folder of kind in a scoped layout; ok false outside a scope.
func (l Layout) scopedCatalogDir(kind CatalogKind) (string, bool, error) {
	switch l.scope {
	case scopeGlobal:
		if kind == CatalogProfiles {
			return "", true, &CatalogError{Kind: CatalogNotFound, Message: "profiles belong to a workspace in the workspace layout; use scope workspace:<id>"}
		}
	case scopeWorkspace:
		switch kind {
		case CatalogModels:
			return l.WorkspaceModels, true, nil
		case CatalogToolsets:
			return l.WorkspaceTools, true, nil
		case CatalogMCP, CatalogSandboxes:
			return "", true, &CatalogError{Kind: CatalogNotFound, Message: string(kind) + " files are global in the workspace layout; use scope global"}
		}
	}
	return "", false, nil
}

// usersOf are the layouts and roots an in-use check of this layout looks at: every
// workspace for a global scope, else the layout itself.
func (l Layout) usersOf(roots []Root) []scopeUser {
	if l.scope == scopeGlobal {
		return l.scopeUsers
	}
	return []scopeUser{{layout: l, roots: roots}}
}
