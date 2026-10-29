package extension

import (
	"strings"
	"unicode"
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
