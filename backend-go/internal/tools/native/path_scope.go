package native

import (
	"path/filepath"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// scopedPolicy returns the tool's policy narrowed by the call's PathScope. The scope
// adds a condition (Within) and extra deny rules; it never removes a rule, so the
// result allows a subset of what policy allowed. It is applied once, to a policy that
// has no Within of its own: only this function sets it.
func scopedPolicy(policy sessionfs.NativePathPolicy, execution tools.ExecutionContext) sessionfs.NativePathPolicy {
	scope := execution.PathScope
	if scope == nil {
		return policy
	}
	root := execution.WorkspaceRoot
	narrowed := sessionfs.NativePathPolicy{
		Paths:             append([]sessionfs.NativePathRule(nil), policy.Paths...),
		UnrestrictedPaths: policy.UnrestrictedPaths,
		Within:            append([]string(nil), policy.Within...),
	}
	if len(scope.Allow) > 0 {
		allowed := make([]string, 0, len(scope.Allow))
		for _, entry := range scope.Allow {
			allowed = append(allowed, scopeRoot(root, entry))
		}
		narrowed.Within = allowed
	}
	for _, entry := range scope.Deny {
		narrowed.Paths = append(narrowed.Paths, sessionfs.NativePathRule{Action: "deny", Path: scopeRoot(root, entry)})
	}
	return narrowed
}

// scopeRoot makes one entry absolute from the workspace root and follows symlinks of
// the part that exists, because the path checks compare against resolved paths.
func scopeRoot(workspaceRoot, entry string) string {
	if !filepath.IsAbs(entry) {
		entry = filepath.Join(workspaceRoot, entry)
	}
	entry = filepath.Clean(entry)
	existing, rest := entry, ""
	for {
		if resolved, err := filepath.EvalSymlinks(existing); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return entry
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
}

func withinAny(roots []string, target string) bool {
	for _, root := range roots {
		if sessionfs.PathWithin(root, target) {
			return true
		}
	}
	return false
}

// narrowScope is the scope of a child: its agent's own paths cut down to what the
// parent's scope reaches, so a child never reaches more than the run that started it.
// Allow is the intersection of both Allow lists (an empty list reaches everything), Deny
// is both Deny lists together. An intersection with nothing in it denies every path.
func narrowScope(workspaceRoot string, parent *tools.PathScope, own *tools.PathScope) *tools.PathScope {
	if parent == nil && own == nil {
		return nil
	}
	if parent == nil {
		return &tools.PathScope{Allow: append([]string(nil), own.Allow...), Deny: append([]string(nil), own.Deny...)}
	}
	if own == nil {
		own = &tools.PathScope{}
	}
	narrowed := &tools.PathScope{}
	for _, entry := range append(append([]string(nil), parent.Deny...), own.Deny...) {
		narrowed.Deny = append(narrowed.Deny, scopeRoot(workspaceRoot, entry))
	}
	switch {
	case len(parent.Allow) == 0:
		for _, entry := range own.Allow {
			narrowed.Allow = append(narrowed.Allow, scopeRoot(workspaceRoot, entry))
		}
	case len(own.Allow) == 0:
		for _, entry := range parent.Allow {
			narrowed.Allow = append(narrowed.Allow, scopeRoot(workspaceRoot, entry))
		}
	default:
		for _, a := range parent.Allow {
			a = scopeRoot(workspaceRoot, a)
			for _, b := range own.Allow {
				b = scopeRoot(workspaceRoot, b)
				switch {
				case sessionfs.PathWithin(a, b):
					narrowed.Allow = append(narrowed.Allow, b)
				case sessionfs.PathWithin(b, a):
					narrowed.Allow = append(narrowed.Allow, a)
				}
			}
		}
		if len(narrowed.Allow) == 0 {
			narrowed.Deny = append(narrowed.Deny, string(filepath.Separator))
		}
	}
	return narrowed
}
