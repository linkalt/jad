// Package catalog scans one kind of configuration files laid out as
// <root>/<category>/<name>.json and narrows them with selections.
//
// Identity is the file name and is unique across the whole kind. The
// category is the parent folder and is used only to select and group, so
// moving a file between categories changes who can see it but breaks no
// reference.
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
	index   map[string]int
}

func newCatalog(kind string, entries []Entry) *Catalog {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	index := make(map[string]int, len(entries))
	for i, entry := range entries {
		index[entry.Name] = i
	}
	return &Catalog{kind: kind, entries: entries, index: index}
}

// Kind is the label used in error messages, for example "model".
func (c *Catalog) Kind() string { return c.kind }

// Len is the number of entries.
func (c *Catalog) Len() int { return len(c.entries) }

// Entries returns a copy of the entries ordered by name.
func (c *Catalog) Entries() []Entry { return append([]Entry(nil), c.entries...) }

// Names returns the entry names in order.
func (c *Catalog) Names() []string {
	names := make([]string, len(c.entries))
	for i, entry := range c.entries {
		names[i] = entry.Name
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

// Get returns the entry with this name.
func (c *Catalog) Get(name string) (Entry, bool) {
	i, ok := c.index[name]
	if !ok {
		return Entry{}, false
	}
	return c.entries[i], true
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
		if item.kind == selectName {
			if _, ok := c.index[item.value]; !ok {
				missing = append(missing, item.value)
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
