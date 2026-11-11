package tools

import "slices"

// JoinDeny is the deny entries of the lists in order, each entry once: the ceiling a run hands
// the runs it starts (ExecutionContext.Deny). It is nil when every list is empty.
func JoinDeny(lists ...[]string) []string {
	var joined []string
	for _, list := range lists {
		for _, entry := range list {
			if !slices.Contains(joined, entry) {
				joined = append(joined, entry)
			}
		}
	}
	return joined
}

// WithoutDenied is the catalog without the tools a deny entry covers (EntryMatches), in the same
// order, each kept tool unchanged (its hooks, lazy gate and route are the tool's own). It returns
// the receiver itself when no entry covers a tool, so an unnarrowed run keeps the very catalog it
// had. Unlike a profile's tools.deny, an entry that covers nothing is not an error, and a catalog
// left with no tool is valid: the run answers in text. The receiver is unchanged.
func (c *Catalog) WithoutDenied(deny []string) (*Catalog, error) {
	if c == nil || len(deny) == 0 {
		return c, nil
	}
	covered := false
	for _, tool := range c.tools {
		if AnyEntryMatches(deny, tool.Definition) {
			covered = true
			break
		}
	}
	if !covered {
		return c, nil
	}
	narrowed, _, err := c.Filter(func(definition Definition) bool { return !AnyEntryMatches(deny, definition) })
	return narrowed, err
}
