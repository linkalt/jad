package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// categoryPrefix starts a selector that matches every entry of one category.
const categoryPrefix = "category:"

type selectorKind int

const (
	selectName selectorKind = iota
	selectCategory
	selectAll
)

// Selector is one item of a Selection: a name, "category:<c>" or "*".
type Selector struct {
	kind  selectorKind
	value string
}

// ParseSelector parses one selector. Whitespace is never trimmed: a padded
// entry is an error, so a typo cannot silently select nothing.
func ParseSelector(text string) (Selector, error) {
	switch {
	case text == "*":
		return Selector{kind: selectAll}, nil
	case strings.HasPrefix(text, categoryPrefix):
		category := strings.TrimPrefix(text, categoryPrefix)
		if err := ValidateIdentifier(category); err != nil {
			return Selector{}, fmt.Errorf("selector %q: category %w", text, err)
		}
		return Selector{kind: selectCategory, value: category}, nil
	}
	if strings.Contains(text, ":") {
		return Selector{}, fmt.Errorf("selector %q: only %q prefixes a selector", text, categoryPrefix)
	}
	if err := ValidateIdentifier(text); err != nil {
		return Selector{}, fmt.Errorf("selector %q: name %w", text, err)
	}
	return Selector{kind: selectName, value: text}, nil
}

// String returns the selector text that ParseSelector accepts.
func (s Selector) String() string {
	switch s.kind {
	case selectAll:
		return "*"
	case selectCategory:
		return categoryPrefix + s.value
	}
	return s.value
}

func (s Selector) matches(entry Entry) bool {
	switch s.kind {
	case selectAll:
		return true
	case selectCategory:
		return entry.Category == s.value
	}
	return entry.Name == s.value
}

// Selection is the optional list a level uses to narrow the level above.
// The zero value is "absent": everything the level above offers. A present
// empty list selects nothing.
type Selection struct {
	items   []Selector
	present bool
}

// All is the absent selection.
func All() Selection { return Selection{} }

// Of builds a present selection from selector texts.
func Of(texts ...string) (Selection, error) {
	items := make([]Selector, 0, len(texts))
	for _, text := range texts {
		item, err := ParseSelector(text)
		if err != nil {
			return Selection{}, err
		}
		items = append(items, item)
	}
	return Selection{items: items, present: true}, nil
}

// IsAll reports whether the selection is absent.
func (s Selection) IsAll() bool { return !s.present }

// Selectors returns the parsed items, or nil when the selection is absent.
func (s Selection) Selectors() []Selector {
	if !s.present {
		return nil
	}
	return append([]Selector(nil), s.items...)
}

// matches is only called for a present selection; Narrow handles absent.
func (s Selection) matches(entry Entry) bool {
	for _, item := range s.items {
		if item.matches(entry) {
			return true
		}
	}
	return false
}

// UnmarshalJSON accepts a JSON list of selector strings. null is refused:
// "absent" is a missing key, and a null would hide a mistake.
func (s *Selection) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("selection must be a list of strings, not null")
	}
	var texts []string
	if err := json.Unmarshal(data, &texts); err != nil {
		return fmt.Errorf("selection must be a list of strings: %w", err)
	}
	parsed, err := Of(texts...)
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// MarshalJSON writes the list; an absent selection has no JSON form and
// must be left out of the object with omitzero.
func (s Selection) MarshalJSON() ([]byte, error) {
	texts := make([]string, 0, len(s.items))
	for _, item := range s.items {
		texts = append(texts, item.String())
	}
	return json.Marshal(texts)
}

// IsZero lets encoding/json omit an absent selection (omitzero).
func (s Selection) IsZero() bool { return !s.present }
