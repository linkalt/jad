package native

import (
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
	input := make([]byte, 0, len(tracker.pending)+len(chunk))
	input = append(input, tracker.pending...)
	input = append(input, chunk...)
	tracker.pending = nil
	for len(input) > 0 {
		if !utf8.FullRune(input) {
			tracker.pending = append(tracker.pending[:0], input...)
			return
		}
		r, size := utf8.DecodeRune(input)
		if r == utf8.RuneError && size == 1 {
			tracker.invalid = true
		}
		input = input[size:]
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
	for _, value := range chunk {
		if value == 0 {
			collector.binary = true
			break
		}
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
	return collector.renderWithProducedBytes(maxLineChars, stripANSI, collector.producedBytes)
}

func (collector *bashStreamCollector) renderWithProducedBytes(maxLineChars int, stripANSI bool, producedBytes int64) bashStreamOutput {
	headText := sanitizeBashOutput(string(collector.head), stripANSI)
	tailText := sanitizeBashOutput(string(collector.tail), stripANSI)
	shown := int64(len(headText) + len(tailText))
	if producedBytes < shown {
		producedBytes = shown
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
	return collector.renderWithProducedBytes(collector.stdout.producedBytes, collector.stderr.producedBytes)
}

func (collector *bashOutputCollector) renderWithProducedBytes(stdoutProducedBytes, stderrProducedBytes int64) bashRenderedOutput {
	stdout := collector.stdout.renderWithProducedBytes(collector.maxLineChars, collector.stripANSI, stdoutProducedBytes)
	stderr := collector.stderr.renderWithProducedBytes(collector.maxLineChars, collector.stripANSI, stderrProducedBytes)
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
		binary:        collector.stdout.binary || collector.stderr.binary,
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
