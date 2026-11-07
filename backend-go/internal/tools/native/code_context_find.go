package native

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"

	"jad/backend-go/internal/tools"
)

// codeContextTerms splits a find query into lower-case words. Path characters stay in a
// word, so a file name is one word; sentence punctuation is dropped.
func codeContextTerms(query string) []string {
	var terms []string
	for _, field := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_./-", r)
	}) {
		field = strings.Trim(field, ".-/")
		if field != "" && !slices.Contains(terms, field) {
			terms = append(terms, field)
		}
	}
	return terms
}

type codeContextHit struct {
	entry codeContextEntry
	score int
}

// codeContextFind answers find: the task's entries whose path or description contain the
// query's words, those with every word first, checked against their files.
func codeContextFind(call codeContextCall, reader codeContextReader, store codeContextStore, by string) string {
	callID := reader.execution.CallID
	terms := codeContextTerms(call.query)
	if len(terms) == 0 {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_query", "query has no words to look for")
	}
	document, err := store.load(call.task)
	if err != nil {
		return codeContextStoreFailure(callID, err)
	}
	var hits []codeContextHit
	for _, entry := range document.Entries {
		text := strings.ToLower(entry.Path + "\n" + entry.Description)
		score := 0
		for _, term := range terms {
			if strings.Contains(text, term) {
				score++
			}
		}
		if score > 0 {
			hits = append(hits, codeContextHit{entry: entry, score: score})
		}
	}
	every := slices.ContainsFunc(hits, func(hit codeContextHit) bool { return hit.score == len(terms) })
	if every {
		hits = slices.DeleteFunc(hits, func(hit codeContextHit) bool { return hit.score < len(terms) })
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.entry.Priority != b.entry.Priority {
			return a.entry.Priority < b.entry.Priority
		}
		return a.entry.ID < b.entry.ID
	})
	more := 0
	if over := len(hits) - int(reader.settings.MaxFindResults); over > 0 {
		hits, more = hits[:reader.settings.MaxFindResults], over
	}
	selected := make([]codeContextEntry, len(hits))
	for index, hit := range hits {
		selected[index] = hit.entry
	}
	checked := checkCodeContextTask(call.task, reader, store, by, document, selected)

	var output strings.Builder
	fmt.Fprintf(&output, "code_context find %s in task %s", yamlString(call.query), call.task)
	switch {
	case len(hits) == 0:
		output.WriteString(": no entry matches; search the code, then save what you find with read\n")
	case every:
		fmt.Fprintf(&output, ": %d entries match every word\n", len(hits))
	default:
		fmt.Fprintf(&output, ": no entry matches every word; %d match some, most words first\n", len(hits))
	}
	if more > 0 {
		fmt.Fprintf(&output, "%d more matches left out by max_find_results; narrow the query\n", more)
	}
	if checked.note != "" {
		output.WriteString(checked.note + "\n")
	}
	for _, entry := range checked.entries {
		writeCodeContextEntry(&output, entry, codeContextWhere(entry), codeContextNote(checked.statuses[entry.ID]))
	}
	return output.String()
}
