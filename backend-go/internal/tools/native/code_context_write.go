package native

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"jad/backend-go/internal/tools"
)

// codeContextResult is what one read or update item did to the task.
type codeContextResult struct {
	// failure is a rendered tool error; the item changed nothing.
	failure string
	summary string
	// entry is the entry as the item left it; kept is false when nothing was saved.
	entry codeContextEntry
	kept  bool
	// code is the bytes read, printed when print is set.
	code []byte
	path string
	// data is the whole file the item read, at path, so the task's other entries of
	// that file are checked without reading it again.
	data    []byte
	print   bool
	changed bool
}

// errCodeContextAllFailed ends a mutation whose every item failed, so nothing is written.
var errCodeContextAllFailed = errors.New("every item failed")

// codeContextWrite answers read and update, of one range or of items. The items are
// applied in order to the task in one write; one that fails is reported on its line and
// does not stop the others. When every item fails nothing is written and the answer is a
// tool error.
func codeContextWrite(call codeContextCall, reader codeContextReader, store codeContextStore, by string) string {
	callID := reader.execution.CallID
	batch := call.has["items"]
	items := call.items
	if !batch && (call.action == "read" || call.has["id"]) {
		items = []codeContextCall{call}
	}
	results := make([]codeContextResult, len(items))
	for index, item := range items {
		results[index].failure = validateCodeContextItem(item, reader.settings, callID)
		if missing := codeContextMissingFields(item); results[index].failure == "" && missing != "" {
			results[index].failure = tools.ExecutionError(callID, tools.StatusInvalidInput, "missing_fields", missing)
		}
	}
	done := 0
	document, err := store.mutate(call.task, by, reader.settings.MaxStorageBytes, func(document *codeContextDocument) error {
		changed := false
		if call.has["task_description"] && document.Description != call.taskDescription {
			document.Description, changed = call.taskDescription, true
		}
		for index, item := range items {
			if results[index].failure != "" {
				continue
			}
			if item.action == "update" {
				results[index] = applyCodeContextUpdate(document, item, reader, by)
			} else {
				results[index] = applyCodeContextRead(document, item, reader, by)
			}
			if results[index].failure == "" {
				done++
				changed = changed || results[index].changed
			}
		}
		switch {
		case len(items) > 0 && done == 0:
			return errCodeContextAllFailed
		case !changed:
			return errCodeContextNoChange
		}
		return nil
	})
	switch {
	case errors.Is(err, errCodeContextAllFailed):
		return codeContextAllFailed(callID, batch, results)
	case errors.Is(err, errCodeContextNoChange):
		document, err = store.load(call.task)
	}
	if err != nil {
		return codeContextStoreFailure(callID, err)
	}

	var parts []string
	if call.has["task_description"] {
		parts = append(parts, "task description set")
	}
	switch {
	case batch:
		parts = append(parts, fmt.Sprintf("%d of %d items done", done, len(items)))
	case len(items) == 1:
		parts = append(parts, results[0].summary)
	}
	if stale := codeContextStaleInFilesRead(document, results, reader); len(stale) > 0 {
		parts = append(parts, "stale in the files read: "+codeContextIDSpans(stale)+" (get them with ids for why)")
	}
	var output strings.Builder
	fmt.Fprintf(&output, "code_context task %s, generation %d: %s\n", call.task, document.Generation, strings.Join(parts, "; "))
	budget := reader.settings.MaxOutputBytes
	for index, result := range results {
		if batch {
			if result.failure != "" {
				failure := parseCodeContextItemFailure(result.failure)
				fmt.Fprintf(&output, "item %d failed (%s): %s\n", index+1, failure.code, failure.summary)
				continue
			}
			fmt.Fprintf(&output, "item %d: %s\n", index+1, result.summary)
		}
		budget -= writeCodeContextResult(&output, result, budget)
	}
	return output.String()
}

// codeContextStaleInFilesRead checks the task's entries of the files the call's items
// read against the bytes already read, and answers the stale ones' ids, smallest first.
// No file is read again and no other file is checked; a relocation is not stale, and
// get, list and find save it.
func codeContextStaleInFilesRead(document codeContextDocument, results []codeContextResult, reader codeContextReader) []int64 {
	checker := newCodeContextChecker(reader)
	for _, result := range results {
		if result.failure == "" && result.path != "" {
			if _, read := checker.files[result.path]; !read {
				checker.files[result.path] = &codeContextFile{data: result.data, sha256: codeContextSHA256(result.data)}
			}
		}
	}
	var entries []codeContextEntry
	for _, entry := range document.Entries {
		if _, read := checker.files[entry.Path]; read {
			entries = append(entries, entry)
		}
	}
	statuses := checker.checkAll(entries)
	settleCodeContextRelocations(document, statuses)
	var stale []int64
	for _, entry := range entries {
		if statuses[entry.ID].stale {
			stale = append(stale, entry.ID)
		}
	}
	slices.Sort(stale)
	return stale
}

// writeCodeContextResult writes an item's entry and, for a read that prints, its code
// while it fits budget bytes. It answers the code bytes printed.
func writeCodeContextResult(output *strings.Builder, result codeContextResult, budget int64) int64 {
	if !result.print {
		if result.kept {
			writeCodeContextEntry(output, result.entry, codeContextWhere(result.entry), "")
		}
		return 0
	}
	start, end := result.entry.Start, result.entry.End
	fmt.Fprintf(output, "=== %s lines %d-%d ===\n", result.path, start, end)
	if result.kept {
		writeCodeContextEntry(output, result.entry, fmt.Sprintf("lines %d-%d", start, end), "")
	}
	printed := int64(len(result.code))
	if printed > budget {
		hint := fmt.Sprintf("get it with ids [%d]", result.entry.ID)
		if !result.kept {
			hint = "read it again in another call"
		}
		fmt.Fprintf(output, "(code not printed: this call reached max_output_bytes; %s)\n", hint)
		return 0
	}
	writeCodeContextCode(output, result.code, codeContextToken())
	return printed
}

// codeContextAllFailed is the tool error of a call none of whose items was done.
func codeContextAllFailed(callID string, batch bool, results []codeContextResult) string {
	if !batch {
		return results[0].failure
	}
	information := make([]tools.Information, 0, len(results))
	for index, result := range results {
		failure := parseCodeContextItemFailure(result.failure)
		information = append(information, tools.Information{Key: fmt.Sprintf("item_%d", index+1), Value: failure.code + ": " + failure.summary})
	}
	first := parseCodeContextItemFailure(results[0].failure)
	return tools.ExecutionError(callID, first.status, "all_items_failed", fmt.Sprintf("none of the %d items was done; item 1: %s", len(results), first.summary), information...)
}

// applyCodeContextRead saves lines start..end. Without a description it prints them:
// lines an entry already holds unchanged are not saved again, and the exact range of a
// stale entry refreshes that entry. With a description the caller has seen the code, so
// it is saved, or the entry of the same range described, without printing it.
func applyCodeContextRead(document *codeContextDocument, call codeContextCall, reader codeContextReader, by string) codeContextResult {
	callID := reader.execution.CallID
	read, failure := reader.readRange(call.path, call.start, call.end)
	if failure != "" {
		return codeContextResult{failure: failure}
	}
	result := codeContextResult{code: read.content, path: read.path, data: read.data, print: !call.has["description"], kept: true}
	holds := func(entry codeContextEntry) bool {
		content, _, ok := codeContextLines(read.data, entry.Start, entry.End)
		return ok && codeContextSHA256(content) == entry.ContentSHA256
	}
	exact := slices.IndexFunc(document.Entries, func(entry codeContextEntry) bool {
		return entry.Path == read.path && entry.Start == call.start && entry.End == call.end
	})
	if exact >= 0 {
		entry := document.Entries[exact]
		if holds(entry) && !call.has["description"] && !call.has["priority"] {
			result.entry, result.summary = entry, fmt.Sprintf("already saved as #%d; get it with ids next time", entry.ID)
			return result
		}
		result.summary = fmt.Sprintf("#%d updated", entry.ID)
		if !holds(entry) && result.print {
			result.summary = fmt.Sprintf("#%d held other code; it now holds the code below, so check its description still holds", entry.ID)
		}
		read.store(&entry)
		setCodeContextFields(&entry, call)
		entry.UpdatedBy, entry.UpdatedAt, entry.Generation = by, codeContextTimestamp(), document.Generation
		document.Entries[exact] = entry
		result.entry, result.changed = entry, true
		return result
	}
	if result.print {
		for _, entry := range document.Entries {
			if entry.Path == read.path && entry.Start <= call.start && entry.End >= call.end && holds(entry) {
				result.entry = entry
				result.entry.Start, result.entry.End = call.start, call.end
				result.kept, result.summary = false, fmt.Sprintf("these lines are inside #%d (lines %d-%d); get it with ids next time", entry.ID, entry.Start, entry.End)
				return result
			}
		}
	}
	if int64(len(document.Entries)) >= reader.settings.MaxEntries {
		summary := fmt.Sprintf("the task already has %d entries; max_entries is %d", len(document.Entries), reader.settings.MaxEntries)
		if !result.print {
			return codeContextResult{failure: tools.ExecutionError(callID, tools.StatusInvalidInput, "too_many_entries", summary)}
		}
		result.entry = codeContextEntry{Start: call.start, End: call.end}
		result.kept, result.summary = false, "not saved: "+summary
		return result
	}
	entry := codeContextEntry{
		ID: document.NextID, Priority: reader.settings.DefaultPriority, Path: read.path, Start: call.start, End: call.end,
		UpdatedBy: by, UpdatedAt: codeContextTimestamp(), Generation: document.Generation,
	}
	read.store(&entry)
	setCodeContextFields(&entry, call)
	document.NextID++
	document.Entries = append(document.Entries, entry)
	result.entry, result.changed, result.summary = entry, true, fmt.Sprintf("#%d saved", entry.ID)
	if entry.Description == "" {
		result.summary += ", no description yet"
	}
	return result
}

// applyCodeContextUpdate changes entry id: its range, read again, its description and
// its priority. The id is kept.
func applyCodeContextUpdate(document *codeContextDocument, call codeContextCall, reader codeContextReader, by string) codeContextResult {
	callID := reader.execution.CallID
	index := slices.IndexFunc(document.Entries, func(entry codeContextEntry) bool { return entry.ID == call.id })
	if index < 0 {
		return codeContextResult{failure: tools.ExecutionError(callID, tools.StatusNotFound, "entry_not_found", fmt.Sprintf("task %s has no entry %d", call.task, call.id))}
	}
	entry := document.Entries[index]
	path, start, end := entry.Path, entry.Start, entry.End
	if call.has["path"] {
		path = call.path
	}
	if call.has["start"] {
		start = call.start
	}
	if call.has["end"] {
		end = call.end
	}
	read, failure := reader.readRange(path, start, end)
	if failure != "" {
		return codeContextResult{failure: failure}
	}
	for _, other := range document.Entries {
		if other.ID != entry.ID && other.Path == read.path && other.Start == start && other.End == end {
			return codeContextResult{failure: tools.ExecutionError(callID, tools.StatusInvalidInput, "duplicate_entry", fmt.Sprintf("entry %d already reads this range; drop one of them", other.ID))}
		}
	}
	entry.Path, entry.Start, entry.End = read.path, start, end
	read.store(&entry)
	setCodeContextFields(&entry, call)
	entry.UpdatedBy, entry.UpdatedAt, entry.Generation = by, codeContextTimestamp(), document.Generation
	document.Entries[index] = entry
	return codeContextResult{entry: entry, path: read.path, data: read.data, kept: true, changed: true, summary: fmt.Sprintf("#%d updated", entry.ID)}
}

// setCodeContextFields sets the description and priority the call gives.
func setCodeContextFields(entry *codeContextEntry, call codeContextCall) {
	if call.has["description"] {
		entry.Description = call.description
	}
	if call.has["priority"] {
		entry.Priority = call.priority
	}
}

// codeContextItemFailure is one item's tool error, read back from its rendered form.
type codeContextItemFailure struct {
	status  tools.Status
	code    string
	summary string
}

func parseCodeContextItemFailure(rendered string) codeContextItemFailure {
	failure := codeContextItemFailure{status: tools.StatusError, code: "failed", summary: oneLine(rendered)}
	header, rest, _ := strings.Cut(rendered, "\n")
	var fields struct {
		Status  tools.Status `json:"status"`
		Summary string       `json:"summary"`
	}
	if json.Unmarshal([]byte(header), &fields) != nil {
		return failure
	}
	failure.status, failure.summary = fields.Status, oneLine(fields.Summary)
	for _, line := range strings.Split(rest, "\n") {
		if code, found := strings.CutPrefix(line, "error_code: "); found {
			failure.code = code
			break
		}
	}
	return failure
}
