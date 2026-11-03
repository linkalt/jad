package webui

import (
	"encoding/json"
	"fmt"
	"strings"
)

const mcpToolErrorText = "MCP server reported a tool error."

// StreamingRun reduces provider events into the frontend's cumulative assistant
// message. One instance spans all Responses rounds in a user-visible run, so
// tool calls, their results, and later assistant output share one message.
type StreamingRun struct {
	id                    string
	timestamp             string
	parts                 []Part
	steps                 []Step
	currentStepIsFallback bool
	callIDsByItemID       map[string]string
	callIDsByOutputIndex  map[int]string
	hostedRawLimit        int
	// delta accumulates the text of parts[deltaPart-1] so a stream of small
	// deltas appends in amortized constant time instead of copying the whole
	// part each time. deltaPart 0 means no part is tracked.
	delta     strings.Builder
	deltaPart int
	// summary keeps FieldsFromParts(parts) current, so a snapshot per delta does not
	// join every text part again (which made a long multi-step answer quadratic).
	summary summaryCache
}

// summaryCache caches the message-level text and reasoning of a run's parts. valid is
// false after any change to a text or reasoning part other than a delta appended to the
// last part, and the next snapshot rebuilds it. A kind with a single part uses that
// part's text directly; the builders hold the join only when a kind has several parts.
type summaryCache struct {
	valid                     bool
	textParts, reasoningParts int
	lastText, lastReasoning   int
	text, reasoning           strings.Builder
}

// invalidate marks the summary stale after a text or reasoning part changed.
func (s *summaryCache) invalidate() { s.valid = false }

// appended records that delta was appended to the last part, of kind kind.
func (s *summaryCache) appended(kind, delta string) {
	if !s.valid {
		return
	}
	switch {
	case kind == "text" && s.textParts > 1:
		s.text.WriteString(delta)
	case kind == "reasoning" && s.reasoningParts > 1:
		s.reasoning.WriteString(delta)
	}
}

// fields returns what FieldsFromParts(parts) returns, rebuilding the cache if stale.
func (s *summaryCache) fields(parts []Part) (text, reasoning string) {
	if !s.valid {
		s.rebuild(parts)
	}
	switch s.textParts {
	case 0:
	case 1:
		text = parts[s.lastText].Text
	default:
		text = s.text.String()
	}
	switch s.reasoningParts {
	case 0:
	case 1:
		reasoning = strings.TrimSpace(parts[s.lastReasoning].Text)
	default:
		reasoning = strings.TrimSpace(s.reasoning.String())
	}
	return text, reasoning
}

func (s *summaryCache) rebuild(parts []Part) {
	s.textParts, s.reasoningParts = 0, 0
	for index, part := range parts {
		switch part.Type {
		case "text":
			s.textParts++
			s.lastText = index
		case "reasoning":
			s.reasoningParts++
			s.lastReasoning = index
		}
	}
	s.text.Reset()
	s.reasoning.Reset()
	if s.textParts > 1 || s.reasoningParts > 1 {
		for _, part := range parts {
			switch {
			case part.Type == "text" && s.textParts > 1:
				s.text.WriteString(part.Text)
			case part.Type == "reasoning" && s.reasoningParts > 1:
				if s.reasoning.Len() > 0 {
					s.reasoning.WriteString("\n\n")
				}
				s.reasoning.WriteString(part.Text)
			}
		}
	}
	s.valid = true
}

// appendDelta adds streamed text to the last part of the given kind, or starts
// a new part. parts[i].Text always holds the full text so far.
func (r *StreamingRun) appendDelta(kind, text string) {
	if text == "" {
		return
	}
	last := len(r.parts) - 1
	if last >= 0 && r.parts[last].Type == kind && r.deltaPart == last+1 && r.delta.Len() == len(r.parts[last].Text) {
		r.delta.WriteString(text)
		r.parts[last].Text = r.delta.String()
		r.summary.appended(kind, text)
		return
	}
	r.summary.invalidate()
	r.parts = appendTextPart(r.parts, kind, text)
	last = len(r.parts) - 1
	r.delta.Reset()
	r.delta.WriteString(r.parts[last].Text)
	r.deltaPart = last + 1
}

// SetHostedRawLimit caps the raw JSON of provider-hosted tool parts in live
// messages, in bytes (0 keeps it whole). A hosted fetch can carry a whole page.
func (r *StreamingRun) SetHostedRawLimit(limit int) {
	if r != nil && limit >= 0 {
		r.hostedRawLimit = limit
	}
}

func NewStreamingRun(id, timestamp string) *StreamingRun {
	return &StreamingRun{
		id:                   id,
		timestamp:            timestamp,
		parts:                make([]Part, 0),
		steps:                make([]Step, 0),
		callIDsByItemID:      make(map[string]string),
		callIDsByOutputIndex: make(map[int]string),
	}
}

// Update consumes one raw Responses stream event and returns a snapshot when
// that event changes visible message state.
func (r *StreamingRun) Update(raw json.RawMessage) (Message, bool) {
	if r == nil {
		return Message{}, false
	}
	fields, err := rawObject(raw)
	if err != nil {
		return Message{}, false
	}
	typeName := stringField(fields["type"])
	switch typeName {
	case "response.created":
		response, _ := rawObject(fields["response"])
		r.ensureStep(stringField(response["id"]))
		return Message{}, false
	case "response.output_text.delta":
		if delta := stringField(fields["delta"]); delta != "" {
			r.ensureStep("")
			r.appendDelta("text", delta)
			r.updateCurrentStep(false)
			return r.Message("streaming"), true
		}
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if delta := stringField(fields["delta"]); strings.TrimSpace(delta) != "" {
			r.ensureStep("")
			r.appendDelta("reasoning", delta)
			r.updateCurrentStep(false)
			return r.Message("streaming"), true
		}
	case "response.output_item.added":
		item, _ := rawObject(fields["item"])
		if len(item) > 0 {
			if isHostedToolItem(item) {
				return r.upsertHostedTool(item)
			}
			return r.addToolCall(fields, item, true)
		}
	case "response.function_call_arguments.delta":
		callID := r.toolCallIDForEvent(fields)
		delta := stringField(fields["delta"])
		if callID != "" && delta != "" && r.updateToolCall(callID, func(current string) string { return current + delta }, true, "") {
			return r.Message("streaming"), true
		}
	case "response.function_call_arguments.done":
		callID := r.toolCallIDForEvent(fields)
		arguments := stringField(fields["arguments"])
		if callID != "" && r.updateToolCall(callID, func(string) string { return arguments }, false, "") {
			return r.Message("streaming"), true
		}
	case "response.output_item.done":
		item, _ := rawObject(fields["item"])
		if len(item) > 0 {
			if isHostedToolItem(item) {
				return r.upsertHostedTool(item)
			}
			return r.addToolCall(fields, item, false)
		}
	case "response.completed", "response.incomplete":
		response, err := rawObject(fields["response"])
		if err == nil {
			r.mergeCompletedResponse(response)
			return r.Message("streaming"), true
		}
	}
	return Message{}, false
}

// AddToolResult appends a persisted function_call_output item to the live
// message. Call it only after the runner has durably appended the item.
func (r *StreamingRun) AddToolResult(raw json.RawMessage) (Message, bool) {
	if r == nil {
		return Message{}, false
	}
	part, ok := ToolResultPart(raw, len(r.parts))
	if !ok {
		return Message{}, false
	}
	r.ensureStep("")
	r.parts = append(r.parts, part)
	r.updateCurrentStep(false)
	return r.Message("streaming"), true
}

// Complete reconciles the final response with streamed parts and clears all
// in-progress markers before the terminal UI event is emitted.
func (r *StreamingRun) Complete(raw json.RawMessage) Message {
	if r == nil {
		return Message{}
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) == nil {
		response, err := rawObject(envelope["response"])
		if err == nil {
			r.mergeCompletedResponse(response)
		}
	}
	for index := range r.parts {
		if r.parts[index].Type == "tool_call" {
			r.parts[index].Partial = false
		}
	}
	r.updateCurrentStep(false)
	return r.Message("complete")
}

func (r *StreamingRun) Message(status string) Message {
	if r == nil {
		return Message{}
	}
	parts := cloneParts(r.parts)
	text, reasoning := r.summary.fields(parts)
	message := Message{
		ID:               r.id,
		Sender:           "assistant",
		Text:             text,
		Timestamp:        r.timestamp,
		Parts:            parts,
		Status:           status,
		ReasoningContent: reasoning,
	}
	steps := make([]Step, 0, len(r.steps))
	for _, step := range r.steps {
		if step.EndPart > step.StartPart {
			steps = append(steps, step)
		}
	}
	if len(steps) > 1 {
		message.Steps = steps
	}
	return message
}

func (r *StreamingRun) addToolCall(event, item map[string]json.RawMessage, partial bool) (Message, bool) {
	if stringField(item["type"]) != "function_call" {
		return Message{}, false
	}
	callID := stringField(item["call_id"])
	if callID == "" {
		callID = stringField(item["id"])
	}
	if callID == "" {
		callID = fmt.Sprintf("tool-call-%d", len(r.parts))
	}
	name := stringField(item["name"])
	arguments := stringField(item["arguments"])
	r.rememberToolCall(callID, event, item)
	r.ensureStep("")
	for index := range r.parts {
		if r.parts[index].Type == "tool_call" && r.parts[index].CallID == callID {
			if name != "" {
				r.parts[index].Name = name
			}
			r.parts[index].Arguments = streamStringPointer(arguments)
			r.parts[index].Partial = partial
			r.updateCurrentStep(true)
			return r.Message("streaming"), true
		}
	}
	r.parts = append(r.parts, Part{Type: "tool_call", CallID: callID, Name: name, Arguments: streamStringPointer(arguments), Partial: partial})
	r.updateCurrentStep(true)
	return r.Message("streaming"), true
}

// upsertHostedTool shows a provider-hosted tool as it starts and as its state
// changes. It does not mark the step as a tool step: no client reply follows.
func (r *StreamingRun) upsertHostedTool(item map[string]json.RawMessage) (Message, bool) {
	part, ok := hostedToolPart(item, r.hostedRawLimit)
	if !ok {
		return Message{}, false
	}
	r.ensureStep("")
	r.replaceHostedTool(part)
	r.updateCurrentStep(false)
	return r.Message("streaming"), true
}

// replaceHostedTool updates a known hosted tool part in place, or appends it. A
// later event may carry less detail (an announcement has no result yet), so only
// non-empty fields overwrite. It reports whether the part already existed.
func (r *StreamingRun) replaceHostedTool(part Part) bool {
	for index := range r.parts {
		current := &r.parts[index]
		if current.Type != "hosted_tool" || current.CallID != part.CallID {
			continue
		}
		if part.Name != "" {
			current.Name = part.Name
		}
		if part.Status != "" {
			current.Status = part.Status
		}
		if part.Target != "" {
			current.Target = part.Target
		}
		// An event that reports a state is a full item and replaces raw, so raw and
		// status never disagree. A sparser event without a state keeps the fuller raw.
		if part.Raw != "" && (part.Status != "" || hostedRawSize(part) >= hostedRawSize(*current)) {
			current.Raw = part.Raw
			current.Truncated = part.Truncated
		}
		return true
	}
	r.parts = append(r.parts, part)
	return false
}

func (r *StreamingRun) rememberToolCall(callID string, event, item map[string]json.RawMessage) {
	if itemID := stringField(item["id"]); itemID != "" {
		r.callIDsByItemID[itemID] = callID
	}
	if itemID := stringField(event["item_id"]); itemID != "" {
		r.callIDsByItemID[itemID] = callID
	}
	if outputIndex, ok := outputIndex(event["output_index"]); ok {
		r.callIDsByOutputIndex[outputIndex] = callID
	}
}

func (r *StreamingRun) toolCallIDForEvent(event map[string]json.RawMessage) string {
	if itemID := stringField(event["item_id"]); itemID != "" {
		if callID := r.callIDsByItemID[itemID]; callID != "" {
			return callID
		}
	}
	if index, ok := outputIndex(event["output_index"]); ok {
		return r.callIDsByOutputIndex[index]
	}
	return ""
}

func (r *StreamingRun) updateToolCall(callID string, update func(string) string, partial bool, name string) bool {
	for index := range r.parts {
		part := &r.parts[index]
		if part.Type != "tool_call" || part.CallID != callID {
			continue
		}
		current := ""
		if part.Arguments != nil {
			current = *part.Arguments
		}
		part.Arguments = streamStringPointer(update(current))
		part.Partial = partial
		if name != "" {
			part.Name = name
		}
		r.updateCurrentStep(true)
		return true
	}
	return false
}

func (r *StreamingRun) mergeCompletedResponse(response map[string]json.RawMessage) {
	r.deltaPart = 0
	r.summary.invalidate()
	r.ensureStep(stringField(response["id"]))
	completed := responseParts(response)
	currentStepStart := 0
	if len(r.steps) > 0 {
		currentStepStart = r.steps[len(r.steps)-1].StartPart
	}
	// Text and reasoning parts of one response are matched in order: a hosted
	// search can split them, and a second text must not overwrite the first.
	nextByType := map[string]int{}
	for _, part := range completed {
		if part.Type == "tool_call" || part.Type == "tool_result" {
			found := false
			for index := range r.parts {
				if r.parts[index].Type != part.Type || r.parts[index].CallID != part.CallID {
					continue
				}
				found = true
				if part.Type == "tool_call" {
					r.parts[index] = part
				}
				break
			}
			if !found {
				r.parts = append(r.parts, part)
			}
			continue
		}
		if part.Type == "hosted_tool" {
			capHostedRaw(&part, r.hostedRawLimit)
			r.replaceHostedTool(part)
			continue
		}
		found := false
		start := currentStepStart
		if next, ok := nextByType[part.Type]; ok {
			start = next
		}
		for index := start; index < len(r.parts); index++ {
			if r.parts[index].Type == part.Type {
				r.parts[index].Text = part.Text
				r.parts[index].Citations = part.Citations
				nextByType[part.Type] = index + 1
				found = true
				break
			}
		}
		if !found {
			r.parts = appendTextPart(r.parts, part.Type, part.Text)
			last := len(r.parts) - 1
			// appendTextPart may have extended the previous part of this type:
			// keep the sources it already has.
			r.parts[last].Citations = mergeCitations(r.parts[last].Citations, part.Citations)
			nextByType[part.Type] = last + 1
		}
	}
	for index := range r.parts {
		if r.parts[index].Type == "tool_call" {
			r.parts[index].Partial = false
		}
	}
	r.updateCurrentStep(hasToolCall(completed))
}

func (r *StreamingRun) ensureStep(id string) {
	if len(r.steps) == 0 {
		if id == "" {
			id = fmt.Sprintf("%s-step-%d", r.id, len(r.steps)+1)
			r.currentStepIsFallback = true
		}
		r.steps = append(r.steps, Step{ID: id, StartPart: len(r.parts), EndPart: len(r.parts)})
		return
	}
	current := &r.steps[len(r.steps)-1]
	if id == "" || current.ID == id {
		return
	}
	if r.currentStepIsFallback {
		current.ID = id
		r.currentStepIsFallback = false
		return
	}
	current.EndPart = len(r.parts)
	r.steps = append(r.steps, Step{ID: id, StartPart: len(r.parts), EndPart: len(r.parts)})
}

func (r *StreamingRun) updateCurrentStep(hasTools bool) {
	if len(r.steps) == 0 {
		r.ensureStep("")
	}
	step := &r.steps[len(r.steps)-1]
	step.EndPart = len(r.parts)
	if hasTools {
		step.HasTools = true
	}
}

func outputIndex(raw json.RawMessage) (int, bool) {
	var index int
	if err := json.Unmarshal(raw, &index); err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func cloneParts(parts []Part) []Part {
	cloned := append([]Part(nil), parts...)
	for index := range cloned {
		if parts[index].Arguments != nil {
			value := *parts[index].Arguments
			cloned[index].Arguments = &value
		}
		if parts[index].Truncated != nil {
			value := *parts[index].Truncated
			cloned[index].Truncated = &value
		}
	}
	return cloned
}

func streamStringPointer(value string) *string { return &value }

// ToolResultPart projects a persisted function_call_output item to the
// frontend's renderable tool-result part.
func ToolResultPart(raw json.RawMessage, key any) (Part, bool) {
	fields, err := rawObject(raw)
	if err != nil || stringField(fields["type"]) != "function_call_output" {
		return Part{}, false
	}
	part, _, read := toolResultOutput(fields, fmt.Sprint(key))
	if part == nil {
		return Part{}, false
	}
	if read.parts != nil {
		part.Status = "success"
		for _, output := range read.parts {
			if output.Text == mcpToolErrorText {
				part.Status = "error"
				break
			}
		}
		return *part, true
	}
	var outputs []map[string]json.RawMessage
	if firstByte(fields["output"]) == '[' && json.Unmarshal(fields["output"], &outputs) == nil && outputs != nil {
		part.Status = "success"
		for _, output := range outputs {
			if stringField(output["text"]) == mcpToolErrorText {
				part.Status = "error"
				break
			}
		}
	}
	return *part, true
}
