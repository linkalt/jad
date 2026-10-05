package native

import (
	"strings"
	"unicode/utf8"
)

const maxEditDiagnosticRunes = 200

type editLineEnding string

const (
	editLineEndingLF    editLineEnding = "lf"
	editLineEndingCRLF  editLineEnding = "crlf"
	editLineEndingCR    editLineEnding = "cr"
	editLineEndingMixed editLineEnding = "mixed"
)

type editMatchDiagnostic struct {
	nearestLine    int
	matchedLines   int
	expected       string
	actual         string
	whitespaceOnly bool
}

func decodeEditUTF8(data []byte) (string, bool) {
	if !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}

func detectEditLineEnding(text string) (editLineEnding, bool) {
	var lf, crlf, cr bool
	for index := 0; index < len(text); index++ {
		switch text[index] {
		case '\n':
			if index > 0 && text[index-1] == '\r' {
				crlf = true
			} else {
				lf = true
			}
		case '\r':
			if index+1 == len(text) || text[index+1] != '\n' {
				cr = true
			}
		}
	}
	styles := 0
	for _, present := range []bool{lf, crlf, cr} {
		if present {
			styles++
		}
	}
	if styles == 0 {
		return "", false
	}
	if styles > 1 {
		return editLineEndingMixed, true
	}
	if crlf {
		return editLineEndingCRLF, true
	}
	if cr {
		return editLineEndingCR, true
	}
	return editLineEndingLF, true
}

func withEditLineEnding(text string, ending editLineEnding, known bool) string {
	if !known || ending == editLineEndingMixed {
		return text
	}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	switch ending {
	case editLineEndingCRLF:
		return strings.ReplaceAll(normalized, "\n", "\r\n")
	case editLineEndingCR:
		return strings.ReplaceAll(normalized, "\n", "\r")
	default:
		return normalized
	}
}

func countEditOccurrences(text, search string) int {
	if search == "" {
		return 0
	}
	count := 0
	for position := 0; ; {
		index := strings.Index(text[position:], search)
		if index < 0 {
			return count
		}
		position += index + len(search)
		count++
	}
}

func editOccurrenceLineNumbers(text, search string, limit int) []int {
	lines := make([]int, 0, limit)
	if search == "" || limit < 1 {
		return lines
	}
	line, scanned := 1, 0
	for position := 0; len(lines) < limit; {
		index := strings.Index(text[position:], search)
		if index < 0 {
			break
		}
		index += position
		line += strings.Count(text[scanned:index], "\n")
		lines = append(lines, line)
		scanned = index
		position = index + len(search)
	}
	return lines
}

func replaceEditOccurrences(text, search, replacement string, limit int) (string, int, int, bool) {
	if search == "" || limit < 1 {
		return text, 0, 0, false
	}
	var output strings.Builder
	output.Grow(len(text))
	count, cursor, firstIndex := 0, 0, 0
	foundFirst := false
	for {
		index := strings.Index(text[cursor:], search)
		if index < 0 || count >= limit {
			output.WriteString(text[cursor:])
			break
		}
		index += cursor
		if !foundFirst {
			firstIndex = index
			foundFirst = true
		}
		output.WriteString(text[cursor:index])
		output.WriteString(replacement)
		cursor = index + len(search)
		count++
	}
	return output.String(), count, firstIndex, foundFirst
}

func lineNumberAt(text string, position int) int {
	if position < 0 {
		position = 0
	}
	if position > len(text) {
		position = len(text)
	}
	return 1 + strings.Count(text[:position], "\n")
}

func insertedEditLineSpan(text string) int {
	if text == "" {
		return 0
	}
	newlines := strings.Count(text, "\n")
	if strings.HasSuffix(text, "\n") && newlines > 0 {
		return newlines - 1
	}
	return newlines
}

func diagnoseEditMatch(text, oldText string) *editMatchDiagnostic {
	expectedLines := strings.Split(oldText, "\n")
	anchor := strings.TrimSpace(expectedLines[0])
	if anchor == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	start := -1
	for index, line := range lines {
		if strings.TrimSpace(line) == anchor {
			start = index
			break
		}
	}
	if start < 0 {
		return nil
	}
	matchedLines := 0
	for matchedLines < len(expectedLines) && start+matchedLines < len(lines) && lines[start+matchedLines] == expectedLines[matchedLines] {
		matchedLines++
	}
	lastIndex := len(expectedLines) - 1
	expectedLine := expectedLines[lastIndex]
	if matchedLines <= lastIndex {
		expectedLine = expectedLines[matchedLines]
	}
	actualLine := ""
	if start+matchedLines < len(lines) {
		actualLine = lines[start+matchedLines]
	}
	return &editMatchDiagnostic{
		nearestLine:    start + 1,
		matchedLines:   matchedLines,
		expected:       truncateEditDiagnostic(expectedLine),
		actual:         truncateEditDiagnostic(actualLine),
		whitespaceOnly: strings.TrimSpace(expectedLine) == strings.TrimSpace(actualLine),
	}
}

func truncateEditDiagnostic(value string) string {
	singleLine := strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(value)
	runes := []rune(singleLine)
	if len(runes) <= maxEditDiagnosticRunes {
		return singleLine
	}
	return string(runes[:maxEditDiagnosticRunes]) + "…"
}
