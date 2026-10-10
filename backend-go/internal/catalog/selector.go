package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type selectorKind int

const (
	selectCategory selectorKind = iota
	selectAll
	selectFile
)

// Selector is one item of a Selection: "<category>" for a whole folder,
// "<category>/<name>" for one file, or "*" for everything.
type Selector struct {
	kind     selectorKind
	category string
	name     string // only for selectFile
}

// ParseSelector parses one selector. Whitespace is never trimmed: a padded
// entry is an error, so a typo cannot silently select nothing.
func ParseSelector(text string) (Selector, error) {
	if text == "*" {
		return Selector{kind: selectAll}, nil
	}
	if strings.Contains(text, ":") {
		return Selector{}, fmt.Errorf("selector %q: write a category as its bare name and a file as <category>/<name>; no prefix is used", text)
	}
	if category, name, ok := strings.Cut(text, "/"); ok {
		if err := ValidateIdentifier(category); err != nil {
			return Selector{}, fmt.Errorf("selector %q: category %w", text, err)
		}
		if err := ValidateIdentifier(name); err != nil {
			return Selector{}, fmt.Errorf("selector %q: name %w", text, err)
		}
		return Selector{kind: selectFile, category: category, name: name}, nil
	}
	if err := ValidateIdentifier(text); err != nil {
		return Selector{}, fmt.Errorf("selector %q: category %w", text, err)
	}
	return Selector{kind: selectCategory, category: text}, nil
}

// String returns the selector text that ParseSelector accepts.
func (s Selector) String() string {
	switch s.kind {
	case selectAll:
		return "*"
	case selectFile:
		return s.category + "/" + s.name
	}
	return s.category
}

func (s Selector) matches(entry Entry) bool {
	switch s.kind {
	case selectAll:
		return true
	case selectFile:
		return entry.Category == s.category && entry.Name == s.name
	}
	return entry.Category == s.category
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
