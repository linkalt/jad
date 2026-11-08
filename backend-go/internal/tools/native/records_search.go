package native

import (
	"strings"
)

// recordsSearchTerms splits a search query into lower-case words. A record matches when
// each word appears in at least one of its string or number values.
func recordsSearchTerms(query string) []string {
	return strings.Fields(strings.ToLower(query))
}

// recordsCursorFingerprint is the filter fingerprint of a cursor, with the normalized
// search query added for a search cursor. A list cursor's fingerprint is unchanged, so
// cursors written before search existed still continue.
func recordsCursorFingerprint(filter map[string]any, excludeAny []map[string]any, query string) (string, error) {
	if query == "" {
		return recordsFilterFingerprint(filter, excludeAny)
	}
	base, err := recordsFilterFingerprint(filter, excludeAny)
	if err != nil {
		return "", err
	}
	return recordsFilterFingerprint(map[string]any{"filter": base, "query": query}, nil)
}

// recordsContainsTerms reports whether every term occurs, case-insensitively, in some
// string or number value of value; keys are not searched. No terms always matches.
func recordsContainsTerms(value *recordsJSONValue, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	var texts []string
	var collect func(*recordsJSONValue)
	collect = func(node *recordsJSONValue) {
		switch node.kind {
		case recordsString, recordsNumber:
			texts = append(texts, strings.ToLower(node.text))
		case recordsArray:
			for _, entry := range node.array {
				collect(entry)
			}
		case recordsObject:
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
