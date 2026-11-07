package native

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// selectCodeContextEntries picks the entries a call names: those of ids (all without),
// of maxPriority or a lower number (all without). It answers them by priority, and the
// ids that name no entry.
func selectCodeContextEntries(document codeContextDocument, ids []int64, maxPriority int64) ([]codeContextEntry, []int64) {
	selected := document.Entries
	var missing []int64
	if len(ids) > 0 {
		selected = nil
		for _, id := range ids {
			index := slices.IndexFunc(document.Entries, func(entry codeContextEntry) bool { return entry.ID == id })
			if index < 0 {
				missing = append(missing, id)
				continue
			}
			selected = append(selected, document.Entries[index])
		}
	}
	if maxPriority > 0 {
		var kept []codeContextEntry
		for _, entry := range selected {
			if entry.Priority <= maxPriority {
				kept = append(kept, entry)
			}
		}
		selected = kept
	}
	return sortedCodeContextEntries(selected), missing
}

func sortedCodeContextEntries(entries []codeContextEntry) []codeContextEntry {
	sorted := append([]codeContextEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority < sorted[j].Priority
		}
		return sorted[i].ID < sorted[j].ID
	})
	return sorted
}

// codeContextChecked is the selected entries of a call checked against their files, with
// the ranges of relocated entries already moved.
type codeContextChecked struct {
	document  codeContextDocument
	entries   []codeContextEntry
	statuses  map[int64]codeContextStatus
	relocated int
	note      string
}

// checkCodeContextTask checks the entries, keeps the relocations no other entry
// contradicts and saves them.
func checkCodeContextTask(task string, reader codeContextReader, store codeContextStore, by string, document codeContextDocument, selected []codeContextEntry) codeContextChecked {
	statuses := newCodeContextChecker(reader).checkAll(selected)
	settleCodeContextRelocations(document, statuses)
	document, _, note := saveCodeContextRelocations(store, task, by, reader.settings.MaxStorageBytes, document, statuses)
	checked := codeContextChecked{document: document, statuses: statuses, note: note, entries: make([]codeContextEntry, len(selected))}
	for index, entry := range selected {
		if status := statuses[entry.ID]; status.relocated {
			entry.Start, entry.End = status.start, status.end
			checked.relocated++
		}
		checked.entries[index] = entry
	}
	return checked
}

func codeContextList(call codeContextCall, reader codeContextReader, store codeContextStore, by string) string {
	document, err := store.load(call.task)
	if err != nil {
		return codeContextStoreFailure(reader.execution.CallID, err)
	}
	selected, _ := selectCodeContextEntries(document, nil, call.maxPriority)
	checked := checkCodeContextTask(call.task, reader, store, by, document, selected)
	stats := store.loadState(call.task).Stats
	output := renderCodeContextYAML(checked.document, checked.entries, checked.statuses, &stats)
	if checked.note != "" {
		output = "# " + checked.note + "\n" + output
	}
	return output
}

// renderCodeContextYAML writes the entries as YAML. statuses holds the checked entries;
// an entry without one is not given a status. stats, when given, is written before them.
func renderCodeContextYAML(document codeContextDocument, entries []codeContextEntry, statuses map[int64]codeContextStatus, stats *codeContextStats) string {
	var output strings.Builder
	fmt.Fprintf(&output, "task: %s\ngeneration: %d\n", yamlString(document.Task), document.Generation)
	if document.LastUpdatedBy != "" {
		fmt.Fprintf(&output, "last_updated_by: %s\n", yamlString(document.LastUpdatedBy))
	}
	if stats != nil {
		fmt.Fprintf(&output, "stats:\n  gets: %d\n  code_bytes_printed: %d\n  code_bytes_not_repeated: %d\n  entries_relocated: %d\n  stale_seen: %d\n  reads: %d\n  reads_already_saved: %d\n  finds: %d\n  finds_without_match: %d\n",
			stats.Gets, stats.CodeBytesPrinted, stats.CodeBytesNotRepeated, stats.EntriesRelocated, stats.StaleSeen, stats.Reads, stats.ReadsAlreadySaved, stats.Finds, stats.FindsWithoutMatch)
	}
	if len(entries) == 0 {
		output.WriteString("entries: []\n")
		return output.String()
	}
	output.WriteString("entries:\n")
	for _, entry := range entries {
		fmt.Fprintf(&output, "  - id: %d\n    priority: %d\n", entry.ID, entry.Priority)
		if status, checked := statuses[entry.ID]; checked {
			if status.stale {
				fmt.Fprintf(&output, "    stale: true\n    stale_reason: %s\n", yamlString(status.reason))
			} else {
				output.WriteString("    stale: false\n")
			}
			if status.relocated {
				fmt.Fprintf(&output, "    moved_from: read(%s, %d, %d)\n", yamlString(entry.Path), status.fromStart, status.fromEnd)
			}
		}
		fmt.Fprintf(&output, "    command: read(%s, %d, %d)\n", yamlString(entry.Path), entry.Start, entry.End)
		if entry.Description == "" {
			output.WriteString("    description: \"\"\n    undescribed: true\n")
		} else {
			output.WriteString("    description: |-\n")
			for _, line := range strings.Split(entry.Description, "\n") {
				if line == "" {
					output.WriteString("\n")
					continue
				}
				output.WriteString("      " + line + "\n")
			}
		}
		fmt.Fprintf(&output, "    updated_by: %s\n    generation: %d\n    file_sha256: %s\n    content_sha256: %s\n",
			yamlString(entry.UpdatedBy), entry.Generation, entry.FileSHA256, entry.ContentSHA256)
	}
	return output.String()
}

// codeContextBlock is the code printed once for one or more current entries of one file
// whose ranges overlap or touch.
type codeContextBlock struct {
	path       string
	start, end int64
	priority   int64
	entries    []codeContextEntry
	content    []byte
}

func codeContextGet(call codeContextCall, reader codeContextReader, store codeContextStore, by string) string {
	document, err := store.load(call.task)
	if err != nil {
		return codeContextStoreFailure(reader.execution.CallID, err)
	}
	selected, missing := selectCodeContextEntries(document, call.ids, call.maxPriority)
	checked := checkCodeContextTask(call.task, reader, store, by, document, selected)
	document, statuses := checked.document, checked.statuses
	var stale, current []codeContextEntry
	for _, entry := range checked.entries {
		if statuses[entry.ID].stale {
			stale = append(stale, entry)
			continue
		}
		current = append(current, entry)
	}
	var notes []string
	if checked.note != "" {
		notes = append(notes, checked.note)
	}
	if len(missing) > 0 {
		notes = append(notes, "no entry "+joinCodeContextIDs(missing))
	}
	if len(document.Entries) == 0 {
		notes = append(notes, "the task has no entries; save ranges with action read or add")
	}
	if call.mode == "outline" {
		return codeContextOutline(call, reader, store, document, checked, stale, notes)
	}

	// Entries the caller holds unchanged since the delivery it names are not printed again.
	var unchanged []codeContextEntry
	if call.has["since"] {
		state := store.loadState(call.task)
		index := slices.IndexFunc(state.Deliveries, func(delivery codeContextDelivery) bool { return delivery.Token == call.since })
		if index < 0 {
			notes = append(notes, "since "+call.since+" is unknown or expired; every entry is printed")
		} else {
			held := state.Deliveries[index].Entries
			var kept []codeContextEntry
			for _, entry := range current {
				if fingerprint, exists := held[entry.ID]; exists && fingerprint == codeContextFingerprint(entry) {
					unchanged = append(unchanged, entry)
					continue
				}
				kept = append(kept, entry)
			}
			current = kept
		}
	}

	byPath := map[string][]codeContextEntry{}
	for _, entry := range current {
		byPath[entry.Path] = append(byPath[entry.Path], entry)
	}
	blocks := mergeCodeContextBlocks(byPath, statuses)
	token := codeContextToken()
	var shown []string
	var shownBlocks, left []codeContextBlock
	var used int64
	cutoff := int64(0)
	for _, block := range blocks {
		rendered := renderCodeContextBlock(block, statuses, token)
		if cutoff == 0 || block.priority <= cutoff {
			if used+int64(len(rendered)) <= reader.settings.MaxOutputBytes {
				shown = append(shown, rendered)
				shownBlocks = append(shownBlocks, block)
				used += int64(len(rendered))
				continue
			}
			if cutoff == 0 {
				cutoff = block.priority
			}
		}
		left = append(left, block)
	}

	// The since token is apart from the code markers, so knowing one never helps forge the other.
	sinceToken := codeContextToken()
	delivered := map[int64]string{}
	var printedBytes, notRepeatedBytes int64
	shownEntries, leftEntries := 0, 0
	for _, block := range shownBlocks {
		printedBytes += int64(len(block.content))
		shownEntries += len(block.entries)
		for _, entry := range block.entries {
			delivered[entry.ID] = codeContextFingerprint(entry)
		}
	}
	for _, block := range left {
		leftEntries += len(block.entries)
	}
	for _, entry := range unchanged {
		notRepeatedBytes += int64(len(statuses[entry.ID].content))
		delivered[entry.ID] = codeContextFingerprint(entry)
	}
	stateErr := store.mutateState(call.task, reader.settings.MaxDeliveries, reader.settings.MaxStorageBytes, func(state *codeContextState) {
		if len(delivered) > 0 {
			state.Deliveries = append(state.Deliveries, codeContextDelivery{Token: sinceToken, Generation: document.Generation, CreatedAt: codeContextTimestamp(), Entries: delivered})
		}
		state.Stats.Gets++
		state.Stats.CodeBytesPrinted += printedBytes
		state.Stats.CodeBytesNotRepeated += notRepeatedBytes
		state.Stats.EntriesRelocated += int64(checked.relocated)
		state.Stats.StaleSeen += int64(len(stale))
	})

	var output strings.Builder
	fmt.Fprintf(&output, "code_context task %s, generation %d: %d entries shown", call.task, document.Generation, shownEntries)
	if len(unchanged) > 0 {
		fmt.Fprintf(&output, ", %d unchanged since %s", len(unchanged), call.since)
	}
	if checked.relocated > 0 {
		fmt.Fprintf(&output, ", %d relocated", checked.relocated)
	}
	if len(stale) > 0 {
		fmt.Fprintf(&output, ", %d stale", len(stale))
	}
	if leftEntries > 0 {
		fmt.Fprintf(&output, ", %d left out by max_output_bytes", leftEntries)
	}
	fmt.Fprintf(&output, "\ncode bytes: %d printed", printedBytes)
	if notRepeatedBytes > 0 {
		fmt.Fprintf(&output, ", %d not printed again", notRepeatedBytes)
	}
	output.WriteString("\n")
	switch {
	case stateErr != nil:
		notes = append(notes, "no since token: "+oneLine(stateErr.Error()))
	case len(delivered) > 0:
		fmt.Fprintf(&output, "since: %s (pass it to a later get to print only what changed)\n", sinceToken)
	}
	for _, note := range notes {
		output.WriteString(note + "\n")
	}
	for _, rendered := range shown {
		output.WriteString("\n" + rendered)
	}
	if len(unchanged) > 0 {
		fmt.Fprintf(&output, "\n=== %d entries unchanged since %s: their code and description are as that get printed them ===\n", len(unchanged), call.since)
		for _, entry := range unchanged {
			fmt.Fprintf(&output, "- #%d  %s %d-%d  (priority %d%s)\n", entry.ID, entry.Path, entry.Start, entry.End, entry.Priority, codeContextMoved(statuses[entry.ID]))
		}
	}
	writeCodeContextStale(&output, stale, statuses)
	if leftEntries > 0 {
		output.WriteString("\n=== left out by max_output_bytes: get them with ids ===\n")
		for _, block := range left {
			for _, entry := range block.entries {
				fmt.Fprintf(&output, "- id %d  %s %d-%d  (priority %d)\n", entry.ID, entry.Path, entry.Start, entry.End, entry.Priority)
			}
		}
	}
	return output.String()
}

// codeContextOutline answers get with mode outline: the descriptions and ranges, no code.
func codeContextOutline(call codeContextCall, reader codeContextReader, store codeContextStore, document codeContextDocument, checked codeContextChecked, stale []codeContextEntry, notes []string) string {
	stateErr := store.mutateState(call.task, reader.settings.MaxDeliveries, reader.settings.MaxStorageBytes, func(state *codeContextState) {
		state.Stats.Gets++
		state.Stats.EntriesRelocated += int64(checked.relocated)
		state.Stats.StaleSeen += int64(len(stale))
	})
	if stateErr != nil {
		notes = append(notes, "stats not saved: "+oneLine(stateErr.Error()))
	}
	var output strings.Builder
	fmt.Fprintf(&output, "code_context task %s, generation %d: outline of %d entries", call.task, document.Generation, len(checked.entries))
	if checked.relocated > 0 {
		fmt.Fprintf(&output, ", %d relocated", checked.relocated)
	}
	if len(stale) > 0 {
		fmt.Fprintf(&output, ", %d stale", len(stale))
	}
	output.WriteString("; no code: get it with ids\n")
	for _, note := range notes {
		output.WriteString(note + "\n")
	}
	for _, entry := range checked.entries {
		status := checked.statuses[entry.ID]
		state := codeContextMoved(status)
		if status.stale {
			state = ", stale: " + status.reason
		}
		fmt.Fprintf(&output, "\n#%d  %s %d-%d  (priority %d, updated by %s%s)\n", entry.ID, entry.Path, entry.Start, entry.End, entry.Priority, entry.UpdatedBy, state)
		output.WriteString(codeContextDescriptionText(entry) + "\n")
	}
	return output.String()
}

func writeCodeContextStale(output *strings.Builder, stale []codeContextEntry, statuses map[int64]codeContextStatus) {
	if len(stale) == 0 {
		return
	}
	fmt.Fprintf(output, "\n=== %d stale entries: their code is not shown. Re-read the code (action read of the same range refreshes an entry), then update each entry (new start, end and, if the meaning changed, description) or drop it ===\n", len(stale))
	for _, entry := range stale {
		fmt.Fprintf(output, "- id %d  %s  (was %d-%d, priority %d): %s\n", entry.ID, entry.Path, entry.Start, entry.End, entry.Priority, statuses[entry.ID].reason)
		output.WriteString(codeContextDescriptionText(entry) + "\n")
	}
}

// codeContextMoved notes where a relocated entry was.
func codeContextMoved(status codeContextStatus) string {
	if !status.relocated {
		return ""
	}
	return fmt.Sprintf(", moved from %d-%d", status.fromStart, status.fromEnd)
}

// codeContextDescriptionText is an entry's description indented for get, or the hint
// that it has none yet.
func codeContextDescriptionText(entry codeContextEntry) string {
	if entry.Description == "" {
		return "  (no description yet: add one with update)"
	}
	return indentCodeContext(entry.Description, "  ")
}

// mergeCodeContextBlocks joins the current entries of each file whose ranges overlap or
// touch into one block, ordered by the block's best priority, then path and line.
func mergeCodeContextBlocks(byPath map[string][]codeContextEntry, statuses map[int64]codeContextStatus) []codeContextBlock {
	var blocks []codeContextBlock
	for path, entries := range byPath {
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].Start != entries[j].Start {
				return entries[i].Start < entries[j].Start
			}
			return entries[i].End < entries[j].End
		})
		var group []codeContextBlock
		for _, entry := range entries {
			if last := len(group) - 1; last >= 0 && entry.Start <= group[last].end+1 {
				block := &group[last]
				block.entries = append(block.entries, entry)
				block.end = max(block.end, entry.End)
				block.priority = min(block.priority, entry.Priority)
				continue
			}
			group = append(group, codeContextBlock{path: path, start: entry.Start, end: entry.End, priority: entry.Priority, entries: []codeContextEntry{entry}})
		}
		for index := range group {
			block := &group[index]
			if len(block.entries) == 1 {
				block.content = statuses[block.entries[0].ID].content
				continue
			}
			block.content = joinCodeContextContent(*block, statuses)
		}
		blocks = append(blocks, group...)
	}
	sort.SliceStable(blocks, func(i, j int) bool {
		if blocks[i].priority != blocks[j].priority {
			return blocks[i].priority < blocks[j].priority
		}
		if blocks[i].path != blocks[j].path {
			return blocks[i].path < blocks[j].path
		}
		return blocks[i].start < blocks[j].start
	})
	return blocks
}

// joinCodeContextContent builds a merged block's lines from its entries' checked bytes,
// so every printed line is one some entry verified. Entries are sorted by start.
func joinCodeContextContent(block codeContextBlock, statuses map[int64]codeContextStatus) []byte {
	var content []byte
	next := block.start
	for _, entry := range block.entries {
		if entry.End < next {
			continue
		}
		lines := splitCodeContextLines(statuses[entry.ID].content)
		skip := next - entry.Start
		for _, line := range lines[skip:] {
			content = append(content, line...)
		}
		next = entry.End + 1
	}
	return content
}

// splitCodeContextLines splits bytes after each newline, keeping the newlines.
func splitCodeContextLines(content []byte) [][]byte {
	var lines [][]byte
	for len(content) > 0 {
		index := bytes.IndexByte(content, '\n')
		if index < 0 {
			lines = append(lines, content)
			break
		}
		lines = append(lines, content[:index+1])
		content = content[index+1:]
	}
	return lines
}

func renderCodeContextBlock(block codeContextBlock, statuses map[int64]codeContextStatus, token string) string {
	var output strings.Builder
	fmt.Fprintf(&output, "=== %s lines %d-%d ===\n", block.path, block.start, block.end)
	for _, entry := range block.entries {
		fmt.Fprintf(&output, "#%d (priority %d, lines %d-%d, updated by %s%s)\n", entry.ID, entry.Priority, entry.Start, entry.End, entry.UpdatedBy, codeContextMoved(statuses[entry.ID]))
		output.WriteString(codeContextDescriptionText(entry) + "\n")
	}
	writeCodeContextCode(&output, block.content, token)
	return output.String()
}

// writeCodeContextCode writes code between the call's markers.
func writeCodeContextCode(output *strings.Builder, content []byte, token string) {
	fmt.Fprintf(output, "--- code %s ---\n", token)
	output.Write(content)
	if len(content) > 0 && content[len(content)-1] != '\n' {
		output.WriteString("\n")
	}
	fmt.Fprintf(output, "--- end code %s ---\n", token)
}
