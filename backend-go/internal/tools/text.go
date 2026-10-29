package tools

// CutText cuts text to at most maxBytes of UTF-8 without leaving half a character.
// A negative limit cuts everything instead of panicking.
func CutText(text string, maxBytes int64) (string, bool) {
	maxBytes = max(maxBytes, 0)
	if int64(len(text)) <= maxBytes {
		return text, false
	}
	end := int(maxBytes)
	// A UTF-8 continuation byte starts with 0b10; stepping back to a lead byte drops a split character.
	for end > 0 && text[end]&0xc0 == 0x80 {
		end--
	}
	return text[:end], true
}
