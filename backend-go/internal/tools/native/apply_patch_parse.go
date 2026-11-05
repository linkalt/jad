package native

import (
	"fmt"
	"strings"
)

// The patch format of apply_patch, as GPT models write it:
//
//	*** Begin Patch
//	*** Add File: path
//	+line
//	*** Delete File: path
//	*** Update File: path
//	*** Move to: new/path
//	@@ optional line to find first
//	 context
//	-removed
//	+added
//	*** End of File
//	*** End Patch
const (
	patchBegin      = "*** Begin Patch"
	patchEnd        = "*** End Patch"
	patchAddFile    = "*** Add File: "
	patchDeleteFile = "*** Delete File: "
	patchUpdateFile = "*** Update File: "
	patchMoveTo     = "*** Move to: "
	patchEndOfFile  = "*** End of File"
)

type patchOperationKind string

const (
	patchOperationAdd    patchOperationKind = "add"
	patchOperationDelete patchOperationKind = "delete"
	patchOperationUpdate patchOperationKind = "update"
)

// patchHunk is one "@@" section of an Update File: the lines to find (context and
// removed lines) and the lines that replace them (context and added lines).
type patchHunk struct {
	anchor    string
	oldLines  []string
	newLines  []string
	endOfFile bool
	line      int
}

type patchOperation struct {
	kind     patchOperationKind
	path     string
	moveTo   string
	addLines []string
	hunks    []patchHunk
	line     int
}

// patchSyntaxError names the patch line that could not be read.
type patchSyntaxError struct {
	line    int
	message string
}

func (e *patchSyntaxError) Error() string {
	return fmt.Sprintf("line %d: %s", e.line, e.message)
}

func patchError(line int, format string, arguments ...any) error {
	return &patchSyntaxError{line: line, message: fmt.Sprintf(format, arguments...)}
}

// parsePatch reads a whole patch. Every operation is read before any is applied, so a
// malformed patch changes nothing.
func parsePatch(text string) ([]patchOperation, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(strings.Trim(text, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != patchBegin {
		return nil, patchError(1, "the patch must start with %q", patchBegin)
	}
	last := len(lines) - 1
	if last == 0 || strings.TrimSpace(lines[last]) != patchEnd {
		return nil, patchError(len(lines), "the patch must end with %q", patchEnd)
	}
	var operations []patchOperation
	index := 1
	for index < last {
		line := lines[index]
		lineNumber := index + 1
		switch {
		case strings.TrimSpace(line) == "":
			index++
		case strings.HasPrefix(line, patchAddFile):
			operation := patchOperation{kind: patchOperationAdd, path: strings.TrimSpace(strings.TrimPrefix(line, patchAddFile)), line: lineNumber}
			index++
			for index < last && !isPatchHeader(lines[index]) {
				content := lines[index]
				if !strings.HasPrefix(content, "+") {
					return nil, patchError(index+1, "every line of an Add File must start with +")
				}
				operation.addLines = append(operation.addLines, content[1:])
				index++
			}
			operations = append(operations, operation)
		case strings.HasPrefix(line, patchDeleteFile):
			operations = append(operations, patchOperation{kind: patchOperationDelete, path: strings.TrimSpace(strings.TrimPrefix(line, patchDeleteFile)), line: lineNumber})
			index++
		case strings.HasPrefix(line, patchUpdateFile):
			operation := patchOperation{kind: patchOperationUpdate, path: strings.TrimSpace(strings.TrimPrefix(line, patchUpdateFile)), line: lineNumber}
			index++
			if index < last && strings.HasPrefix(lines[index], patchMoveTo) {
				operation.moveTo = strings.TrimSpace(strings.TrimPrefix(lines[index], patchMoveTo))
				if operation.moveTo == "" {
					return nil, patchError(index+1, "Move to needs a path")
				}
				index++
			}
			hunks, next, err := parsePatchHunks(lines, index, last)
			if err != nil {
				return nil, err
			}
			if len(hunks) == 0 && operation.moveTo == "" {
				return nil, patchError(lineNumber, "Update File %s has no hunk", operation.path)
			}
			operation.hunks = hunks
			index = next
			operations = append(operations, operation)
		default:
			return nil, patchError(lineNumber, "expected *** Add File, *** Delete File, *** Update File or *** End Patch, found %q", cutPatchLine(line))
		}
		if len(operations) > 0 && operations[len(operations)-1].path == "" {
			return nil, patchError(operations[len(operations)-1].line, "the file header needs a path")
		}
	}
	if len(operations) == 0 {
		return nil, patchError(1, "the patch changes no file")
	}
	return operations, nil
}

// parsePatchHunks reads the hunks of an Update File from index up to the next file
// header and returns the index after them.
func parsePatchHunks(lines []string, index, last int) ([]patchHunk, int, error) {
	var hunks []patchHunk
	var current *patchHunk
	finish := func() error {
		if current == nil {
			return nil
		}
		changed := len(current.oldLines) != len(current.newLines)
		for position := 0; !changed && position < len(current.oldLines); position++ {
			changed = current.oldLines[position] != current.newLines[position]
		}
		if !changed {
			return patchError(current.line, "the hunk changes nothing: it needs a - or + line")
		}
		hunks = append(hunks, *current)
		current = nil
		return nil
	}
	for index < last && !isPatchHeader(lines[index]) {
		line := lines[index]
		lineNumber := index + 1
		switch {
		case strings.HasPrefix(line, "@@"):
			if err := finish(); err != nil {
				return nil, 0, err
			}
			anchor := strings.TrimSpace(strings.TrimPrefix(line, "@@"))
			if strings.HasSuffix(anchor, "@@") {
				// A unified-diff range such as "@@ -1,3 +1,4 @@" names lines, not text.
				anchor = ""
			}
			current = &patchHunk{anchor: anchor, line: lineNumber}
		case strings.TrimSpace(line) == patchEndOfFile:
			if current == nil {
				return nil, 0, patchError(lineNumber, "*** End of File must follow a hunk")
			}
			current.endOfFile = true
		case line == "" || line[0] == ' ' || line[0] == '-' || line[0] == '+':
			if current == nil {
				if len(hunks) > 0 {
					return nil, 0, patchError(lineNumber, "a hunk after the first must start with @@")
				}
				current = &patchHunk{line: lineNumber}
			}
			if current.endOfFile {
				return nil, 0, patchError(lineNumber, "no line may follow *** End of File in a hunk")
			}
			marker, content := byte(' '), ""
			if line != "" {
				marker, content = line[0], line[1:]
			}
			switch marker {
			case ' ':
				current.oldLines = append(current.oldLines, content)
				current.newLines = append(current.newLines, content)
			case '-':
				current.oldLines = append(current.oldLines, content)
			case '+':
				current.newLines = append(current.newLines, content)
			}
		default:
			return nil, 0, patchError(lineNumber, "a hunk line must start with a space, - or +, found %q", cutPatchLine(line))
		}
		index++
	}
	if err := finish(); err != nil {
		return nil, 0, err
	}
	return hunks, index, nil
}

func isPatchHeader(line string) bool {
	return strings.HasPrefix(line, patchAddFile) || strings.HasPrefix(line, patchDeleteFile) ||
		strings.HasPrefix(line, patchUpdateFile) || strings.TrimSpace(line) == patchEnd
}

func cutPatchLine(line string) string {
	const limit = 80
	if len(line) <= limit {
		return line
	}
	return line[:limit] + "…"
}

// patchApplyError names the hunk that did not match.
type patchApplyError struct {
	hunk    int
	line    int
	message string
}

func (e *patchApplyError) Error() string { return e.message }

// applyPatchHunks applies the hunks of one Update File to text, in order: each hunk is
// looked for after the previous one. Lines match exactly first, then ignoring trailing
// whitespace, then ignoring surrounding whitespace. The file's line ending and final
// newline are kept.
func applyPatchHunks(text string, hunks []patchHunk) (string, error) {
	ending := "\n"
	if lineEnding, known := detectEditLineEnding(text); known && lineEnding == editLineEndingCRLF {
		ending = "\r\n"
	}
	trailingNewline := strings.HasSuffix(text, "\n")
	var lines []string
	if text != "" {
		lines = strings.Split(text, "\n")
		if trailingNewline {
			lines = lines[:len(lines)-1]
		}
		if ending == "\r\n" {
			for index, line := range lines {
				lines[index] = strings.TrimSuffix(line, "\r")
			}
		}
	}
	type replacement struct {
		start, length int
		lines         []string
	}
	var replacements []replacement
	cursor := 0
	for number, hunk := range hunks {
		if hunk.anchor != "" {
			found := seekPatchLines(lines, []string{hunk.anchor}, cursor, false)
			if found < 0 {
				return "", &patchApplyError{hunk: number + 1, line: hunk.line, message: fmt.Sprintf("hunk %d: the @@ line %q is not in the file", number+1, cutPatchLine(hunk.anchor))}
			}
			cursor = found + 1
		}
		oldLines, newLines := hunk.oldLines, hunk.newLines
		if len(oldLines) == 0 {
			at := len(lines)
			if hunk.anchor != "" {
				at = cursor
			}
			replacements = append(replacements, replacement{start: at, lines: newLines})
			cursor = at
			continue
		}
		found := seekPatchLines(lines, oldLines, cursor, hunk.endOfFile)
		if found < 0 && oldLines[len(oldLines)-1] == "" {
			// A trailing blank context line often stands for the end of the file.
			oldLines = oldLines[:len(oldLines)-1]
			if len(newLines) > 0 && newLines[len(newLines)-1] == "" {
				newLines = newLines[:len(newLines)-1]
			}
			if len(oldLines) > 0 {
				found = seekPatchLines(lines, oldLines, cursor, hunk.endOfFile)
			}
		}
		if found < 0 {
			return "", &patchApplyError{hunk: number + 1, line: hunk.line, message: fmt.Sprintf("hunk %d: its context and - lines are not in the file after the previous hunk (first line %q)", number+1, cutPatchLine(hunk.oldLines[0]))}
		}
		replacements = append(replacements, replacement{start: found, length: len(oldLines), lines: newLines})
		cursor = found + len(oldLines)
	}
	for index := len(replacements) - 1; index >= 0; index-- {
		change := replacements[index]
		next := make([]string, 0, len(lines)-change.length+len(change.lines))
		next = append(next, lines[:change.start]...)
		next = append(next, change.lines...)
		next = append(next, lines[change.start+change.length:]...)
		lines = next
	}
	result := strings.Join(lines, ending)
	if len(lines) > 0 && (trailingNewline || text == "") {
		result += ending
	}
	return result, nil
}

// seekPatchLines returns where pattern starts in lines at or after start, or -1. With
// endOfFile the end of the file is tried first.
func seekPatchLines(lines, pattern []string, start int, endOfFile bool) int {
	if len(pattern) > len(lines) {
		return -1
	}
	normalizers := []func(string) string{
		func(line string) string { return line },
		func(line string) string { return strings.TrimRight(line, " \t") },
		strings.TrimSpace,
	}
	for _, normalize := range normalizers {
		matches := func(at int) bool {
			for offset, want := range pattern {
				if normalize(lines[at+offset]) != normalize(want) {
					return false
				}
			}
			return true
		}
		if endOfFile {
			if at := len(lines) - len(pattern); at >= start && matches(at) {
				return at
			}
		}
		for at := start; at+len(pattern) <= len(lines); at++ {
			if matches(at) {
				return at
			}
		}
	}
	return -1
}
