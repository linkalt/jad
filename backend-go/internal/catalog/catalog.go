// Package catalog scans one kind of configuration files laid out as
// <root>/<category>/<name>.json and narrows them with selections.
//
// Identity is "<category>/<name>": the folder and the file name without
// ".json". The same file name may exist in several categories (one MCP
// program with a headed and a headless environment); they are different
// entries. Every reference to one file is written the same way, in every
// catalog: "openai/gpt-deep". A bare word in a selection is a category.
package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// Entry is one file of a catalog.
type Entry struct {
	Category string
	Name     string
	Path     string
	// Scope is the name of the scope MergeScopes found the entry in ("global", "workspace");
	// empty for a catalog of one folder.
	Scope string
}

// ID is "<category>/<name>".
func (e Entry) ID() string { return e.Category + "/" + e.Name }

// Catalog is an immutable set of entries of one kind, ordered by ID.
type Catalog struct {
	kind    string
	entries []Entry
	index   map[string]int
	// skipped are the files and folders the scan left out for their names; only a scan sets it.
	skipped []Skipped
	// ambiguous are the IDs Merge found in more than one scope, with their entries; naming one
	// without a scope prefix is an error. Narrow and Without keep them.
	ambiguous map[string][]Entry
}

// Skipped is a file or category folder a scan left out because its name is not an identifier
// (or a .json lies outside a category folder). It is in no catalog, so no reference can name it
// and nothing can use it; the scan reports it here instead of failing every other entry.
type Skipped struct {
	Path   string
	Reason string
}

// String is the path and why it was left out.
func (s Skipped) String() string { return s.Path + ": " + s.Reason }

// Skipped returns what the scan left out for its name, ordered by path. A catalog made by
// Narrow or Without reports none: ask the scanned one.
func (c *Catalog) Skipped() []Skipped { return append([]Skipped(nil), c.skipped...) }

func newCatalog(kind string, entries []Entry) *Catalog {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Category != entries[j].Category {
			return entries[i].Category < entries[j].Category
		}
		return entries[i].Name < entries[j].Name
	})
	index := make(map[string]int, len(entries))
	for i, entry := range entries {
		index[entry.ID()] = i
	}
	return &Catalog{kind: kind, entries: entries, index: index}
}

// Kind is the label used in error messages, for example "model".
func (c *Catalog) Kind() string { return c.kind }

// Len is the number of entries.
func (c *Catalog) Len() int { return len(c.entries) }

// Entries returns a copy of the entries ordered by ID.
func (c *Catalog) Entries() []Entry { return append([]Entry(nil), c.entries...) }

// IDs returns the entry IDs in order.
func (c *Catalog) IDs() []string {
	ids := make([]string, len(c.entries))
	for i, entry := range c.entries {
		ids[i] = entry.ID()
	}
	return ids
}

// Categories returns the distinct categories in order.
func (c *Catalog) Categories() []string {
	var categories []string
	for i, entry := range c.entries {
		if i == 0 || entry.Category != c.entries[i-1].Category {
			categories = append(categories, entry.Category)
		}
	}
	return categories
}

// Get returns the entry with this ID.
func (c *Catalog) Get(id string) (Entry, bool) {
	position, ok := c.index[id]
	if !ok {
		return Entry{}, false
	}
	return c.entries[position], true
}

// Lookup returns the entry a "<category>/<name>" reference names; "<scope>:<category>/<name>"
// picks the entry of that scope (workspace layout). A reference of any other shape, and one
// that is not in this catalog, is an error that says what is available.
func (c *Catalog) Lookup(id string) (Entry, error) {
	scope, bare, err := splitScope(id)
	if err != nil {
		return Entry{}, fmt.Errorf("%s %q: %w", c.kind, id, err)
	}
	category, name, ok := strings.Cut(bare, "/")
	if !ok || ValidateIdentifier(category) != nil || ValidateIdentifier(name) != nil {
		return Entry{}, fmt.Errorf("%s %q must be written <category>/<name>", c.kind, id)
	}
	if scope != "" {
		for _, entry := range c.scoped() {
			if entry.Scope == scope && entry.ID() == bare {
				return entry, nil
			}
		}
		return Entry{}, fmt.Errorf("%s %q is not available here (available: %s)", c.kind, id, availableText(c.IDs()))
	}
	if entry, found := c.Get(id); found {
		return entry, nil
	}
	if _, ambiguous := c.ambiguous[id]; ambiguous {
		return Entry{}, c.ambiguityError(id)
	}
	return Entry{}, fmt.Errorf("%s %q is not available here (available: %s)", c.kind, id, availableText(c.IDs()))
}

// Narrow returns the entries a selection keeps. An absent selection keeps all of
// them. Every item must exist: a category with no entry here, or a file that is not
// here, is an error naming it, so a typo or a reference outside the level above is
// reported and never dropped. "*" matches all and cannot fail. A scope-prefixed item
// (workspace layout) matches the entries of that scope, an ambiguous one too; picking the
// same ID from two scopes is an error.
func (c *Catalog) Narrow(sel Selection) (*Catalog, error) {
	if sel.IsAll() {
		return c, nil
	}
	candidates := c.entries
	for _, item := range sel.items {
		if item.scope != "" {
			candidates = c.scoped()
			break
		}
	}
	var missing []string
	for _, item := range sel.items {
		if id := c.ambiguousIn(item); id != "" {
			return nil, c.ambiguityError(id)
		}
		if item.kind == selectAll {
			continue
		}
		found := false
		for _, entry := range candidates {
			if item.matches(entry) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, item.String())
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s %s not available here (available: %s)",
			c.kind, quoteList(missing), availableText(append(c.Categories(), c.IDs()...)))
	}
	var kept []Entry
	picked := map[string]string{}
	for _, entry := range candidates {
		if !sel.matches(entry) {
			continue
		}
		if other, taken := picked[entry.ID()]; taken {
			return nil, fmt.Errorf("%s %q is selected from two scopes (%s and %s); name one", c.kind, entry.ID(), other, entry.Path)
		}
		picked[entry.ID()] = entry.Path
		kept = append(kept, entry)
	}
	narrowed := newCatalog(c.kind, kept)
	narrowed.ambiguous = c.ambiguous
	return narrowed, nil
}

// Without returns the catalog minus the entry with this ID.
func (c *Catalog) Without(id string) *Catalog {
	kept := make([]Entry, 0, len(c.entries))
	for _, entry := range c.entries {
		if entry.ID() != id {
			kept = append(kept, entry)
		}
	}
	without := newCatalog(c.kind, kept)
	without.ambiguous = c.ambiguous
	return without
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
