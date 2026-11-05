package native

import (
	"unicode/utf8"

	"jad/backend-go/internal/tools"
)

func safeInteger(value any) (int64, bool) {
	return tools.SafeInteger(value)
}

// tailUTF8 keeps the last limit bytes of text, starting on a character boundary.
func tailUTF8(text string, limit int) (string, bool) {
	if limit < 0 {
		limit = 0
	}
	if len(text) <= limit {
		return text, false
	}
	start := len(text) - limit
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:], true
}
