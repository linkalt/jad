package native

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"

	"jad/backend-go/internal/tools"
)

// codeContextReadRange answers action read: it prints lines start..end and saves them
// as an entry, so reading code during discovery is also what saves it. Lines an entry
// already holds unchanged are printed but not saved again; the exact range of a stale
// entry refreshes that entry.
func codeContextReadRange(call codeContextCall, reader codeContextReader, store codeContextStore, by string) string {
	text, _, failure := codeContextReadItem(call, reader, store, by, -1)
	if failure != "" {
		return failure
	}
	return text
}

// codeContextReadItem is one read. Its code is printed only when it fits budget bytes
// (budget < 0: no bound); it answers the text, the code bytes printed and a rendered
// tool error when nothing was read.
func codeContextReadItem(call codeContextCall, reader codeContextReader, store codeContextStore, by string, budget int64) (string, int64, string) {
	callID := reader.execution.CallID
	read, failure := reader.readRange(call.path, call.start, call.end)
	if failure != "" {
		return "", 0, failure
	}
	printed := int64(len(read.content))
	if budget >= 0 && printed > budget {
		printed = 0
	}
	description, priority := call.description, call.priority
	var details []string
	borrowed := ""
	if !call.has["description"] {
		if source, found := borrowCodeContextDescription(store, call.task, read, reader.settings.MaxScanTasks); found {
			description = source.entry.Description
			if !call.has["priority"] {
				priority = source.entry.Priority
			}
			borrowed = fmt.Sprintf("description copied from task %s #%d, whose code is the same", source.task, source.entry.ID)
		} else if !call.has["priority"] {
			priority = reader.settings.DefaultPriority
		}
	}
	changing := call.has["description"] || call.has["priority"]

	var saved codeContextEntry
	var summary string
	alreadySaved, kept, created := false, true, false
	document, err := store.mutate(call.task, by, reader.settings.MaxStorageBytes, func(document *codeContextDocument) error {
		holds := func(entry codeContextEntry) bool {
			content, _, ok := codeContextLines(read.data, entry.Start, entry.End)
			return ok && codeContextSHA256(content) == entry.ContentSHA256
		}
		exact := slices.IndexFunc(document.Entries, func(entry codeContextEntry) bool {
			return entry.Path == read.path && entry.Start == call.start && entry.End == call.end
		})
		if exact >= 0 {
			entry := document.Entries[exact]
			if holds(entry) && !changing {
				saved, alreadySaved, summary = entry, true, fmt.Sprintf("already saved as #%d; get it with ids next time", entry.ID)
				return errCodeContextNoChange
			}
			summary = fmt.Sprintf("entry #%d updated", entry.ID)
			if !holds(entry) {
				summary = fmt.Sprintf("entry #%d held other code; it now holds the code below, so check its description still holds", entry.ID)
			}
			read.store(&entry)
			if call.has["description"] {
				entry.Description = call.description
			}
			if call.has["priority"] {
				entry.Priority = call.priority
			}
			entry.UpdatedBy, entry.UpdatedAt, entry.Generation = by, codeContextTimestamp(), document.Generation
			document.Entries[exact] = entry
			saved = entry
			return nil
		}
		if !call.has["description"] {
			for _, entry := range document.Entries {
				if entry.Path == read.path && entry.Start <= call.start && entry.End >= call.end && holds(entry) {
					saved, alreadySaved = entry, true
					summary = fmt.Sprintf("these lines are inside entry #%d (lines %d-%d); get it with ids next time", entry.ID, entry.Start, entry.End)
					return errCodeContextNoChange
				}
			}
		}
		if int64(len(document.Entries)) >= reader.settings.MaxEntries {
			kept, summary = false, fmt.Sprintf("not saved: the task already has %d entries (max_entries)", len(document.Entries))
			return errCodeContextNoChange
		}
		saved = codeContextEntry{
			ID: document.NextID, Priority: priority, Path: read.path, Start: call.start, End: call.end, Description: description,
			UpdatedBy: by, UpdatedAt: codeContextTimestamp(), Generation: document.Generation,
		}
		read.store(&saved)
		document.NextID++
		document.Entries = append(document.Entries, saved)
		created, summary = true, fmt.Sprintf("saved as #%d", saved.ID)
		return nil
	})
	if errors.Is(err, errCodeContextNoChange) {
		document, err = store.load(call.task)
	}
	if err != nil {
		return "", 0, codeContextStoreFailure(callID, err)
	}
	if created && borrowed != "" {
		details = append(details, borrowed)
	}
	if kept && !alreadySaved && saved.Description == "" {
		details = append(details, "no description yet: add one with update")
	}
	stateErr := store.mutateState(call.task, reader.settings.MaxDeliveries, reader.settings.MaxStorageBytes, func(state *codeContextState) {
		state.Stats.Reads++
		state.Stats.CodeBytesPrinted += printed
		if alreadySaved {
			state.Stats.ReadsAlreadySaved++
		}
	})
	if stateErr != nil {
		details = append(details, "stats not saved: "+oneLine(stateErr.Error()))
	}

	var output strings.Builder
	fmt.Fprintf(&output, "code_context task %s, generation %d: %s", call.task, document.Generation, summary)
	if len(details) > 0 {
		output.WriteString(" (" + strings.Join(details, "; ") + ")")
	}
	fmt.Fprintf(&output, "\n=== %s lines %d-%d ===\n", read.path, call.start, call.end)
	if kept {
		fmt.Fprintf(&output, "#%d (priority %d, lines %d-%d, updated by %s)\n", saved.ID, saved.Priority, saved.Start, saved.End, saved.UpdatedBy)
		output.WriteString(codeContextDescriptionText(saved) + "\n")
	}
	if printed == 0 && len(read.content) > 0 {
		hint := fmt.Sprintf("get it with ids [%d]", saved.ID)
		if !kept {
			hint = "read it again in another call"
		}
		fmt.Fprintf(&output, "(code not printed: this call reached max_output_bytes; %s)\n", hint)
		return output.String(), 0, ""
	}
	writeCodeContextCode(&output, read.content, codeContextToken())
	return output.String(), printed, ""
}

// codeContextSource is an entry of another task.
type codeContextSource struct {
	task  string
	entry codeContextEntry
}

// borrowCodeContextDescription finds, in the other tasks, the most recently written
// described entry of the same path whose saved bytes are the bytes just read.
func borrowCodeContextDescription(store codeContextStore, task string, read codeContextRead, limit int64) (codeContextSource, bool) {
	files, err := store.otherTasks(task, limit)
	if err != nil {
		return codeContextSource{}, false
	}
	var best codeContextSource
	found := false
	for _, file := range files {
		document, err := store.load(file.task)
		if err != nil {
			continue
		}
		for _, entry := range document.Entries {
			if entry.Path == read.path && entry.ContentSHA256 == read.contentSHA256 && entry.Description != "" && (!found || entry.UpdatedAt > best.entry.UpdatedAt) {
				best, found = codeContextSource{task: file.task, entry: entry}, true
			}
		}
	}
	return best, found
}

// codeContextAddFrom answers add with from_task: the other task's entry is copied once
// its code is checked unchanged (or found moved), with the caller's description and
// priority when given.
func codeContextAddFrom(call codeContextCall, reader codeContextReader, store codeContextStore, by string) codeContextOutcome {
	callID := reader.execution.CallID
	source, err := store.load(call.fromTask)
	if err != nil {
		return codeContextOutcome{failure: codeContextStoreFailure(callID, err)}
	}
	index := slices.IndexFunc(source.Entries, func(entry codeContextEntry) bool { return entry.ID == call.id })
	if index < 0 {
		return codeContextOutcome{failure: tools.ExecutionError(callID, tools.StatusNotFound, "entry_not_found", fmt.Sprintf("task %s has no entry %d", call.fromTask, call.id))}
	}
	entry := source.Entries[index]
	status := newCodeContextChecker(reader).check(entry)
	if status.stale {
		return codeContextOutcome{failure: tools.ExecutionError(callID, tools.StatusInvalidInput, "stale_source", fmt.Sprintf("entry %d of task %s is stale (%s); read the code and save it with read or add", entry.ID, call.fromTask, status.reason))}
	}
	call.path, call.start, call.end = entry.Path, status.start, status.end
	if !call.has["description"] {
		call.description = entry.Description
	}
	if !call.has["priority"] {
		call.priority = entry.Priority
	}
	return codeContextAdd(call, reader, store, by, fmt.Sprintf("copied from task %s #%d", call.fromTask, entry.ID))
}

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
	task   string
	entry  codeContextEntry
	score  int
	status codeContextStatus
}

// codeContextFind answers find: the entries whose path or description contain the
// query's words, every word first. Entries of other tasks are shown only while their
// code is unchanged, with how to copy them.
func codeContextFind(call codeContextCall, reader codeContextReader, store codeContextStore) string {
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
	collect := func(task string, entries []codeContextEntry) {
		for _, entry := range entries {
			if call.maxPriority > 0 && entry.Priority > call.maxPriority {
				continue
			}
			text := strings.ToLower(entry.Path + "\n" + entry.Description)
			score := 0
			for _, term := range terms {
				if strings.Contains(text, term) {
					score++
				}
			}
			if score > 0 {
				hits = append(hits, codeContextHit{task: task, entry: entry, score: score})
			}
		}
	}
	collect(call.task, document.Entries)
	scanned, unreadable := 0, 0
	if call.allTasks {
		files, err := store.otherTasks(call.task, reader.settings.MaxScanTasks)
		if err != nil {
			return codeContextStoreFailure(callID, err)
		}
		for _, file := range files {
			other, err := store.load(file.task)
			if err != nil {
				unreadable++
				continue
			}
			scanned++
			collect(file.task, other.Entries)
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
		if (a.task == call.task) != (b.task == call.task) {
			return a.task == call.task
		}
		if a.entry.Priority != b.entry.Priority {
			return a.entry.Priority < b.entry.Priority
		}
		if a.task != b.task {
			return a.task < b.task
		}
		return a.entry.ID < b.entry.ID
	})

	checker := newCodeContextChecker(reader)
	var shown []codeContextHit
	hiddenStale, more := 0, 0
	for _, hit := range hits {
		if int64(len(shown)) >= reader.settings.MaxFindResults {
			more++
			continue
		}
		hit.status = checker.check(hit.entry)
		if hit.task != call.task && hit.status.stale {
			hiddenStale++
			continue
		}
		if hit.status.relocated {
			hit.entry.Start, hit.entry.End = hit.status.start, hit.status.end
		}
		shown = append(shown, hit)
	}
	stateErr := store.mutateState(call.task, reader.settings.MaxDeliveries, reader.settings.MaxStorageBytes, func(state *codeContextState) {
		state.Stats.Finds++
		if len(shown) == 0 {
			state.Stats.FindsWithoutMatch++
		}
	})

	var output strings.Builder
	fmt.Fprintf(&output, "code_context find %s in task %s", yamlString(call.query), call.task)
	if call.allTasks {
		fmt.Fprintf(&output, " and %d other tasks", scanned)
	}
	switch {
	case len(shown) == 0:
		output.WriteString(": no saved entry matches; search the code, then save what you find with read or add\n")
	case every:
		fmt.Fprintf(&output, ": %d entries match every word\n", len(shown))
	default:
		fmt.Fprintf(&output, ": no entry matches every word; %d match some, most words first\n", len(shown))
	}
	if more > 0 {
		fmt.Fprintf(&output, "%d more matches left out by max_find_results; narrow the query or set max_priority\n", more)
	}
	if hiddenStale > 0 {
		fmt.Fprintf(&output, "%d matches of other tasks left out: their code changed\n", hiddenStale)
	}
	if unreadable > 0 {
		fmt.Fprintf(&output, "%d other task files could not be read\n", unreadable)
	}
	if stateErr != nil {
		fmt.Fprintf(&output, "stats not saved: %s\n", oneLine(stateErr.Error()))
	}
	for _, hit := range shown {
		state := codeContextMoved(hit.status)
		if hit.status.stale {
			state = ", stale: " + hit.status.reason
		}
		if hit.task == call.task {
			fmt.Fprintf(&output, "\n- #%d  %s %d-%d  (priority %d%s)\n", hit.entry.ID, hit.entry.Path, hit.entry.Start, hit.entry.End, hit.entry.Priority, state)
		} else {
			fmt.Fprintf(&output, "\n- task %s #%d  %s %d-%d  (priority %d%s): copy it with action add, from_task %s, id %d\n",
				hit.task, hit.entry.ID, hit.entry.Path, hit.entry.Start, hit.entry.End, hit.entry.Priority, state, yamlString(hit.task), hit.entry.ID)
		}
		output.WriteString(codeContextDescriptionText(hit.entry) + "\n")
	}
	return output.String()
}
