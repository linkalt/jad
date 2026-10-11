package webui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"example.com/agent-harness/backend-go/internal/imagestore"
)

// NormalizeTranscript converts JSONL transcript entries into frontend messages.
// Prefix is "o" for current transcript offsets and "b" for backup offsets.
func NormalizeTranscript(records []TranscriptRecord) ([]Message, error) {
	return normalizeTranscript(records, nil)
}

// normalizeTranscript is NormalizeTranscript that also reports, for every tool result, the
// id of the message it joined and the record it came from.
func normalizeTranscript(records []TranscriptRecord, onToolResult func(messageID string, part Part, entry TranscriptRecord)) ([]Message, error) {
	state := reducerState{toolMessages: make(map[string]int), activeToolMessage: -1, onToolResult: onToolResult}
	for _, entry := range records {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entry.Raw, &fields); err != nil || fields == nil {
			return nil, fmt.Errorf("transcript record at byte %d is not a JSON object", entry.Offset)
		}
		prefix := entry.Prefix
		if prefix == "" {
			prefix = "o"
		}
		key := fmt.Sprintf("%s%d", prefix, entry.Offset)
		typeName := stringField(fields["type"])
		role := stringField(fields["role"])
		if typeName == "message" && role == "user" {
			message := userMessage(fields, key, entry)
			if message != nil {
				state.toolMessages = make(map[string]int)
				state.activeToolMessage = -1
				state.activeStepID = ""
				state.messages = append(state.messages, *message)
				continue
			}
		}
		if typeName == "function_call_output" {
			if part, callID := toolResult(fields, key); part != nil {
				state.addToolResult(callID, *part, entry)
				continue
			}
		}
		if typeName != "response.completed" {
			continue
		}
		response, err := rawObject(fields["response"])
		if err != nil {
			return nil, fmt.Errorf("decode response at byte %d: %w", entry.Offset, err)
		}
		parts := responseParts(response)
		if len(parts) == 0 {
			continue
		}
		responseID := stringField(response["id"])
		id := responseID
		if id == "" {
			id = "session-assistant-" + key
		}
		stepID := responseID
		if stepID == "" {
			stepID = "session-step-" + key
		}
		timestamp := timestampFromRaw(response["created_at"])
		if timestamp == "" {
			timestamp = timestampFromRaw(fields["created_at"])
		}
		if hasToolCall(parts) {
			state.addResponseWithTools(id, stepID, timestamp, parts, entry)
			continue
		}
		if state.activeToolMessage >= 0 && hasMessageToolCall(state.messages[state.activeToolMessage]) {
			state.appendResponseStep(stepID, parts, false)
			state.toolMessages = make(map[string]int)
			state.activeToolMessage = -1
			state.activeStepID = ""
			continue
		}
		message := Message{ID: id, Sender: "assistant", Timestamp: timestamp, Parts: parts, Status: "complete"}
		setCursor(&message, entry)
		message.Text, message.ReasoningContent = FieldsFromParts(parts)
		state.messages = append(state.messages, message)
	}
	return state.messages, nil
}

type reducerState struct {
	messages          []Message
	toolMessages      map[string]int
	activeToolMessage int
	activeStepID      string
	onToolResult      func(messageID string, part Part, entry TranscriptRecord)
}

func (s *reducerState) addToolResult(callID string, part Part, entry TranscriptRecord) {
	if index, ok := s.toolMessages[callID]; ok && index >= 0 && index < len(s.messages) {
		message := s.messages[index]
		message.Parts = append(message.Parts, part)
		if len(message.Steps) > 0 {
			message.Steps[len(message.Steps)-1].EndPart = len(message.Parts)
		}
		message.Text, message.ReasoningContent = FieldsFromParts(message.Parts)
		s.messages[index] = message
		if s.onToolResult != nil {
			s.onToolResult(message.ID, part, entry)
		}
		return
	}
	message := Message{
		ID:        "session-tool-" + recordKey(entry),
		Sender:    "assistant",
		Timestamp: timestampFromRaw(recordCreatedAt(entry.Raw)),
		Parts:     []Part{part},
		Status:    "complete",
	}
	setCursor(&message, entry)
	message.Text, message.ReasoningContent = FieldsFromParts(message.Parts)
	s.messages = append(s.messages, message)
	if s.onToolResult != nil {
		s.onToolResult(message.ID, part, entry)
	}
}

func (s *reducerState) addResponseWithTools(id, stepID, timestamp string, parts []Part, entry TranscriptRecord) {
	if s.activeToolMessage < 0 || s.activeToolMessage >= len(s.messages) {
		message := Message{ID: id, Sender: "assistant", Timestamp: timestamp, Parts: append([]Part(nil), parts...), Status: "complete"}
		setCursor(&message, entry)
		message.Text, message.ReasoningContent = FieldsFromParts(message.Parts)
		s.messages = append(s.messages, message)
		s.activeToolMessage = len(s.messages) - 1
		s.activeStepID = stepID
		for _, part := range parts {
			if part.Type == "tool_call" {
				s.toolMessages[part.CallID] = s.activeToolMessage
			}
		}
		return
	}
	s.appendResponseStep(stepID, parts, true)
	for _, part := range parts {
		if part.Type == "tool_call" {
			s.toolMessages[part.CallID] = s.activeToolMessage
		}
	}
}

func (s *reducerState) appendResponseStep(stepID string, parts []Part, hasTools bool) {
	index := s.activeToolMessage
	if index < 0 || index >= len(s.messages) {
		return
	}
	message := s.messages[index]
	start := len(message.Parts)
	previousSteps := message.Steps
	if previousSteps == nil {
		previousID := s.activeStepID
		if previousID == "" {
			previousID = "step-before-" + stepID
		}
		previousSteps = []Step{{ID: previousID, StartPart: 0, EndPart: start, HasTools: true}}
	} else {
		previousSteps = append([]Step(nil), previousSteps...)
		previousSteps[len(previousSteps)-1].EndPart = start
	}
	message.Parts = append(message.Parts, parts...)
	message.Steps = append(previousSteps, Step{ID: stepID, StartPart: start, EndPart: start + len(parts), HasTools: hasTools})
	message.Text, message.ReasoningContent = FieldsFromParts(message.Parts)
	s.messages[index] = message
	s.activeStepID = stepID
}

func userMessage(fields map[string]json.RawMessage, key string, entry TranscriptRecord) *Message {
	var content []UserContentPart
	if err := json.Unmarshal(fields["content"], &content); err != nil {
		return nil
	}
	text, images, files, visible := UserContent(content)
	if !visible {
		return nil
	}
	message := Message{
		ID:        "session-user-" + key,
		Sender:    "user",
		Text:      text,
		Timestamp: timestampFromRaw(fields["created_at"]),
		Parts:     []Part{{Type: "text", Text: text, Images: images, Files: files}},
	}
	setCursor(&message, entry)
	return &message
}

func responseParts(response map[string]json.RawMessage) []Part {
	var outputs []json.RawMessage
	if err := json.Unmarshal(response["output"], &outputs); err != nil {
		return nil
	}
	parts := make([]Part, 0)
	for _, raw := range outputs {
		var output map[string]json.RawMessage
		if json.Unmarshal(raw, &output) != nil || output == nil {
			continue
		}
		typeName := stringField(output["type"])
		if typeName == "reasoning" {
			for _, text := range reasoningTexts(output) {
				parts = appendTextPart(parts, "reasoning", text)
			}
			continue
		}
		if typeName == "message" {
			var contents []struct {
				Type        string          `json:"type"`
				Text        string          `json:"text"`
				Annotations json.RawMessage `json:"annotations"`
			}
			if json.Unmarshal(output["content"], &contents) == nil {
				for _, content := range contents {
					switch content.Type {
					case "output_text":
						parts = appendCitedTextPart(parts, content.Text, citationsFromAnnotations(content.Annotations))
					case "refusal":
						parts = appendTextPart(parts, "text", content.Text)
					case "reasoning":
						parts = appendTextPart(parts, "reasoning", content.Text)
					}
				}
			}
			continue
		}
		if part, ok := hostedToolPart(output, 0); ok {
			parts = append(parts, part)
			continue
		}
		if typeName == "function_call" {
			callID := stringField(output["call_id"])
			name := stringField(output["name"])
			if callID == "" || name == "" {
				continue
			}
			arguments := stringField(output["arguments"])
			parts = append(parts, Part{Type: "tool_call", CallID: callID, Name: name, Arguments: &arguments})
		}
	}
	return parts
}

func toolResult(fields map[string]json.RawMessage, key string) (*Part, string) {
	callID := stringField(fields["call_id"])
	if callID == "" {
		callID = "tool-call-" + key
	}
	outputRaw := fields["output"]
	output := toolOutputForUI(outputRaw)
	status := "unknown"
	firstLine, _, _ := strings.Cut(output, "\n")
	var parsed map[string]json.RawMessage
	if json.Unmarshal([]byte(firstLine), &parsed) == nil {
		if state := stringField(parsed["status"]); state != "" {
			if state == "ok" {
				status = "success"
			} else {
				status = "error"
			}
		}
	}
	part := &Part{Type: "tool_result", CallID: callID, Output: output, Status: status, Images: storedImageNames(outputRaw), InlineImages: len(inlineImageURLs(outputRaw))}
	return part, callID
}

// storedImageNames returns the stored-image file names a tool output refers to, in
// order and without repeats. An image still inline as a data URL is not listed.
func storedImageNames(raw json.RawMessage) []string {
	var parts []struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var names []string
	for _, part := range parts {
		name, ok := strings.CutPrefix(part.ImageURL, imagestore.RefPrefix)
		if part.Type != "input_image" || !ok {
			continue
		}
		if _, valid := imagestore.ValidName(name); !valid || slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}
	return names
}

func toolOutputForUI(raw json.RawMessage) string {
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) == nil && parts != nil {
		lines := make([]string, 0, len(parts))
		for _, part := range parts {
			typeName := stringField(part["type"])
			switch typeName {
			case "input_text":
				lines = append(lines, stringField(part["text"]))
			case "input_image":
				lines = append(lines, "[MCP image content]")
			case "input_file":
				filename := stringField(part["filename"])
				if filename == "" {
					lines = append(lines, "[MCP file]")
				} else {
					lines = append(lines, "[MCP file: "+filename+"]")
				}
			default:
				if typeName == "" {
					typeName = "unknown"
				}
				lines = append(lines, "[MCP content: "+typeName+"]")
			}
		}
		return strings.Join(lines, "\n")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	if len(raw) == 0 {
		return "null"
	}
	return string(raw)
}

func appendTextPart(parts []Part, kind, text string) []Part {
	if text == "" {
		return parts
	}
	if len(parts) > 0 && parts[len(parts)-1].Type == kind {
		parts[len(parts)-1].Text += text
		return parts
	}
	return append(parts, Part{Type: kind, Text: text})
}

// appendCitedTextPart appends answer text like appendTextPart and merges the
// url_citation sources into the text part that now holds it.
func appendCitedTextPart(parts []Part, text string, citations []Citation) []Part {
	if text == "" {
		return parts
	}
	parts = appendTextPart(parts, "text", text)
	last := &parts[len(parts)-1]
	last.Citations = mergeCitations(last.Citations, citations)
	return parts
}

// mergeCitations adds the new sources after the existing ones, skipping a URL
// that is already listed, so the same page cited twice shows once.
func mergeCitations(existing, added []Citation) []Citation {
	for _, citation := range added {
		duplicate := false
		for _, known := range existing {
			if known.URL == citation.URL {
				duplicate = true
				break
			}
		}
		if !duplicate {
			existing = append(existing, citation)
		}
	}
	return existing
}

// citationsFromAnnotations reads the url_citation entries of an output_text
// content item. Other annotation types and entries without a URL are ignored.
func citationsFromAnnotations(raw json.RawMessage) []Citation {
	var annotations []struct {
		Type  string `json:"type"`
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if json.Unmarshal(raw, &annotations) != nil {
		return nil
	}
	var citations []Citation
	for _, annotation := range annotations {
		if annotation.Type == "url_citation" && annotation.URL != "" {
			citations = mergeCitations(citations, []Citation{{URL: annotation.URL, Title: annotation.Title}})
		}
	}
	return citations
}

func hasToolCall(parts []Part) bool {
	for _, part := range parts {
		if part.Type == "tool_call" {
			return true
		}
	}
	return false
}

func hasMessageToolCall(message Message) bool { return hasToolCall(message.Parts) }

func setCursor(message *Message, entry TranscriptRecord) {
	if entry.HasCursor {
		cursor := entry.Cursor
		message.Cursor = &cursor
	}
}

func recordKey(entry TranscriptRecord) string {
	prefix := entry.Prefix
	if prefix == "" {
		prefix = "o"
	}
	return fmt.Sprintf("%s%d", prefix, entry.Offset)
}

func stringField(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("expected object")
	}
	return result, nil
}

// reasoningTexts is the text of one reasoning item. OpenAI puts it in `summary`;
// vLLM-style providers and the Anthropic adapter put it in `content` as
// reasoning_text and leave `summary` empty. A summary wins when both exist, so a
// provider that sends both does not show the same thinking twice.
func reasoningTexts(output map[string]json.RawMessage) []string {
	if summary := textArray(output["summary"]); len(summary) > 0 {
		return summary
	}
	return textArray(output["content"])
}

func textArray(raw json.RawMessage) []string {
	var entries []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	text := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Text != "" {
			text = append(text, entry.Text)
		}
	}
	return text
}

func timestampFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var seconds float64
	if json.Unmarshal(raw, &seconds) == nil {
		return time.Unix(0, int64(seconds*float64(time.Second))).Local().Format("3:04 PM")
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z07:00", "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Local().Format("3:04 PM")
		}
	}
	return ""
}

func recordCreatedAt(raw json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	return fields["created_at"]
}
