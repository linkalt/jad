package extension

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// isJSSpace is JavaScript whitespace: U+FEFF counts, U+0085 does not.
func isJSSpace(character rune) bool {
	switch character {
	case '\ufeff':
		return true
	case '\u0085':
		return false
	}
	return unicode.IsSpace(character)
}

// oneLine collapses every whitespace run to a single space and trims the ends.
func oneLine(value string) string {
	return strings.Join(strings.FieldsFunc(value, isJSSpace), " ")
}

// cutUTF8 cuts text to at most maxBytes bytes without leaving half a character.
func cutUTF8(text string, maxBytes int64) (string, bool) {
	if int64(len(text)) <= maxBytes {
		return text, false
	}
	end := int(maxBytes)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end], true
}
