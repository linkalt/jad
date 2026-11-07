package native

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// selectCodeContextEntries picks the entries of ids (all without), by priority, and the
// ids that name no entry.
func selectCodeContextEntries(document codeContextDocument, ids []int64) ([]codeContextEntry, []int64) {
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
	stale     int
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
		status := statuses[entry.ID]
		if status.relocated {
			entry.Start, entry.End = status.start, status.end
			checked.relocated++
		}
		if status.stale {
			checked.stale++
		}
		checked.entries[index] = entry
	}
	return checked
}

// counts is ", N relocated, M stale" for what the check found.
func (c codeContextChecked) counts() string {
	text := ""
	if c.relocated > 0 {
		text += fmt.Sprintf(", %d relocated", c.relocated)
	}
	if c.stale > 0 {
		text += fmt.Sprintf(", %d stale", c.stale)
	}
	return text
}

// codeContextList answers list with a task: one line per entry, by priority, checked
// against its file, and its description.
func codeContextList(call codeContextCall, reader codeContextReader, store codeContextStore, by string) string {
	document, err := store.load(call.task)
	if err != nil {
		return codeContextStoreFailure(reader.execution.CallID, err)
	}
	selected, _ := selectCodeContextEntries(document, nil)
	checked := checkCodeContextTask(call.task, reader, store, by, document, selected)
	var output strings.Builder
	fmt.Fprintf(&output, "code_context task %s, generation %d: ", call.task, checked.document.Generation)
	if len(selected) == 0 {
		output.WriteString("no entries; save ranges with read")
	} else {
		fmt.Fprintf(&output, "%d entries%s", len(selected), checked.counts())
	}
	output.WriteString(codeContextTaskDescriptionLine(checked.document) + "\n")
	if checked.note != "" {
		output.WriteString(checked.note + "\n")
	}
	for _, entry := range checked.entries {
		writeCodeContextEntry(&output, entry, codeContextWhere(entry), codeContextNote(checked.statuses[entry.ID]))
	}
	return output.String()
}

// writeCodeContextEntry writes one entry: "#id p<priority> where [note]", then its
// description, the first line on the same line and the rest indented below.
func writeCodeContextEntry(output *strings.Builder, entry codeContextEntry, where, note string) {
	fmt.Fprintf(output, "#%d p%d %s", entry.ID, entry.Priority, where)
	if note != "" {
		output.WriteString(" [" + note + "]")
	}
	if entry.Description == "" {
		output.WriteString("  (no description)\n")
		return
	}
	first, rest, _ := strings.Cut(entry.Description, "\n")
	output.WriteString("  " + first + "\n")
	if rest != "" {
		output.WriteString(indentCodeContext(rest, "  ") + "\n")
	}
}

// codeContextWhere is an entry's range as path:start-end.
func codeContextWhere(entry codeContextEntry) string {
	return fmt.Sprintf("%s:%d-%d", entry.Path, entry.Start, entry.End)
}

// codeContextNote says what a check found of an entry: stale and why, or where it moved
// from; nothing for an entry that is where it was.
func codeContextNote(status codeContextStatus) string {
	switch {
	case status.stale:
		return "stale: " + status.reason
	case status.relocated:
		return fmt.Sprintf("moved from %d-%d", status.fromStart, status.fromEnd)
	}
	return ""
}

// codeContextBlock is the code printed once for one or more current entries of one file
// whose ranges overlap or touch.
type codeContextBlock struct {
	path       string
	start, end int64
	// whole is the merged range when a get range printed only part of it, else zero.
	whole    [2]int64
	priority int64
	entries  []codeContextEntry
	content  []byte
}

// codeContextSpan is the part of the saved code a get prints: the entries of path when
// path is set, and lines start..end of them; zero start and end are the whole range.
type codeContextSpan struct {
	path       string
	start, end int64
}

func newCodeContextSpan(call codeContextCall, workspaceRoot string) codeContextSpan {
	span := codeContextSpan{}
	if call.has["path"] {
		path := call.path
		if !filepath.IsAbs(path) {
			path = filepath.Join(workspaceRoot, path)
		}
		span.path = displayPath(workspaceRoot, filepath.Clean(path))
	}
	if call.has["start"] {
		span.start = call.start
	}
	if call.has["end"] {
		span.end = call.end
	}
	return span
}

func (s codeContextSpan) lines() bool { return s.start > 0 || s.end > 0 }

// overlaps reports whether an entry's range has a line the span prints.
func (s codeContextSpan) overlaps(entry codeContextEntry) bool {
	return (s.start == 0 || entry.End >= s.start) && (s.end == 0 || entry.Start <= s.end)
}

// String is how the header names the span: " in path lines a-b".
func (s codeContextSpan) String() string {
	text := ""
	if s.path != "" {
		text = " in " + s.path
	}
	if s.lines() {
		text += " " + s.linesText()
	}
	return text
}

func (s codeContextSpan) linesText() string {
	switch {
	case s.start > 0 && s.end > 0:
		return fmt.Sprintf("lines %d-%d", s.start, s.end)
	case s.start > 0:
		return fmt.Sprintf("lines %d-end", s.start)
	}
	return fmt.Sprintf("lines 1-%d", s.end)
}

// missing is the note of a get whose span matched no entry.
func (s codeContextSpan) missing(ids []int64) string {
	text := "no entry"
	if len(ids) > 0 {
		text += " of ids " + joinCodeContextIDs(ids)
	}
	if s.path != "" {
		text += " in " + s.path
	}
	if s.lines() {
		text += " holds " + s.linesText()
	}
	return text
}

// clip cuts a block to the span's lines, keeping its merged range in whole.
func (s codeContextSpan) clip(block codeContextBlock) codeContextBlock {
	start, end := block.start, block.end
	if s.start > start {
		start = s.start
	}
	if s.end > 0 && s.end < end {
		end = s.end
	}
	if start == block.start && end == block.end {
		return block
	}
	lines := splitCodeContextLines(block.content)
	var content []byte
	for _, line := range lines[start-block.start : end-block.start+1] {
		content = append(content, line...)
	}
	block.whole = [2]int64{block.start, block.end}
	block.start, block.end, block.content = start, end, content
	return block
}

func codeContextGet(call codeContextCall, reader codeContextReader, store codeContextStore, by string) string {
	document, err := store.load(call.task)
	if err != nil {
		return codeContextStoreFailure(reader.execution.CallID, err)
	}
	selected, missing := selectCodeContextEntries(document, call.ids)
	span := newCodeContextSpan(call, reader.execution.WorkspaceRoot)
	if span.path != "" {
		selected = slices.DeleteFunc(selected, func(entry codeContextEntry) bool { return entry.Path != span.path })
	}
	checked := checkCodeContextTask(call.task, reader, store, by, document, selected)
	document, statuses := checked.document, checked.statuses
	// The lines are matched after the check, against the ranges as relocated.
	if span.lines() {
		checked.entries = slices.DeleteFunc(checked.entries, func(entry codeContextEntry) bool { return !span.overlaps(entry) })
		checked.relocated, checked.stale = 0, 0
		for _, entry := range checked.entries {
			if statuses[entry.ID].relocated {
				checked.relocated++
			}
			if statuses[entry.ID].stale {
				checked.stale++
			}
		}
	}
	var stale []codeContextEntry
	byPath := map[string][]codeContextEntry{}
	for _, entry := range checked.entries {
		if statuses[entry.ID].stale {
			stale = append(stale, entry)
			continue
		}
		byPath[entry.Path] = append(byPath[entry.Path], entry)
	}
	var notes []string
	if checked.note != "" {
		notes = append(notes, checked.note)
	}
	if len(missing) > 0 {
		notes = append(notes, "no entry "+joinCodeContextIDs(missing))
	}
	switch {
	case len(document.Entries) == 0:
		notes = append(notes, "the task has no entries; save ranges with read")
	case len(checked.entries) == 0 && (span.path != "" || span.lines()):
		notes = append(notes, span.missing(call.ids))
	}

	token := codeContextToken()
	var shown []string
	var left []codeContextBlock
	var used int64
	cutoff := int64(0)
	shownEntries, leftEntries := 0, 0
	for _, block := range mergeCodeContextBlocks(byPath, statuses) {
		block = span.clip(block)
		rendered := renderCodeContextBlock(block, statuses, token)
		if cutoff == 0 || block.priority <= cutoff {
			if used+int64(len(rendered)) <= reader.settings.MaxOutputBytes {
				shown = append(shown, rendered)
				shownEntries += len(block.entries)
				used += int64(len(rendered))
				continue
			}
			if cutoff == 0 {
				cutoff = block.priority
			}
		}
		left = append(left, block)
		leftEntries += len(block.entries)
	}

	var output strings.Builder
	fmt.Fprintf(&output, "code_context task %s, generation %d: %d entries shown%s%s", call.task, document.Generation, shownEntries, span, checked.counts())
	if leftEntries > 0 {
		fmt.Fprintf(&output, ", %d left out by max_output_bytes", leftEntries)
	}
	output.WriteString(codeContextTaskDescriptionLine(document) + "\n")
	for _, note := range notes {
		output.WriteString(note + "\n")
	}
	for _, rendered := range shown {
		output.WriteString("\n" + rendered)
	}
	if len(stale) > 0 {
		fmt.Fprintf(&output, "\n=== %d stale entries: their code is not shown. Re-read each range (read of the same range refreshes its entry), then update or drop it ===\n", len(stale))
		for _, entry := range stale {
			writeCodeContextEntry(&output, entry, codeContextWhere(entry), codeContextNote(statuses[entry.ID]))
		}
	}
	if leftEntries > 0 {
		output.WriteString("\n=== left out by max_output_bytes: get them with ids, or part of one with ids, start and end ===\n")
		for _, block := range left {
			for _, entry := range block.entries {
				fmt.Fprintf(&output, "#%d p%d %s\n", entry.ID, entry.Priority, codeContextWhere(entry))
			}
		}
	}
	return output.String()
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
	fmt.Fprintf(&output, "=== %s lines %d-%d", block.path, block.start, block.end)
	if block.whole != [2]int64{} {
		fmt.Fprintf(&output, " of %d-%d", block.whole[0], block.whole[1])
	}
	output.WriteString(" ===\n")
	for _, entry := range block.entries {
		writeCodeContextEntry(&output, entry, fmt.Sprintf("lines %d-%d", entry.Start, entry.End), codeContextNote(statuses[entry.ID]))
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
