package native

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// transcriptStep is one step of a turn: a user or assistant message, a tool call with its
// result, a reasoning item or a marker for something else. Steps are numbered turn.step in
// file order; the user message that opens a turn is step 0. The list needs only the line
// fields; the detail keeps the full text of the steps it was asked for.
type transcriptStep struct {
	id      transcriptStepID
	turnOff int64
	off     int64 // where the line that made the step starts
	ord     int64
	kind    string // user, assistant, message, call, result, reasoning, marker
	role    string // what the role filter matches
	tool    string
	callID  string
	matched bool

	line     string // list: the text after the kind (a message preview, a marker)
	argsLine string // list: the arguments of a call, values cut

	hasResult   bool
	status      string
	resultBytes int

	full      bool
	text      string
	args      string
	result    string
	reasoning string
}

type transcriptWalk struct {
	turns   []transcriptTurn
	needle  string
	preview int
	value   int
	// from..to are the steps whose full text is kept; both nil in list mode.
	keepFrom, keepTo *transcriptStepID

	steps     []*transcriptStep
	calls     map[string]*transcriptStep
	turnIndex int
	counter   int64
	ord       int64
	started   bool
	lineOff   int64
}

func newTranscriptWalk(turns []transcriptTurn, first int, needle string, preview, value int) *transcriptWalk {
	return &transcriptWalk{turns: turns, turnIndex: first, needle: needle, preview: preview, value: value, calls: map[string]*transcriptStep{}}
}

func (w *transcriptWalk) keeps(id transcriptStepID) bool {
	return w.keepFrom != nil && !id.less(*w.keepFrom) && !w.keepTo.less(id)
}

// advance moves to the turn the line at off belongs to and restarts the step counting there.
func (w *transcriptWalk) advance(off int64) {
	moved := !w.started
	for w.turnIndex+1 < len(w.turns) && w.turns[w.turnIndex+1].off <= off {
		w.turnIndex++
		moved = true
	}
	if moved {
		w.counter, w.ord, w.started = 0, 0, true
	}
}

func (w *transcriptWalk) add(step *transcriptStep, opens bool) *transcriptStep {
	turn := w.turns[w.turnIndex]
	if !opens {
		w.counter++
	}
	step.id = transcriptStepID{turn.number, w.counter}
	step.turnOff = turn.off
	step.off = w.lineOff
	step.ord = w.ord
	w.ord++
	step.full = w.keeps(step.id)
	w.steps = append(w.steps, step)
	return step
}

func (w *transcriptWalk) matches(text string) bool {
	return w.needle != "" && strings.Contains(strings.ToLower(text), w.needle)
}

func collapseSpaces(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func replaceDataURLs(text string) string {
	if !strings.Contains(text, "data:") {
		return text
	}
	return transcriptDataURL.ReplaceAllStringFunc(text, func(match string) string {
		return fmt.Sprintf("[data URL %d bytes]", len(match))
	})
}

func (w *transcriptWalk) message(role, text string, opens bool) {
	text = replaceDataURLs(text)
	kind := role
	if role != "user" && role != "assistant" {
		kind = "message"
	}
	step := &transcriptStep{kind: kind, role: role, matched: w.matches(text), line: midCut(collapseSpaces(text), w.preview)}
	if kind == "message" {
		step.line = role + ": " + step.line
	}
	step = w.add(step, opens)
	if step.full {
		step.text = text
	}
}

func (w *transcriptWalk) marker(label string) {
	w.add(&transcriptStep{kind: "marker", role: "marker", line: label}, false)
}

func (w *transcriptWalk) call(item transcriptRecord) {
	args := replaceDataURLs(transcriptText(item.Arguments))
	step := &transcriptStep{kind: "call", role: "tool", tool: item.Name, callID: item.CallID, status: "no-result",
		matched: w.matches(item.Name + " " + args), argsLine: compactArguments(args, w.value)}
	step = w.add(step, false)
	if step.full {
		step.args = args
	}
	if item.CallID != "" {
		w.calls[item.CallID] = step
	}
}

func (w *transcriptWalk) reasoning(raw json.RawMessage) {
	step := w.add(&transcriptStep{kind: "reasoning", role: "reasoning"}, false)
	if !step.full {
		return
	}
	var item struct {
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return
	}
	var pieces []string
	for _, part := range item.Summary {
		pieces = append(pieces, part.Text)
	}
	for _, part := range item.Content {
		pieces = append(pieces, part.Text)
	}
	step.reasoning = strings.Join(pieces, "\n")
}

func (w *transcriptWalk) result(record transcriptRecord) {
	text := replaceDataURLs(transcriptText(record.Output))
	call := w.calls[record.CallID]
	if call == nil {
		step := w.add(&transcriptStep{kind: "result", role: "tool", callID: record.CallID, hasResult: true, status: transcriptResultStatus(text), resultBytes: len(text), matched: w.matches(text)}, false)
		if step.full {
			step.result = text
		}
		return
	}
	call.hasResult, call.status, call.resultBytes = true, transcriptResultStatus(text), len(text)
	call.matched = call.matched || w.matches(text)
	if call.full {
		call.result = text
	}
}

// transcriptResultStatus reads the status of a tool result envelope, whose first line is a JSON object
// with a status; ok stays ok, any other status is an error, an unreadable one is unknown.
func transcriptResultStatus(text string) string {
	first, _, _ := strings.Cut(text, "\n")
	var envelope struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(first), &envelope) != nil || envelope.Status == "" {
		return "unknown"
	}
	if envelope.Status == "ok" {
		return "ok"
	}
	return "error"
}

// visit takes one transcript line in file order.
func (w *transcriptWalk) visit(line transcriptLine) {
	w.lineOff = line.off
	w.advance(line.off)
	if line.tooLong {
		w.marker(fmt.Sprintf("[line of %d bytes skipped: over tools.native.transcript.line_max_bytes]", line.length))
		return
	}
	var record transcriptRecord
	if err := json.Unmarshal(line.data, &record); err != nil {
		w.marker("[unreadable record]")
		return
	}
	switch {
	case record.Type == "function_call_output":
		w.result(record)
	case record.Type == "response.completed" && record.Response != nil:
		for _, raw := range record.Response.Output {
			var item transcriptRecord
			if json.Unmarshal(raw, &item) != nil {
				continue
			}
			switch item.Type {
			case "message":
				w.message("assistant", transcriptText(item.Content), false)
			case "function_call":
				w.call(item)
			case "reasoning":
				w.reasoning(raw)
			default:
				w.marker("[" + item.Type + "]")
			}
		}
	case record.Type == "message" || record.Role != "":
		role := record.Role
		if role == "" {
			role = "message"
		}
		w.message(role, transcriptText(record.Content), role == "user" && line.off == w.turns[w.turnIndex].off)
	default:
		w.marker("[" + record.Type + "]")
	}
}

// compactArguments writes the arguments of a call on one line: the keys whole, each value
// cut in the middle to limit bytes. Arguments that are not an object are one cut value.
func compactArguments(arguments string, limit int) string {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return "{}"
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return midCut(collapseSpaces(arguments), limit)
	}
	var out strings.Builder
	out.WriteByte('{')
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, isKey := keyToken.(string)
		var value json.RawMessage
		if err != nil || !isKey || decoder.Decode(&value) != nil {
			return midCut(collapseSpaces(arguments), limit)
		}
		if out.Len() > 1 {
			out.WriteByte(',')
		}
		out.WriteString(transcriptJSONString(key))
		out.WriteByte(':')
		var text string
		if len(value) > 0 && value[0] == '"' && json.Unmarshal(value, &text) == nil {
			out.WriteString(transcriptJSONString(midCut(text, limit)))
			continue
		}
		var compact bytes.Buffer
		if json.Compact(&compact, value) != nil {
			compact.Write(value)
		}
		out.WriteString(midCut(compact.String(), limit))
	}
	out.WriteByte('}')
	return out.String()
}

func transcriptJSONString(text string) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(text)
	return strings.TrimRight(buffer.String(), "\n")
}

// stepLine is the list line of a step.
func stepLine(step *transcriptStep) string {
	id := step.id.String() + " "
	switch step.kind {
	case "user", "assistant":
		return id + step.kind + ": " + step.line
	case "call":
		status := step.status
		if step.hasResult {
			status += fmt.Sprintf(" %dB", step.resultBytes)
		}
		return id + "call " + step.tool + " " + status + " " + step.argsLine
	case "result":
		return fmt.Sprintf("%sresult [%s] %s %dB without its call", id, step.callID, step.status, step.resultBytes)
	}
	return id + step.line
}

// selectTranscriptSteps keeps the steps that pass the role and tool filters and, with a
// search, those that match it plus context steps on each side. Reasoning is never listed.
func selectTranscriptSteps(steps []*transcriptStep, role string, tool *compiledGlob, search bool, context int) []*transcriptStep {
	listed := steps[:0:0]
	for _, step := range steps {
		if step.kind != "reasoning" {
			listed = append(listed, step)
		}
	}
	keep := make([]bool, len(listed))
	for index, step := range listed {
		keep[index] = (role == "" || step.role == role) && (tool == nil || step.role == "tool" && step.tool != "" && tool.matches(step.tool, step.tool))
	}
	if search {
		hit := make([]bool, len(listed))
		for index, step := range listed {
			hit[index] = keep[index] && step.matched
		}
		for index := range keep {
			keep[index] = false
		}
		for index, matched := range hit {
			if !matched {
				continue
			}
			for near := max(index-context, 0); near <= min(index+context, len(listed)-1); near++ {
				keep[near] = true
			}
		}
	}
	selected := listed[:0:0]
	for index, step := range listed {
		if keep[index] {
			selected = append(selected, step)
		}
	}
	return selected
}
