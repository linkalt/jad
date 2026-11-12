package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// Scope names a catalog of one scope of the workspace layout for MergeScopes: "global" for
// G's folder, "workspace" for the workspace's own, "profile" for the own models of the profile
// a catalog is made for (phase 3c). A reference may start with "<name>:" to pick the entry of
// that scope (ParseSelector, Lookup).
type Scope struct {
	Name    string
	Catalog *Catalog
}

// The scope names of the workspace layout.
const (
	ScopeGlobal    = "global"
	ScopeWorkspace = "workspace"
	ScopeProfile   = "profile"
)

// ScopePrefixes are the scope names a selector or reference may start with. Only the catalog
// of one profile's own load has entries of ScopeProfile; elsewhere a profile: reference is
// not found.
var ScopePrefixes = []string{ScopeGlobal, ScopeWorkspace, ScopeProfile}

// Merge joins catalogs of one kind without scope names; see MergeScopes.
func Merge(kind string, scopes ...*Catalog) *Catalog {
	named := make([]Scope, 0, len(scopes))
	for _, scope := range scopes {
		named = append(named, Scope{Catalog: scope})
	}
	return MergeScopes(kind, named...)
}

// MergeScopes joins the catalogs of one kind found in several scopes: the global and the
// workspace folders of the workspace layout (docs/design_workspace_layout.md §4.4). Each entry
// keeps its scope's name. An ID found in more than one scope is ambiguous: it is left out of
// the entries and listed by Skipped, so a load warns about it, and naming it without a scope
// prefix (Lookup, or a Narrow selector that names it or its category) is an error that lists
// every file. With a prefix ("global:openai/default") the entry of that scope is picked. No
// scope wins silently. With one catalog, that catalog is returned as it is.
func MergeScopes(kind string, scopes ...Scope) *Catalog {
	if len(scopes) == 1 {
		return scopes[0].Catalog
	}
	byID := map[string][]Entry{}
	var entries []Entry
	var skipped []Skipped
	for _, scope := range scopes {
		if scope.Catalog == nil {
			continue
		}
		skipped = append(skipped, scope.Catalog.skipped...)
		for _, entry := range scope.Catalog.entries {
			entry.Scope = scope.Name
			byID[entry.ID()] = append(byID[entry.ID()], entry)
			entries = append(entries, entry)
		}
	}
	ambiguous := map[string][]Entry{}
	kept := entries[:0]
	for _, entry := range entries {
		if found := byID[entry.ID()]; len(found) > 1 {
			ambiguous[entry.ID()] = found
			skipped = append(skipped, Skipped{Path: entry.Path, Reason: fmt.Sprintf("%s %s is in more than one scope (%s); it is left out until one of the files is renamed or a reference names its scope (%s)",
				kind, entry.ID(), strings.Join(entryPaths(found), ", "), scopedExamples(found))})
			continue
		}
		kept = append(kept, entry)
	}
	merged := newCatalog(kind, kept)
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Path < skipped[j].Path })
	merged.skipped = skipped
	if len(ambiguous) > 0 {
		merged.ambiguous = ambiguous
	}
	return merged
}

func entryPaths(entries []Entry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	return paths
}

// scopedExamples are the prefixed references that pick each entry, where scopes are named.
func scopedExamples(entries []Entry) string {
	var examples []string
	for _, entry := range entries {
		if entry.Scope != "" {
			examples = append(examples, entry.Scope+":"+entry.ID())
		}
	}
	if len(examples) == 0 {
		return "no scope prefix here"
	}
	return strings.Join(examples, " or ")
}

// ambiguityError is the error for naming an ID that Merge found in more than one scope.
func (c *Catalog) ambiguityError(id string) error {
	return fmt.Errorf("%s %q is in more than one scope (%s); rename one of the files or name its scope (%s); a name without a scope is used in one scope only",
		c.kind, id, strings.Join(entryPaths(c.ambiguous[id]), ", "), scopedExamples(c.ambiguous[id]))
}

// ambiguousIn is the first ambiguous ID a selector without a scope names, in ID order; ""
// when none.
func (c *Catalog) ambiguousIn(item Selector) string {
	if len(c.ambiguous) == 0 || item.kind == selectAll || item.scope != "" {
		return ""
	}
	ids := make([]string, 0, len(c.ambiguous))
	for id := range c.ambiguous {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		category, name, _ := strings.Cut(id, "/")
		if item.matches(Entry{Category: category, Name: name}) {
			return id
		}
	}
	return ""
}

// scoped are every entry a scope-prefixed selector may match: the kept entries and the
// ambiguous ones, which only a prefix can pick.
func (c *Catalog) scoped() []Entry {
	all := append([]Entry(nil), c.entries...)
	ids := make([]string, 0, len(c.ambiguous))
	for id := range c.ambiguous {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		all = append(all, c.ambiguous[id]...)
	}
	return all
}
