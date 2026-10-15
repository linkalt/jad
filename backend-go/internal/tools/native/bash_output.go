package native

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

type bashStream string

const (
	bashStdout bashStream = "stdout"
	bashStderr bashStream = "stderr"
)

type bashStreamLimits struct {
	headBytes int
	tailBytes int
}

type bashStreamOutput struct {
	producedBytes int64
	shownBytes    int64
	omittedBytes  int64
	truncated     bool
	text          string
}

type bashRenderedOutput struct {
	text          string
	stdout        bashStreamOutput
	stderr        bashStreamOutput
	truncated     bool
	omittedBytes  int64
	binary        bool
	returnedBytes int64
}

type bashUTF8Tracker struct {
	pending []byte
	invalid bool
}

func (tracker *bashUTF8Tracker) push(chunk []byte) {
	if len(tracker.pending) > 0 {
		// Finish the sequence left over from the last chunk with at most
		// utf8.UTFMax-1 bytes of this one; the rest is scanned in place.
		take := utf8.UTFMax - len(tracker.pending)
		if take > len(chunk) {
			take = len(chunk)
		}
		joined := append(tracker.pending, chunk[:take]...)
		if !utf8.FullRune(joined) {
			tracker.pending = joined
			return
		}
		r, size := utf8.DecodeRune(joined)
		if r == utf8.RuneError && size == 1 {
			tracker.invalid = true
		}
		tracker.pending = tracker.pending[:0]
		if consumed := size - (len(joined) - take); consumed >= 0 {
			chunk = chunk[consumed:]
		} else {
			// The decode stopped inside the old pending bytes (an invalid lead byte):
			// scan the bytes after it again together with the part taken from chunk.
			tracker.push(append([]byte(nil), joined[size:]...))
			chunk = chunk[take:]
		}
	}
	for len(chunk) > 0 {
		if chunk[0] < utf8.RuneSelf {
			chunk = chunk[1:]
			continue
		}
		if !utf8.FullRune(chunk) {
			tracker.pending = append(tracker.pending[:0], chunk...)
			return
		}
		r, size := utf8.DecodeRune(chunk)
		if r == utf8.RuneError && size == 1 {
			tracker.invalid = true
		}
		chunk = chunk[size:]
	}
}

func (tracker *bashUTF8Tracker) end() {
	if len(tracker.pending) > 0 {
		tracker.invalid = true
		tracker.pending = nil
	}
}

type bashStreamCollector struct {
	limits        bashStreamLimits
	head          []byte
	tail          []byte
	producedBytes int64
	binary        bool
	utf8Tracker   bashUTF8Tracker
}

func newBashStreamCollector(limits bashStreamLimits) *bashStreamCollector {
	return &bashStreamCollector{limits: limits}
}

func (collector *bashStreamCollector) push(chunk []byte) {
	collector.producedBytes += int64(len(chunk))
	if !collector.binary && bytes.IndexByte(chunk, 0) >= 0 {
		collector.binary = true
	}
	collector.utf8Tracker.push(chunk)

	remaining := chunk
	if len(collector.head) < collector.limits.headBytes {
		count := collector.limits.headBytes - len(collector.head)
		if count > len(remaining) {
			count = len(remaining)
		}
		collector.head = append(collector.head, remaining[:count]...)
		remaining = remaining[count:]
	}
	if collector.limits.tailBytes <= 0 || len(remaining) == 0 {
		return
	}
	if len(remaining) >= collector.limits.tailBytes {
		collector.tail = append(collector.tail[:0], remaining[len(remaining)-collector.limits.tailBytes:]...)
		return
	}
	collector.tail = append(collector.tail, remaining...)
	if overflow := len(collector.tail) - collector.limits.tailBytes; overflow > 0 {
		collector.tail = append(collector.tail[:0], collector.tail[overflow:]...)
	}
}

func (collector *bashStreamCollector) end() {
	collector.utf8Tracker.end()
	if collector.utf8Tracker.invalid {
		collector.binary = true
	}
}

func (collector *bashStreamCollector) render(maxLineChars int, stripANSI bool) bashStreamOutput {
	return collector.renderWithProducedBytes(maxLineChars, stripANSI, collector.producedBytes, false)
}

// lineAwareWindows drops the partial line at the end of the head and at the start of the
// tail, so a cut stream shows whole lines only. A window with no line break (one very
// long line) is left as it is. The dropped bytes are counted by the caller as omitted.
func lineAwareWindows(headText, tailText string) (string, string) {
	if end := strings.LastIndexByte(headText, '\n') + 1; end > 0 && end < len(headText) {
		headText = headText[:end]
	}
	if start := strings.IndexByte(tailText, '\n') + 1; start > 0 && start < len(tailText) {
		tailText = tailText[start:]
	}
	return headText, tailText
}

// frozenStream is a copy of what a stream collector holds, so it can be rendered
// after the lock that guards the collector is released.
type frozenStream struct {
	head, tail    string
	producedBytes int64
	binary        bool
}

func (collector *bashStreamCollector) freeze() frozenStream {
	return frozenStream{
		head:          string(collector.head),
		tail:          string(collector.tail),
		producedBytes: collector.producedBytes,
		binary:        collector.binary,
	}
}

func (collector *bashStreamCollector) renderWithProducedBytes(maxLineChars int, stripANSI bool, producedBytes int64, lineAware bool) bashStreamOutput {
	return collector.freeze().renderWithProducedBytes(maxLineChars, stripANSI, producedBytes, lineAware)
}

func (frozen frozenStream) renderWithProducedBytes(maxLineChars int, stripANSI bool, producedBytes int64, lineAware bool) bashStreamOutput {
	headText := sanitizeBashOutput(frozen.head, stripANSI)
	tailText := sanitizeBashOutput(frozen.tail, stripANSI)
	shown := int64(len(headText) + len(tailText))
	if producedBytes < shown {
		producedBytes = shown
	}
	if lineAware && producedBytes > shown {
		headText, tailText = lineAwareWindows(headText, tailText)
		shown = int64(len(headText) + len(tailText))
	}
	omitted := producedBytes - shown
	body := headText + tailText
	if omitted > 0 {
		body = headText
		if headText != "" && !strings.HasSuffix(headText, "\n") {
			body += "\n"
		}
		body += "[... " + itoaBash(omitted) + " bytes omitted ...]"
		if tailText != "" && !strings.HasPrefix(tailText, "\n") {
			body += "\n"
		}
		body += tailText
	}
	body = cutBashLongLines(body, maxLineChars)
	return bashStreamOutput{
		producedBytes: producedBytes,
		shownBytes:    shown,
		omittedBytes:  omitted,
		truncated:     omitted > 0,
		text:          body,
	}
}

type bashOutputCollector struct {
	stdout       *bashStreamCollector
	stderr       *bashStreamCollector
	maxLineChars int
	stripANSI    bool
	// lineAware ends a cut stream's head and starts its tail on line boundaries.
	lineAware bool
}

func newBashOutputCollector(stdout, stderr bashStreamLimits, maxLineChars int, stripANSI bool) *bashOutputCollector {
	return &bashOutputCollector{
		stdout:       newBashStreamCollector(stdout),
		stderr:       newBashStreamCollector(stderr),
		maxLineChars: maxLineChars,
		stripANSI:    stripANSI,
	}
}

func (collector *bashOutputCollector) push(stream bashStream, chunk []byte) {
	if stream == bashStdout {
		collector.stdout.push(chunk)
		return
	}
	collector.stderr.push(chunk)
}

func (collector *bashOutputCollector) end() {
	collector.stdout.end()
	collector.stderr.end()
}

func (collector *bashOutputCollector) render() bashRenderedOutput {
	return collector.freeze().render()
}

func (collector *bashOutputCollector) renderWithProducedBytes(stdoutProducedBytes, stderrProducedBytes int64) bashRenderedOutput {
	return collector.freeze().renderWithProducedBytes(stdoutProducedBytes, stderrProducedBytes)
}

// frozenOutput is a copy of both streams and the render settings. Taking it is cheap
// (the windows are a few KB); rendering it is not, and needs no lock.
type frozenOutput struct {
	stdout, stderr frozenStream
	maxLineChars   int
	stripANSI      bool
	lineAware      bool
}

func (collector *bashOutputCollector) freeze() frozenOutput {
	return frozenOutput{
		stdout:       collector.stdout.freeze(),
		stderr:       collector.stderr.freeze(),
		maxLineChars: collector.maxLineChars,
		stripANSI:    collector.stripANSI,
		lineAware:    collector.lineAware,
	}
}

func (frozen frozenOutput) render() bashRenderedOutput {
	return frozen.renderWithProducedBytes(frozen.stdout.producedBytes, frozen.stderr.producedBytes)
}

func (frozen frozenOutput) renderWithProducedBytes(stdoutProducedBytes, stderrProducedBytes int64) bashRenderedOutput {
	stdout := frozen.stdout.renderWithProducedBytes(frozen.maxLineChars, frozen.stripANSI, stdoutProducedBytes, frozen.lineAware)
	stderr := frozen.stderr.renderWithProducedBytes(frozen.maxLineChars, frozen.stripANSI, stderrProducedBytes, frozen.lineAware)
	labeled := stdout.text != "" && stderr.text != ""
	sections := make([]string, 0, 2)
	if stdout.text != "" {
		if labeled {
			sections = append(sections, "[stdout]\n"+stdout.text)
		} else {
			sections = append(sections, stdout.text)
		}
	}
	if stderr.text != "" {
		if labeled {
			sections = append(sections, "[stderr]\n"+stderr.text)
		} else {
			sections = append(sections, stderr.text)
		}
	}
	text := ""
	for _, section := range sections {
		if text == "" {
			text = section
		} else {
			if !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			text += section
		}
	}
	return bashRenderedOutput{
		text:          text,
		stdout:        stdout,
		stderr:        stderr,
		truncated:     stdout.truncated || stderr.truncated,
		omittedBytes:  stdout.omittedBytes + stderr.omittedBytes,
		binary:        frozen.stdout.binary || frozen.stderr.binary,
		returnedBytes: int64(len(text)),
	}
}

func sanitizeBashOutput(value string, stripANSI bool) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if !stripANSI {
		return strings.ReplaceAll(value, "\x00", "")
	}
	var output strings.Builder
	for index := 0; index < len(value); {
		character := value[index]
		if character == 0x1b {
			index += bashEscapeSequenceLength(value, index)
			continue
		}
		switch {
		case character == '\n' || character == '\t':
			output.WriteByte(character)
			index++
		case character == '\r':
			if index+1 >= len(value) || value[index+1] != '\n' {
				output.WriteByte('\n')
			}
			index++
		case character < 0x20 || character == 0x7f || (character >= 0x80 && character <= 0x9f):
			index++
		default:
			r, size := utf8.DecodeRuneInString(value[index:])
			if r == utf8.RuneError && size == 1 {
				output.WriteRune(utf8.RuneError)
				index++
			} else {
				output.WriteString(value[index : index+size])
				index += size
			}
		}
	}
	return output.String()
}

func bashEscapeSequenceLength(value string, start int) int {
	if start+1 >= len(value) {
		return 1
	}
	kind := value[start+1]
	if kind == '[' || kind == ']' {
		limit := start + 2 + 256
		if limit > len(value) {
			limit = len(value)
		}
		for index := start + 2; index < limit; index++ {
			character := value[index]
			if kind == '[' && character >= 0x40 && character <= 0x7e {
				return index - start + 1
			}
			if kind == ']' {
				if character == 0x07 {
					return index - start + 1
				}
				if character == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
					return index - start + 2
				}
			}
		}
		return 1
	}
	if kind == '(' || kind == ')' || kind == '#' {
		return 3
	}
	return 2
}

func cutBashLongLines(value string, maximum int) string {
	if maximum <= 0 {
		return value
	}
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		runes := []rune(line)
		if len(runes) <= maximum {
			continue
		}
		prefix := string(runes[:maximum]) + " [... truncated line]"
		lines[index] = trimBashUTF8Bytes(prefix, len(line))
	}
	return strings.Join(lines, "\n")
}

func trimBashUTF8Bytes(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}

func itoaBash(value int64) string {
	if value == 0 {
		return "0"
	}
	var buffer [24]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}
