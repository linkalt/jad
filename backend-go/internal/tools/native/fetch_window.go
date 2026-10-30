package native

import (
	"strings"
	"unicode/utf8"
)

// fetchWindow is sliceFetchCharacters without converting the whole value. It returns the same
// text and consumed units for the same offset and limit, and the value's length in UTF-16
// units, so a page costs its window and not the whole document.
func fetchWindow(value string, offset, limit int64) (string, int64, int64) {
	total := fetchUnits(value)
	start := min(offset, total)
	end := start + limit
	if end < start || end > total {
		end = total
	}
	var builder strings.Builder
	var unit int64
	for _, r := range value {
		width := fetchRuneUnits(r)
		// A surrogate pair split by the end of the window is kept whole, as in sliceFetchCharacters.
		if end > start && width == 2 && unit+1 == end && end < total {
			end++
		}
		switch {
		case unit >= start && unit+width <= end:
			builder.WriteRune(r)
		case end > start && width == 2 && unit < end && unit+width > start && (unit < start || unit+width > end):
			// A window that starts or ends inside a pair decodes its lone half to U+FFFD, as
			// utf16.Decode does on the units of sliceFetchCharacters.
			builder.WriteRune(utf8.RuneError)
		}
		unit += width
	}
	return builder.String(), end - start, total
}

// fetchUnits is the length of value in UTF-16 units.
func fetchUnits(value string) int64 {
	var units int64
	for _, r := range value {
		units += fetchRuneUnits(r)
	}
	return units
}

// fetchRuneUnits is how many UTF-16 units encode r: a supplementary rune takes a pair.
func fetchRuneUnits(r rune) int64 {
	if r >= 0x10000 {
		return 2
	}
	return 1
}
