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
			r.parts = appendTextPart(r.parts, "text", delta)
			r.updateCurrentStep(false)
			return r.Message("streaming"), true
		}
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if delta := stringField(fields["delta"]); strings.TrimSpace(delta) != "" {
			r.ensureStep("")
			r.parts = appendTextPart(r.parts, "reasoning", delta)
			r.updateCurrentStep(false)
			return r.Message("streaming"), true
		}
	case "response.output_item.added":
		item, _ := rawObject(fields["item"])
		if len(item) > 0 {
			if stringField(item["type"]) == "web_search_call" {
				return r.upsertWebSearch(item)
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
			if stringField(item["type"]) == "web_search_call" {
				return r.upsertWebSearch(item)
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
	text, reasoning := FieldsFromParts(parts)
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

// upsertWebSearch shows a provider-hosted search as it starts and as its state
// changes. It does not mark the step as a tool step: no client reply follows.
func (r *StreamingRun) upsertWebSearch(item map[string]json.RawMessage) (Message, bool) {
	part, ok := webSearchPart(item)
	if !ok {
		return Message{}, false
	}
	r.ensureStep("")
	if r.replaceWebSearch(part) {
		r.updateCurrentStep(false)
		return r.Message("streaming"), true
	}
	r.parts = append(r.parts, part)
	r.updateCurrentStep(false)
	return r.Message("streaming"), true
}

// replaceWebSearch updates a known search part in place. A later event may carry
// less detail (an empty query), so only non-empty fields overwrite.
func (r *StreamingRun) replaceWebSearch(part Part) bool {
	for index := range r.parts {
		current := &r.parts[index]
		if current.Type != "web_search" || current.CallID != part.CallID {
			continue
		}
		if part.Query != "" {
			current.Query = part.Query
		}
		if part.Status != "" {
			current.Status = part.Status
		}
		return true
	}
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
		if part.Type == "web_search" {
			if !r.replaceWebSearch(part) {
				r.parts = append(r.parts, part)
			}
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
	part, _ := toolResult(fields, fmt.Sprint(key))
	if part == nil {
		return Part{}, false
	}
	var outputs []map[string]json.RawMessage
	if json.Unmarshal(fields["output"], &outputs) == nil && outputs != nil {
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
