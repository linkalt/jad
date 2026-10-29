package anthropicadapter

import (
	"encoding/json"
	"io"
	"strings"
	"time"
)

// streamOutcome says how one upstream attempt ended.
type streamOutcome struct {
	// Retry is set when an upstream error arrived before any event was sent and
	// the error type is on the retry list. Nothing has been written then.
	Retry bool
	// Finished is set when a terminal event (completed, incomplete or failed)
	// was written.
	Finished bool
	// ErrorType and ErrorMessage hold the error event the upstream sent, for the log.
	ErrorType, ErrorMessage string
}

// streamBlock is one open Anthropic content block.
type streamBlock struct {
	kind        string // text, tool_use, thinking, redacted_thinking or ignored
	outputIndex int
	itemID      string
	callID      string
	name        string
	text        strings.Builder
	arguments   strings.Builder
	thinking    strings.Builder
	signature   string
	redacted    string
}

// reasoningStream is the reasoning item being built from consecutive thinking
// blocks, which the Responses API shows as one item.
type reasoningStream struct {
	outputIndex int
	itemID      string
	text        strings.Builder
	blocks      []anthropicBlock
	// separate is set when a new block starts after text was already streamed.
	separate bool
}

// streamTranslator turns one Anthropic event stream into Responses events.
type streamTranslator struct {
	sink      *sseSink
	config    *Config
	model     string
	now       func() time.Time
	messageID string
	usage     anthropicUsage
	stop      string

	started   bool
	nextIndex int
	blocks    map[int]*streamBlock
	reasoning *reasoningStream
	outputs   []map[string]any
	done      bool
	// allowRetry lets an early retryable error end the attempt without output.
	allowRetry bool
}

func newStreamTranslator(sink *sseSink, config *Config, model string, now func() time.Time) *streamTranslator {
	return &streamTranslator{sink: sink, config: config, model: model, now: now, blocks: map[int]*streamBlock{}}
}

// anthropicStreamEvent is the union of the Anthropic stream events.
type anthropicStreamEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	Message      json.RawMessage `json:"message"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        json.RawMessage `json:"delta"`
	Usage        json.RawMessage `json:"usage"`
	Error        struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

// run reads the upstream stream until it ends or a terminal event is written.
func (t *streamTranslator) run(body io.Reader) streamOutcome {
	var outcome streamOutcome
	err := readSSE(body, t.config.Upstream.MaxEventBytes, func(raw sseEvent) bool {
		if t.sink.err != nil {
			return false
		}
		var event anthropicStreamEvent
		if err := json.Unmarshal([]byte(raw.Data), &event); err != nil {
			t.fail("upstream_invalid_event", "the upstream sent an event that is not valid JSON: "+err.Error())
			outcome.Finished = true
			return false
		}
		switch event.Type {
		case "ping":
			if t.started {
				t.sink.comment("ping")
			}
		case "message_start":
			t.onMessageStart(&event)
		case "content_block_start":
			t.onBlockStart(&event)
		case "content_block_delta":
			t.onBlockDelta(&event)
		case "content_block_stop":
			t.onBlockStop(&event)
		case "message_delta":
			t.onMessageDelta(&event)
		case "message_stop":
			t.complete()
		case "error":
			outcome.ErrorType, outcome.ErrorMessage = event.Error.Type, event.Error.Message
			if t.retryable(event.Error.Type) {
				outcome.Retry = true
				return false
			}
			t.fail(errorCode(event.Error.Type), withRequestID(event.Error.Message, event.RequestID))
		}
		outcome.Finished = t.done
		return !t.done
	})
	if t.sink.err != nil || t.done || outcome.Retry {
		return outcome
	}
	if err != nil {
		t.fail("upstream_read_failed", "reading the upstream stream failed: "+err.Error())
	} else {
		t.fail("upstream_stream_ended", "the upstream stream ended before message_stop")
	}
	outcome.Finished = true
	return outcome
}

// retryable reports whether an error event may be retried: nothing was sent yet
// and the type is on the configured list.
func (t *streamTranslator) retryable(kind string) bool {
	if !t.allowRetry || t.started || t.sink.events > 0 {
		return false
	}
	for _, allowed := range t.config.Upstream.StreamRetry.ErrorTypes {
		if allowed == kind {
			return true
		}
	}
	return false
}

func errorCode(kind string) string {
	if kind == "" {
		return "api_error"
	}
	return kind
}

func (t *streamTranslator) onMessageStart(event *anthropicStreamEvent) {
	var start struct {
		ID    string         `json:"id"`
		Model string         `json:"model"`
		Usage anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(event.Message, &start) != nil {
		return
	}
	t.messageID = start.ID
	t.usage = start.Usage
	if start.Model != "" {
		t.model = start.Model
	}
}

// ensureStarted sends response.created and response.in_progress once. It runs
// when the first block opens, so an error before that can still be retried.
func (t *streamTranslator) ensureStarted() {
	if t.started {
		return
	}
	t.started = true
	skeleton := t.skeleton("in_progress")
	t.sink.event("response.created", map[string]any{"response": skeleton})
	t.sink.event("response.in_progress", map[string]any{"response": skeleton})
}

func (t *streamTranslator) skeleton(status string) *responseObject {
	return &responseObject{
		ID: responseID(t.messageID), Object: "response", CreatedAt: t.now().Unix(),
		Status: status, Model: t.model, Output: []map[string]any{},
	}
}

func (t *streamTranslator) onBlockStart(event *anthropicStreamEvent) {
	var block anthropicBlock
	if json.Unmarshal(event.ContentBlock, &block) != nil {
		return
	}
	t.ensureStarted()
	open := &streamBlock{kind: block.Type}
	t.blocks[event.Index] = open
	switch block.Type {
	case "thinking", "redacted_thinking":
		open.redacted = block.Data
		open.signature = block.Signature
		if block.Thinking != nil {
			open.thinking.WriteString(*block.Thinking)
		}
		t.openThinking(open)
	case "text":
		t.closeReasoning()
		t.openText(open)
		if block.Text != "" {
			t.textDelta(open, block.Text)
		}
	case "tool_use":
		t.closeReasoning()
		t.openToolUse(open, &block)
	default:
		open.kind = "ignored"
	}
}

func (t *streamTranslator) onBlockDelta(event *anthropicStreamEvent) {
	open := t.blocks[event.Index]
	if open == nil {
		return
	}
	var delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
	}
	if json.Unmarshal(event.Delta, &delta) != nil {
		return
	}
	// A delta only applies to the block kind it belongs to; deltas of an
	// ignored block (for example a server tool) produce no output.
	switch {
	case delta.Type == "text_delta" && open.kind == "text":
		t.textDelta(open, delta.Text)
	case delta.Type == "input_json_delta" && open.kind == "tool_use":
		t.argumentsDelta(open, delta.PartialJSON)
	case delta.Type == "thinking_delta" && open.kind == "thinking":
		t.thinkingDelta(open, delta.Thinking)
	case delta.Type == "signature_delta" && open.kind == "thinking":
		open.signature += delta.Signature
	}
}

func (t *streamTranslator) onBlockStop(event *anthropicStreamEvent) {
	open := t.blocks[event.Index]
	if open == nil {
		return
	}
	delete(t.blocks, event.Index)
	switch open.kind {
	case "text":
		t.closeText(open)
	case "tool_use":
		t.closeToolUse(open)
	case "thinking", "redacted_thinking":
		t.finishThinkingBlock(open)
	}
}

func (t *streamTranslator) onMessageDelta(event *anthropicStreamEvent) {
	var delta struct {
		StopReason string `json:"stop_reason"`
	}
	if json.Unmarshal(event.Delta, &delta) == nil && delta.StopReason != "" {
		t.stop = delta.StopReason
	}
	// Usage in message_delta is cumulative; a field it omits keeps its value.
	var usage struct {
		InputTokens              *int                `json:"input_tokens"`
		OutputTokens             *int                `json:"output_tokens"`
		CacheCreationInputTokens *int                `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     *int                `json:"cache_read_input_tokens"`
		CacheCreation            *cacheCreationUsage `json:"cache_creation"`
	}
	if json.Unmarshal(event.Usage, &usage) != nil {
		return
	}
	set := func(target *int, value *int) {
		if value != nil {
			*target = *value
		}
	}
	set(&t.usage.InputTokens, usage.InputTokens)
	set(&t.usage.OutputTokens, usage.OutputTokens)
	set(&t.usage.CacheCreationInputTokens, usage.CacheCreationInputTokens)
	set(&t.usage.CacheReadInputTokens, usage.CacheReadInputTokens)
	if usage.CacheCreation != nil {
		t.usage.CacheCreation = usage.CacheCreation
	}
}

// complete writes the terminal event for a finished message.
func (t *streamTranslator) complete() {
	t.ensureStarted()
	t.closeReasoning()
	response := t.skeleton("completed")
	response.Output = t.outputs
	status, details := statusFor(t.stop)
	response.Status, response.IncompleteDetails = status, details
	usage := translateUsage(&t.usage)
	response.Usage = &usage
	kind := "response.completed"
	if status == "incomplete" {
		kind = "response.incomplete"
	}
	t.sink.event(kind, map[string]any{"response": response})
	t.sink.done()
	t.done = true
}

// fail writes response.failed. Open items are not closed: a partial item would
// look complete to the caller.
func (t *streamTranslator) fail(code, message string) {
	t.ensureStarted()
	response := t.skeleton("failed")
	response.Error = &responseError{Code: code, Message: message}
	t.sink.event("response.failed", map[string]any{"response": response})
	t.sink.done()
	t.done = true
}

func (t *streamTranslator) allocate() int {
	index := t.nextIndex
	t.nextIndex++
	return index
}

func (t *streamTranslator) openText(open *streamBlock) {
	open.outputIndex = t.allocate()
	open.itemID = itemID("msg", t.messageID, open.outputIndex)
	t.sink.event("response.output_item.added", map[string]any{
		"output_index": open.outputIndex,
		"item": map[string]any{
			"type": "message", "id": open.itemID, "status": "in_progress", "role": "assistant", "content": []any{},
		},
	})
	t.sink.event("response.content_part.added", map[string]any{
		"item_id": open.itemID, "output_index": open.outputIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (t *streamTranslator) textDelta(open *streamBlock, text string) {
	if text == "" {
		return
	}
	open.text.WriteString(text)
	t.sink.event("response.output_text.delta", map[string]any{
		"item_id": open.itemID, "output_index": open.outputIndex, "content_index": 0, "delta": text,
	})
}

func (t *streamTranslator) closeText(open *streamBlock) {
	text := open.text.String()
	t.sink.event("response.output_text.done", map[string]any{
		"item_id": open.itemID, "output_index": open.outputIndex, "content_index": 0, "text": text,
	})
	t.sink.event("response.content_part.done", map[string]any{
		"item_id": open.itemID, "output_index": open.outputIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	item := messageItem(open.outputIndex, t.messageID, text)
	t.sink.event("response.output_item.done", map[string]any{"output_index": open.outputIndex, "item": item})
	t.outputs = append(t.outputs, item)
}

func (t *streamTranslator) openToolUse(open *streamBlock, block *anthropicBlock) {
	open.outputIndex = t.allocate()
	open.callID, open.name = block.ID, block.Name
	open.itemID = "fc_" + strings.TrimPrefix(block.ID, "toolu_")
	t.sink.event("response.output_item.added", map[string]any{
		"output_index": open.outputIndex,
		"item": map[string]any{
			"type": "function_call", "id": open.itemID, "call_id": open.callID, "name": open.name,
			"arguments": "", "status": "in_progress",
		},
	})
}

func (t *streamTranslator) argumentsDelta(open *streamBlock, partial string) {
	if open.kind != "tool_use" || partial == "" {
		return
	}
	open.arguments.WriteString(partial)
	t.sink.event("response.function_call_arguments.delta", map[string]any{
		"item_id": open.itemID, "output_index": open.outputIndex, "delta": partial,
	})
}

func (t *streamTranslator) closeToolUse(open *streamBlock) {
	arguments := open.arguments.String()
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	t.sink.event("response.function_call_arguments.done", map[string]any{
		"item_id": open.itemID, "output_index": open.outputIndex, "arguments": arguments,
	})
	item := map[string]any{
		"type": "function_call", "id": open.itemID, "call_id": open.callID, "name": open.name,
		"arguments": arguments, "status": "completed",
	}
	t.sink.event("response.output_item.done", map[string]any{"output_index": open.outputIndex, "item": item})
	t.outputs = append(t.outputs, item)
}

// openThinking starts a thinking block, opening the shared reasoning item when
// this is the first block of a run of thinking blocks.
func (t *streamTranslator) openThinking(open *streamBlock) {
	if t.reasoning == nil {
		index := t.allocate()
		t.reasoning = &reasoningStream{outputIndex: index, itemID: itemID("rs", t.messageID, index)}
		t.sink.event("response.output_item.added", map[string]any{
			"output_index": index,
			"item":         map[string]any{"type": "reasoning", "id": t.reasoning.itemID, "summary": []any{}},
		})
	}
	t.reasoning.separate = t.reasoning.text.Len() > 0
	if text := open.thinking.String(); text != "" {
		open.thinking.Reset()
		t.thinkingDelta(open, text)
	}
}

func (t *streamTranslator) thinkingDelta(open *streamBlock, text string) {
	if text == "" || t.reasoning == nil {
		return
	}
	open.thinking.WriteString(text)
	if t.reasoning.separate {
		text = "\n\n" + text
		t.reasoning.separate = false
	}
	t.reasoning.text.WriteString(text)
	t.sink.event("response.reasoning_text.delta", map[string]any{
		"item_id": t.reasoning.itemID, "output_index": t.reasoning.outputIndex, "content_index": 0, "delta": text,
	})
}

// finishThinkingBlock keeps a closed thinking block for the reasoning item.
func (t *streamTranslator) finishThinkingBlock(open *streamBlock) {
	if t.reasoning == nil {
		return
	}
	block := anthropicBlock{Type: open.kind, Signature: open.signature, Data: open.redacted}
	if open.kind == "thinking" {
		text := open.thinking.String()
		block.Thinking = &text
	}
	t.reasoning.blocks = append(t.reasoning.blocks, block)
}

// closeReasoning finishes the open reasoning item, if any.
func (t *streamTranslator) closeReasoning() {
	reasoning := t.reasoning
	if reasoning == nil {
		return
	}
	t.reasoning = nil
	if reasoning.text.Len() > 0 {
		t.sink.event("response.reasoning_text.done", map[string]any{
			"item_id": reasoning.itemID, "output_index": reasoning.outputIndex, "content_index": 0,
			"text": reasoning.text.String(),
		})
	}
	item := reasoningItem(reasoning.outputIndex, t.messageID, reasoning.blocks)
	t.sink.event("response.output_item.done", map[string]any{"output_index": reasoning.outputIndex, "item": item})
	t.outputs = append(t.outputs, item)
}
