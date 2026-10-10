// Package catalog scans one kind of configuration files laid out as
// <root>/<category>/<name>.json and narrows them with selections.
//
// Identity is the file name. The category is the parent folder and selects
// and groups. The same name may exist in several categories, for example one
// MCP server with a headed and a headless environment. What a profile ends up
// with must then hold each name once: Unique reports the clash. A reference
// by plain name stays valid when a file moves between categories as long as
// the name is unique in the set it is resolved in.
package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// Entry is one file of a catalog.
type Entry struct {
	Name     string
	Category string
	Path     string
}

// Catalog is an immutable set of entries of one kind, ordered by name.
type Catalog struct {
	kind    string
	entries []Entry
	index   map[string][]int
}

func newCatalog(kind string, entries []Entry) *Catalog {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Category < entries[j].Category
	})
	index := make(map[string][]int, len(entries))
	for i, entry := range entries {
		index[entry.Name] = append(index[entry.Name], i)
	}
	return &Catalog{kind: kind, entries: entries, index: index}
}

// Kind is the label used in error messages, for example "model".
func (c *Catalog) Kind() string { return c.kind }

// Len is the number of entries.
func (c *Catalog) Len() int { return len(c.entries) }

// Entries returns a copy of the entries ordered by name.
func (c *Catalog) Entries() []Entry { return append([]Entry(nil), c.entries...) }

// Names returns the distinct entry names in order.
func (c *Catalog) Names() []string {
	names := make([]string, 0, len(c.index))
	for i, entry := range c.entries {
		if i == 0 || entry.Name != c.entries[i-1].Name {
			names = append(names, entry.Name)
		}
	}
	return names
}

// Categories returns the distinct categories in order.
func (c *Catalog) Categories() []string {
	seen := map[string]bool{}
	var categories []string
	for _, entry := range c.entries {
		if !seen[entry.Category] {
			seen[entry.Category] = true
			categories = append(categories, entry.Category)
		}
	}
	sort.Strings(categories)
	return categories
}

// Find returns every entry with this name, ordered by category.
func (c *Catalog) Find(name string) []Entry {
	positions := c.index[name]
	found := make([]Entry, len(positions))
	for i, position := range positions {
		found[i] = c.entries[position]
	}
	return found
}

// Get returns the entry with this name. It reports false when the name is
// missing and also when it is ambiguous; use Find or Unique to tell them apart.
func (c *Catalog) Get(name string) (Entry, bool) {
	positions := c.index[name]
	if len(positions) != 1 {
		return Entry{}, false
	}
	return c.entries[positions[0]], true
}

// Unique returns an error naming every file path of every name that exists
// more than once. A resolved set (what a profile uses) must pass it.
func (c *Catalog) Unique() error {
	var clashes []string
	for _, name := range c.Names() {
		found := c.Find(name)
		if len(found) < 2 {
			continue
		}
		paths := make([]string, len(found))
		for i, entry := range found {
			paths[i] = entry.Path
		}
		clashes = append(clashes, fmt.Sprintf("%q is %s (%s)", name, ambiguousWord(len(found)), strings.Join(paths, ", ")))
	}
	if len(clashes) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %s; select a category or use <category>/<name> to pick one",
		c.kind, strings.Join(clashes, "; "))
}

func ambiguousWord(count int) string {
	return fmt.Sprintf("in %d categories", count)
}

// Narrow returns the entries a selection keeps. An absent selection keeps
// all of them. A plain name that this catalog does not hold is an error, so
// a reference outside the level above is reported and never dropped. A
// category or "*" selector only intersects and may match nothing.
func (c *Catalog) Narrow(sel Selection) (*Catalog, error) {
	if sel.IsAll() {
		return c, nil
	}
	var missing []string
	for _, item := range sel.items {
		switch item.kind {
		case selectName:
			if _, ok := c.index[item.value]; !ok {
				missing = append(missing, item.value)
			}
		case selectQualified:
			if !c.hasQualified(item) {
				missing = append(missing, item.String())
			}
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s %s not available here (available: %s)",
			c.kind, quoteList(missing), availableText(c.Names()))
	}
	var kept []Entry
	for _, entry := range c.entries {
		if sel.matches(entry) {
			kept = append(kept, entry)
		}
	}
	return newCatalog(c.kind, kept), nil
}

func (c *Catalog) hasQualified(item Selector) bool {
	for _, entry := range c.Find(item.value) {
		if entry.Category == item.category {
			return true
		}
	}
	return false
}

func quoteList(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = fmt.Sprintf("%q", value)
	}
	if len(quoted) == 1 {
		return quoted[0] + " is"
	}
	return strings.Join(quoted, ", ") + " are"
}

func availableText(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
