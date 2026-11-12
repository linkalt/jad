package chatadapter

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// chatChunk is one Chat Completions stream chunk, or a whole non-streaming completion
// (its choices carry message instead of delta).
type chatChunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []chatChoice  `json:"choices"`
	Usage   *chatUsage    `json:"usage"`
	Error   *upstreamBody `json:"error"`
}

type chatChoice struct {
	Index        int                        `json:"index"`
	Delta        map[string]json.RawMessage `json:"delta"`
	Message      map[string]json.RawMessage `json:"message"`
	FinishReason *string                    `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type toolCallDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openItem is a message or reasoning item being streamed.
type openItem struct {
	outputIndex int
	itemID      string
	text        strings.Builder
}

// openCall is a function call being streamed.
type openCall struct {
	outputIndex int
	itemID      string
	callID      string
	name        string
	arguments   strings.Builder
}

// translator turns one Chat Completions stream into Responses events.
type translator struct {
	sink       *sink
	config     *Config
	model      string
	now        func() time.Time
	responseID string
	started    bool
	nextIndex  int
	text       *openItem
	reasoning  *openItem
	calls      map[int]*openCall
	// outputs are the finished items. They are closed in the order the stream ends
	// them, which is not the order they were opened in when text follows a call that is
	// still open, so complete() puts them in output_index order.
	outputs  []finishedItem
	finish   string
	finished bool
	usage    *chatUsage
	done     bool
}

func newTranslator(sink *sink, config *Config, model string, now func() time.Time) *translator {
	return &translator{sink: sink, config: config, model: model, now: now, calls: map[int]*openCall{}}
}

// run reads an SSE body to its end, or until a terminal event was written.
func (t *translator) run(body io.Reader) {
	err := readSSE(body, t.config.MaxEventBytes, func(data string) bool {
		if data == "[DONE]" {
			t.complete()
			return false
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.fail("upstream_invalid_event", "the upstream sent an event that is not valid JSON: "+err.Error())
			return false
		}
		t.onChunk(&chunk)
		return !t.done && t.sink.err == nil
	})
	if t.done || t.sink.err != nil {
		return
	}
	switch {
	case err != nil:
		t.fail("upstream_read_failed", "reading the upstream stream failed: "+err.Error())
	case t.finished:
		// Some servers close the stream after the finish reason without [DONE].
		t.complete()
	default:
		t.fail("upstream_stream_ended", "the upstream stream ended before a finish_reason")
	}
}

func (t *translator) onChunk(chunk *chatChunk) {
	if chunk.Error != nil {
		t.fail(errorCode(chunk.Error), chunk.Error.Message)
		return
	}
	if t.responseID == "" && chunk.ID != "" {
		t.responseID = "resp_" + strings.TrimPrefix(chunk.ID, "chatcmpl-")
	}
	if chunk.Model != "" {
		t.model = chunk.Model
	}
	if chunk.Usage != nil {
		t.usage = chunk.Usage
	}
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}
		delta := choice.Delta
		if delta == nil {
			delta = choice.Message
		}
		t.onDelta(delta)
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			t.finish, t.finished = *choice.FinishReason, true
		}
	}
}

func (t *translator) onDelta(delta map[string]json.RawMessage) {
	for _, field := range t.config.ReasoningFields {
		if text := stringValue(delta[field]); text != "" {
			t.reasoningDelta(text)
			break
		}
	}
	if text := stringValue(delta["content"]); text != "" {
		t.textDelta(text)
	}
	if text := stringValue(delta["refusal"]); text != "" {
		t.textDelta(text)
	}
	if raw, ok := delta["tool_calls"]; ok && !isNull(raw) {
		var calls []toolCallDelta
		if json.Unmarshal(raw, &calls) == nil {
			for position, call := range calls {
				index := position
				if call.Index != nil {
					index = *call.Index
				}
				t.callDelta(index, &call)
			}
		}
	}
}

func stringValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return text
}

func (t *translator) ensureStarted() {
	if t.started {
		return
	}
	t.started = true
	skeleton := t.response("in_progress")
	t.sink.event("response.created", map[string]any{"response": skeleton})
	t.sink.event("response.in_progress", map[string]any{"response": skeleton})
}

func (t *translator) id() string {
	if t.responseID == "" {
		return "resp_chat"
	}
	return t.responseID
}

func (t *translator) itemID(prefix string, index int) string {
	return prefix + "_" + strings.TrimPrefix(t.id(), "resp_") + "_" + strconv.Itoa(index)
}

func (t *translator) response(status string) map[string]any {
	return map[string]any{
		"id": t.id(), "object": "response", "created_at": t.now().Unix(), "status": status,
		"model": t.model, "output": []map[string]any{}, "incomplete_details": nil, "error": nil,
	}
}

// finishedItem is a closed output item and the output_index it was opened with.
type finishedItem struct {
	index int
	item  map[string]any
}

func (t *translator) allocate() int {
	index := t.nextIndex
	t.nextIndex++
	return index
}

func (t *translator) reasoningDelta(text string) {
	t.ensureStarted()
	t.closeText()
	if t.reasoning == nil {
		index := t.allocate()
		t.reasoning = &openItem{outputIndex: index, itemID: t.itemID("rs", index)}
		t.sink.event("response.output_item.added", map[string]any{
			"output_index": index, "item": map[string]any{"type": "reasoning", "id": t.reasoning.itemID, "summary": []any{}},
		})
	}
	t.reasoning.text.WriteString(text)
	t.sink.event("response.reasoning_text.delta", map[string]any{
		"item_id": t.reasoning.itemID, "output_index": t.reasoning.outputIndex, "content_index": 0, "delta": text,
	})
}

func (t *translator) closeReasoning() {
	open := t.reasoning
	if open == nil {
		return
	}
	t.reasoning = nil
	text := open.text.String()
	t.sink.event("response.reasoning_text.done", map[string]any{
		"item_id": open.itemID, "output_index": open.outputIndex, "content_index": 0, "text": text,
	})
	item := map[string]any{
		"type": "reasoning", "id": open.itemID, "summary": []any{},
		"content": []map[string]any{{"type": "reasoning_text", "text": text}},
	}
	t.sink.event("response.output_item.done", map[string]any{"output_index": open.outputIndex, "item": item})
	t.outputs = append(t.outputs, finishedItem{open.outputIndex, item})
}

func (t *translator) textDelta(text string) {
	t.ensureStarted()
	t.closeReasoning()
	if t.text == nil {
		index := t.allocate()
		t.text = &openItem{outputIndex: index, itemID: t.itemID("msg", index)}
		t.sink.event("response.output_item.added", map[string]any{
			"output_index": index,
			"item":         map[string]any{"type": "message", "id": t.text.itemID, "status": "in_progress", "role": "assistant", "content": []any{}},
		})
		t.sink.event("response.content_part.added", map[string]any{
			"item_id": t.text.itemID, "output_index": index, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	t.text.text.WriteString(text)
	t.sink.event("response.output_text.delta", map[string]any{
		"item_id": t.text.itemID, "output_index": t.text.outputIndex, "content_index": 0, "delta": text,
	})
}

func (t *translator) closeText() {
	open := t.text
	if open == nil {
		return
	}
	t.text = nil
	text := open.text.String()
	t.sink.event("response.output_text.done", map[string]any{
		"item_id": open.itemID, "output_index": open.outputIndex, "content_index": 0, "text": text,
	})
	t.sink.event("response.content_part.done", map[string]any{
		"item_id": open.itemID, "output_index": open.outputIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	item := map[string]any{
		"type": "message", "id": open.itemID, "status": "completed", "role": "assistant",
		"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	t.sink.event("response.output_item.done", map[string]any{"output_index": open.outputIndex, "item": item})
	t.outputs = append(t.outputs, finishedItem{open.outputIndex, item})
}

func (t *translator) callDelta(index int, delta *toolCallDelta) {
	t.ensureStarted()
	t.closeReasoning()
	t.closeText()
	open := t.calls[index]
	if open == nil {
		output := t.allocate()
		callID := delta.ID
		if callID == "" {
			callID = "call_" + strings.TrimPrefix(t.id(), "resp_") + "_" + strconv.Itoa(output)
		}
		open = &openCall{outputIndex: output, itemID: t.itemID("fc", output), callID: callID, name: delta.Function.Name}
		t.calls[index] = open
		t.sink.event("response.output_item.added", map[string]any{
			"output_index": output,
			"item": map[string]any{
				"type": "function_call", "id": open.itemID, "call_id": open.callID, "name": open.name, "arguments": "", "status": "in_progress",
			},
		})
	} else if open.name == "" {
		open.name = delta.Function.Name
	}
	if delta.Function.Arguments != "" {
		open.arguments.WriteString(delta.Function.Arguments)
		t.sink.event("response.function_call_arguments.delta", map[string]any{
			"item_id": open.itemID, "output_index": open.outputIndex, "delta": delta.Function.Arguments,
		})
	}
}

// closeCalls finishes the function calls in the order they were opened.
func (t *translator) closeCalls() {
	calls := make([]*openCall, 0, len(t.calls))
	for _, open := range t.calls {
		calls = append(calls, open)
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].outputIndex < calls[j].outputIndex })
	t.calls = map[int]*openCall{}
	for _, open := range calls {
		arguments := open.arguments.String()
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		t.sink.event("response.function_call_arguments.done", map[string]any{
			"item_id": open.itemID, "output_index": open.outputIndex, "arguments": arguments,
		})
		item := map[string]any{
			"type": "function_call", "id": open.itemID, "call_id": open.callID, "name": open.name, "arguments": arguments, "status": "completed",
		}
		t.sink.event("response.output_item.done", map[string]any{"output_index": open.outputIndex, "item": item})
		t.outputs = append(t.outputs, finishedItem{open.outputIndex, item})
	}
}

// complete closes the open items and writes the terminal event.
func (t *translator) complete() {
	if t.done {
		return
	}
	t.ensureStarted()
	t.closeReasoning()
	t.closeText()
	t.closeCalls()
	response := t.response("completed")
	sort.SliceStable(t.outputs, func(i, j int) bool { return t.outputs[i].index < t.outputs[j].index })
	output := make([]map[string]any, 0, len(t.outputs))
	for _, finished := range t.outputs {
		output = append(output, finished.item)
	}
	response["output"] = output
	kind := "response.completed"
	switch t.finish {
	case "length":
		kind, response["status"], response["incomplete_details"] = "response.incomplete", "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		kind, response["status"], response["incomplete_details"] = "response.incomplete", "incomplete", map[string]any{"reason": "content_filter"}
	}
	response["usage"] = translateUsage(t.usage)
	t.sink.event(kind, map[string]any{"response": response})
	t.sink.done()
	t.done = true
}

// fail writes response.failed. Open items are not closed: a partial item would look
// complete to the caller.
func (t *translator) fail(code, message string) {
	if t.done {
		return
	}
	t.ensureStarted()
	response := t.response("failed")
	response["error"] = map[string]any{"code": code, "message": message}
	t.sink.event("response.failed", map[string]any{"response": response})
	t.sink.done()
	t.done = true
}

func translateUsage(usage *chatUsage) map[string]any {
	if usage == nil {
		usage = &chatUsage{}
	}
	cached, reasoning := 0, 0
	if usage.PromptTokensDetails != nil {
		cached = usage.PromptTokensDetails.CachedTokens
	}
	if usage.CompletionTokensDetails != nil {
		reasoning = usage.CompletionTokensDetails.ReasoningTokens
	}
	total := usage.TotalTokens
	if total == 0 {
		total = usage.PromptTokens + usage.CompletionTokens
	}
	return map[string]any{
		"input_tokens": usage.PromptTokens, "input_tokens_details": map[string]int{"cached_tokens": cached},
		"output_tokens": usage.CompletionTokens, "output_tokens_details": map[string]int{"reasoning_tokens": reasoning},
		"total_tokens": total,
	}
}

func errorCode(body *upstreamBody) string {
	if code := stringValue(body.Code); code != "" {
		return code
	}
	if len(body.Code) > 0 && !isNull(body.Code) {
		return string(body.Code)
	}
	if body.Type != "" {
		return body.Type
	}
	return "api_error"
}

// readSSE calls handle with the data of each event until it returns false or the body
// ends. An event larger than limit is an error.
func readSSE(body io.Reader, limit int64, handle func(data string) bool) error {
	reader := bufio.NewReader(body)
	var data strings.Builder
	for {
		line, err := readLine(reader, limit-int64(data.Len()))
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		eof := errors.Is(err, io.EOF)
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if data.Len() > 0 {
				payload := data.String()
				data.Reset()
				if !handle(payload) {
					return nil
				}
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if eof {
			if data.Len() > 0 {
				handle(data.String())
			}
			return nil
		}
	}
}

// errEventTooLarge ends a stream whose event is larger than max_event_bytes.
var errEventTooLarge = errors.New("an upstream event is larger than max_event_bytes")

func readLine(reader *bufio.Reader, limit int64) (string, error) {
	var line strings.Builder
	for {
		chunk, err := reader.ReadSlice('\n')
		if int64(line.Len()+len(chunk)) > limit+2 {
			return "", errEventTooLarge
		}
		line.Write(chunk)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line.String(), err
	}
}

// sink writes Responses events as SSE.
type sink struct {
	w       http.ResponseWriter
	flusher http.Flusher
	begun   bool
	events  int
	err     error
}

func newSink(w http.ResponseWriter) *sink {
	flusher, _ := w.(http.Flusher)
	return &sink{w: w, flusher: flusher}
}

func (s *sink) event(kind string, fields map[string]any) {
	if s.err != nil {
		return
	}
	fields["type"] = kind
	fields["sequence_number"] = s.events
	encoded, err := json.Marshal(fields)
	if err != nil {
		s.err = err
		return
	}
	s.events++
	s.write(fmt.Sprintf("event: %s\ndata: %s\n\n", kind, encoded))
}

func (s *sink) done() { s.write("data: [DONE]\n\n") }

func (s *sink) write(text string) {
	if s.err != nil {
		return
	}
	if !s.begun {
		s.begun = true
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.WriteHeader(http.StatusOK)
	}
	if _, err := io.WriteString(s.w, text); err != nil {
		s.err = err
		return
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
}
