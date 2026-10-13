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
		if isWithin(root, target) {
			return true
		}
	}
	return false
}
