package native

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// midCut shortens text to at most budget bytes, marker included, keeping a head (70%) and a
// tail (30%) on rune boundaries and putting "…+N bytes…" where the middle was left out. Text
// that fits is returned as it is; when even one kept byte plus the marker does not fit, the
// bare marker is returned if it is shorter than the text.
func midCut(text string, budget int) string {
	if len(text) <= budget {
		return text
	}
	bare := fmt.Sprintf("…+%d bytes…", len(text))
	keep := budget - len(bare)
	if keep < 1 {
		if len(bare) < len(text) {
			return bare
		}
		return text
	}
	head := keep * 7 / 10
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	start := len(text) - (keep - keep*7/10)
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	cut := text[:head] + fmt.Sprintf("…+%d bytes…", start-head) + text[start:]
	if len(cut) >= len(text) {
		return text
	}
	return cut
}

// transcriptStepID names a step: the turn and the step inside it, in file order.
type transcriptStepID struct {
	turn, step int64
}

func (a transcriptStepID) less(b transcriptStepID) bool {
	return a.turn < b.turn || a.turn == b.turn && a.step < b.step
}

func (a transcriptStepID) String() string {
	return strconv.FormatInt(a.turn, 10) + "." + strconv.FormatInt(a.step, 10)
}

func parseTranscriptStepID(text string) (transcriptStepID, bool) {
	turn, step, ok := strings.Cut(text, ".")
	if !ok {
		return transcriptStepID{}, false
	}
	number := func(s string) (int64, bool) {
		if s == "" || strings.TrimLeft(s, "0123456789") != "" {
			return 0, false
		}
		value, err := strconv.ParseInt(s, 10, 64)
		return value, err == nil
	}
	t, validT := number(turn)
	s, validS := number(step)
	return transcriptStepID{t, s}, validT && validS && t > 0
}

// parseTranscriptSteps reads "3.2" or "3.1-3.4" (first and last step, inclusive).
func parseTranscriptSteps(text string) (from, to transcriptStepID, ok bool) {
	text = strings.TrimSpace(text)
	if low, high, isRange := strings.Cut(text, "-"); isRange {
		from, validFrom := parseTranscriptStepID(low)
		to, validTo := parseTranscriptStepID(high)
		return from, to, validFrom && validTo && !to.less(from)
	}
	from, ok = parseTranscriptStepID(text)
	return from, from, ok
}
