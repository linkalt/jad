package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// Merge joins the catalogs of one kind found in several scopes: the global and the workspace
// folders of the workspace layout (docs/design_workspace_layout.md §4.4). An ID found in more
// than one scope is ambiguous: it is left out of the entries and listed by Skipped, so a load
// warns about it, and naming it (Lookup, or a Narrow selector that names it or its category)
// is an error that lists every file. No scope wins silently. With one catalog, that catalog is
// returned as it is.
func Merge(kind string, scopes ...*Catalog) *Catalog {
	if len(scopes) == 1 {
		return scopes[0]
	}
	paths := map[string][]string{}
	var entries []Entry
	var skipped []Skipped
	for _, scope := range scopes {
		if scope == nil {
			continue
		}
		skipped = append(skipped, scope.skipped...)
		for _, entry := range scope.entries {
			paths[entry.ID()] = append(paths[entry.ID()], entry.Path)
			entries = append(entries, entry)
		}
	}
	ambiguous := map[string][]string{}
	kept := entries[:0]
	for _, entry := range entries {
		if found := paths[entry.ID()]; len(found) > 1 {
			ambiguous[entry.ID()] = found
			skipped = append(skipped, Skipped{Path: entry.Path, Reason: fmt.Sprintf("%s %s is in more than one scope (%s); it is left out until one of the files is renamed",
				kind, entry.ID(), strings.Join(found, ", "))})
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

// ambiguityError is the error for naming an ID that Merge found in more than one scope.
func (c *Catalog) ambiguityError(id string) error {
	return fmt.Errorf("%s %q is in more than one scope (%s); rename one of the files, a name is used in one scope only", c.kind, id, strings.Join(c.ambiguous[id], ", "))
}

// ambiguousIn is the first ambiguous ID a selector names, in ID order; "" when none.
func (c *Catalog) ambiguousIn(item Selector) string {
	if len(c.ambiguous) == 0 || item.kind == selectAll {
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
