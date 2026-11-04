package native

import (
	"strings"
)

// memorySearchTerms splits a search query into lower-case words. A record matches when
// each word appears in at least one of its string or number values.
func memorySearchTerms(query string) []string {
	return strings.Fields(strings.ToLower(query))
}

// memoryCursorFingerprint is the filter fingerprint of a cursor, with the normalized
// search query added for a search cursor. A list cursor's fingerprint is unchanged, so
// cursors written before search existed still continue.
func memoryCursorFingerprint(filter map[string]any, excludeAny []map[string]any, query string) (string, error) {
	if query == "" {
		return memoryFilterFingerprint(filter, excludeAny)
	}
	base, err := memoryFilterFingerprint(filter, excludeAny)
	if err != nil {
		return "", err
	}
	return memoryFilterFingerprint(map[string]any{"filter": base, "query": query}, nil)
}

// memoryContainsTerms reports whether every term occurs, case-insensitively, in some
// string or number value of value; keys are not searched. No terms always matches.
func memoryContainsTerms(value *memoryJSONValue, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	var texts []string
	var collect func(*memoryJSONValue)
	collect = func(node *memoryJSONValue) {
		switch node.kind {
		case memoryString, memoryNumber:
			texts = append(texts, strings.ToLower(node.text))
		case memoryArray:
			for _, entry := range node.array {
				collect(entry)
			}
		case memoryObject:
			for _, pair := range node.object {
				collect(pair.value)
			}
		}
	}
	collect(value)
	for _, term := range terms {
		found := false
		for _, text := range texts {
			if strings.Contains(text, term) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
