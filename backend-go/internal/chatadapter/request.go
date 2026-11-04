package chatadapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// responsesRequest holds the Responses request fields the adapter translates. Fields
// not listed here (store, include, metadata, prompt_cache_key, truncation, ...) are
// dropped: they do not change the generated text.
type responsesRequest struct {
	Model              string          `json:"model"`
	Instructions       json.RawMessage `json:"instructions"`
	Input              json.RawMessage `json:"input"`
	Tools              []toolDef       `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls"`
	Temperature        *float64        `json:"temperature"`
	TopP               *float64        `json:"top_p"`
	MaxOutputTokens    *int            `json:"max_output_tokens"`
	User               string          `json:"user"`
	Stream             bool            `json:"stream"`
	PreviousResponseID string          `json:"previous_response_id"`
	Conversation       json.RawMessage `json:"conversation"`
	Background         bool            `json:"background"`
	Reasoning          *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Text *struct {
		Format json.RawMessage `json:"format"`
	} `json:"text"`
}

type toolDef struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

// chatRequest is the Chat Completions body.
type chatRequest struct {
	Model               string           `json:"model"`
	Messages            []map[string]any `json:"messages"`
	Tools               []map[string]any `json:"tools,omitempty"`
	ToolChoice          any              `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool            `json:"parallel_tool_calls,omitempty"`
	Temperature         *float64         `json:"temperature,omitempty"`
	TopP                *float64         `json:"top_p,omitempty"`
	MaxCompletionTokens *int             `json:"max_completion_tokens,omitempty"`
	MaxTokens           *int             `json:"max_tokens,omitempty"`
	ReasoningEffort     string           `json:"reasoning_effort,omitempty"`
	ResponseFormat      map[string]any   `json:"response_format,omitempty"`
	User                string           `json:"user,omitempty"`
	Stream              bool             `json:"stream"`
	StreamOptions       map[string]any   `json:"stream_options,omitempty"`
}

// translateRequest turns a Responses request body into a Chat Completions body.
func translateRequest(config *Config, body []byte) ([]byte, string, *apiError) {
	var request responsesRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, "", badRequest("", "the request is not a valid Responses request: %v", err)
	}
	if strings.TrimSpace(request.Model) == "" {
		return nil, "", badRequest("model", "model is required")
	}
	if !request.Stream {
		return nil, "", badRequest("stream", "only streaming requests are served")
	}
	if request.PreviousResponseID != "" || (len(request.Conversation) > 0 && !isNull(request.Conversation)) || request.Background {
		return nil, "", badRequest("previous_response_id", "previous_response_id, conversation and background are not supported: the adapter keeps no state, send the full input")
	}
	builder := &messageBuilder{config: config}
	if len(request.Instructions) > 0 && !isNull(request.Instructions) {
		var instructions string
		if err := json.Unmarshal(request.Instructions, &instructions); err != nil {
			return nil, "", badRequest("instructions", "instructions must be a string")
		}
		if instructions != "" {
			builder.messages = append(builder.messages, map[string]any{"role": config.SystemRole, "content": instructions})
		}
	}
	if apiErr := builder.addInput(request.Input); apiErr != nil {
		return nil, "", apiErr
	}
	chat := chatRequest{
		Model: request.Model, Messages: builder.messages,
		ParallelToolCalls: request.ParallelToolCalls, Temperature: request.Temperature, TopP: request.TopP,
		User: request.User, Stream: true,
	}
	for index, tool := range request.Tools {
		if tool.Type != "function" {
			return nil, "", badRequest(fmt.Sprintf("tools[%d]", index), "tool type %q is not supported: Chat Completions serves function tools only, hosted tools are not available", tool.Type)
		}
		function := map[string]any{"name": tool.Name}
		if tool.Description != "" {
			function["description"] = tool.Description
		}
		if len(tool.Parameters) > 0 && !isNull(tool.Parameters) {
			function["parameters"] = tool.Parameters
		}
		if tool.Strict != nil {
			function["strict"] = *tool.Strict
		}
		chat.Tools = append(chat.Tools, map[string]any{"type": "function", "function": function})
	}
	if choice, apiErr := translateToolChoice(request.ToolChoice); apiErr != nil {
		return nil, "", apiErr
	} else if choice != nil && len(chat.Tools) > 0 {
		chat.ToolChoice = choice
	}
	if len(chat.Tools) == 0 {
		chat.ParallelToolCalls = nil
	}
	if request.MaxOutputTokens != nil {
		if config.MaxTokensField == MaxTokensFieldLegacy {
			chat.MaxTokens = request.MaxOutputTokens
		} else {
			chat.MaxCompletionTokens = request.MaxOutputTokens
		}
	}
	if request.Reasoning != nil {
		chat.ReasoningEffort = request.Reasoning.Effort
	}
	if request.Text != nil {
		format, apiErr := translateFormat(request.Text.Format)
		if apiErr != nil {
			return nil, "", apiErr
		}
		chat.ResponseFormat = format
	}
	if config.IncludeUsage {
		chat.StreamOptions = map[string]any{"include_usage": true}
	}
	payload, err := json.Marshal(chat)
	if err != nil {
		return nil, "", &apiError{Status: 500, Type: "api_error", Code: "encode_failed", Message: err.Error()}
	}
	if len(config.ExtraBody) > 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil {
			return nil, "", &apiError{Status: 500, Type: "api_error", Code: "encode_failed", Message: err.Error()}
		}
		for name, value := range config.ExtraBody {
			fields[name] = value
		}
		if payload, err = json.Marshal(fields); err != nil {
			return nil, "", &apiError{Status: 500, Type: "api_error", Code: "encode_failed", Message: err.Error()}
		}
	}
	return payload, request.Model, nil
}

func translateToolChoice(raw json.RawMessage) (any, *apiError) {
	if len(raw) == 0 || isNull(raw) {
		return nil, nil
	}
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		switch mode {
		case "auto", "none", "required":
			return mode, nil
		}
		return nil, badRequest("tool_choice", "tool_choice %q is not supported", mode)
	}
	var object struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &object) == nil && object.Type == "function" && object.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": object.Name}}, nil
	}
	return nil, badRequest("tool_choice", "tool_choice must be auto, none, required or a function")
}

func translateFormat(raw json.RawMessage) (map[string]any, *apiError) {
	if len(raw) == 0 || isNull(raw) {
		return nil, nil
	}
	var format struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"schema"`
		Strict      *bool           `json:"strict"`
	}
	if err := json.Unmarshal(raw, &format); err != nil {
		return nil, badRequest("text.format", "text.format must be an object")
	}
	switch format.Type {
	case "", "text":
		return nil, nil
	case "json_object":
		return map[string]any{"type": "json_object"}, nil
	case "json_schema":
		schema := map[string]any{"name": format.Name, "schema": format.Schema}
		if format.Description != "" {
			schema["description"] = format.Description
		}
		if format.Strict != nil {
			schema["strict"] = *format.Strict
		}
		return map[string]any{"type": "json_schema", "json_schema": schema}, nil
	}
	return nil, badRequest("text.format", "text.format type %q is not supported", format.Type)
}

// inputItem is one element of the Responses input array.
type inputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	Summary   []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

// part is one content part of a Responses message.
type part struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Refusal  string `json:"refusal"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail"`
	FileID   string `json:"file_id"`
	FileData string `json:"file_data"`
	FileURL  string `json:"file_url"`
	Filename string `json:"filename"`
}

// messageBuilder turns Responses items into Chat Completions messages.
//
// Chat Completions wants every assistant tool call answered by a tool message right
// after it, before any other message. Responses has no such rule, so the builder keeps
// the calls not answered yet, answers the ones the history never answers, and holds
// back what cannot go between tool messages (images a tool returned, a result whose
// call is not in the history) until every call is answered.
type messageBuilder struct {
	config   *Config
	messages []map[string]any
	pending  []string
	deferred []map[string]any
	// reasoning is the text of the last reasoning item, for the next assistant message.
	reasoning string
}

func (b *messageBuilder) addInput(raw json.RawMessage) *apiError {
	if len(raw) == 0 || isNull(raw) {
		return badRequest("input", "input is required")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		b.messages = append(b.messages, map[string]any{"role": "user", "content": text})
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return badRequest("input", "input must be a string or an array of items")
	}
	for index, rawItem := range items {
		var item inputItem
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return badRequest("input", "input[%d] is not a valid item: %v", index, err)
		}
		if apiErr := b.add(index, &item); apiErr != nil {
			return apiErr
		}
	}
	b.answerPending()
	if len(b.messages) == 0 {
		return badRequest("input", "input has no content to send")
	}
	return nil
}

func (b *messageBuilder) add(index int, item *inputItem) *apiError {
	param := fmt.Sprintf("input[%d]", index)
	switch item.Type {
	case "", "message":
		return b.addMessage(param, item)
	case "function_call":
		if item.CallID == "" || item.Name == "" {
			return badRequest(param, "function_call needs call_id and name")
		}
		arguments := item.Arguments
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		call := map[string]any{"id": item.CallID, "type": "function", "function": map[string]any{"name": item.Name, "arguments": arguments}}
		if last := b.last(); last != nil && last["role"] == "assistant" {
			calls, _ := last["tool_calls"].([]map[string]any)
			last["tool_calls"] = append(calls, call)
		} else {
			b.appendAssistant(map[string]any{"role": "assistant", "content": nil, "tool_calls": []map[string]any{call}})
		}
		b.pending = append(b.pending, item.CallID)
		return nil
	case "function_call_output":
		return b.addResult(param, item)
	case "reasoning":
		if b.config.EchoReasoningField == "" {
			return nil
		}
		b.reasoning = reasoningText(item)
		return nil
	default:
		return badRequest(param, "item type %q is not supported by the Chat Completions adapter", item.Type)
	}
}

func (b *messageBuilder) addMessage(param string, item *inputItem) *apiError {
	parts, apiErr := decodeParts(param+".content", item.Content)
	if apiErr != nil {
		return apiErr
	}
	switch item.Role {
	case "user":
		content, apiErr := userContent(param+".content", parts)
		if apiErr != nil {
			return apiErr
		}
		b.answerPending()
		b.reasoning = ""
		b.messages = append(b.messages, map[string]any{"role": "user", "content": content})
	case "assistant":
		text, apiErr := textOf(param+".content", parts, true)
		if apiErr != nil {
			return apiErr
		}
		b.answerPending()
		b.appendAssistant(map[string]any{"role": "assistant", "content": text})
	case "system", "developer":
		text, apiErr := textOf(param+".content", parts, false)
		if apiErr != nil {
			return apiErr
		}
		b.answerPending()
		b.reasoning = ""
		b.messages = append(b.messages, map[string]any{"role": b.config.SystemRole, "content": text})
	default:
		return badRequest(param+".role", "role %q is not supported", item.Role)
	}
	return nil
}

// appendAssistant adds an assistant message, carrying the reasoning item before it.
func (b *messageBuilder) appendAssistant(message map[string]any) {
	if b.reasoning != "" {
		message[b.config.EchoReasoningField] = b.reasoning
		b.reasoning = ""
	}
	b.messages = append(b.messages, message)
}

func (b *messageBuilder) addResult(param string, item *inputItem) *apiError {
	parts, apiErr := decodeParts(param+".output", item.Output)
	if apiErr != nil {
		return apiErr
	}
	var text strings.Builder
	var images []map[string]any
	for index, one := range parts {
		switch one.Type {
		case "input_text", "output_text", "text":
			text.WriteString(one.Text)
		case "input_image", "input_file":
			converted, apiErr := convertPart(fmt.Sprintf("%s.output[%d]", param, index), &one)
			if apiErr != nil {
				return apiErr
			}
			images = append(images, converted)
		default:
			return badRequest(fmt.Sprintf("%s.output[%d]", param, index), "content part type %q is not supported", one.Type)
		}
	}
	position := -1
	for index, id := range b.pending {
		if id == item.CallID {
			position = index
		}
	}
	if position < 0 {
		// No open call has this id (for example after compaction); Chat Completions
		// refuses a lone tool message, so the result goes in as user text.
		b.deferred = append(b.deferred, map[string]any{"type": "text", "text": fmt.Sprintf("Result of tool call %s:\n%s", item.CallID, text.String())})
		b.deferred = append(b.deferred, images...)
		b.flushDeferred()
		return nil
	}
	b.pending = append(b.pending[:position], b.pending[position+1:]...)
	b.messages = append(b.messages, map[string]any{"role": "tool", "tool_call_id": item.CallID, "content": text.String()})
	if len(images) > 0 {
		b.deferred = append(b.deferred, map[string]any{"type": "text", "text": fmt.Sprintf("Attachments returned by tool call %s:", item.CallID)})
		b.deferred = append(b.deferred, images...)
	}
	b.flushDeferred()
	return nil
}

// answerPending answers the calls the history has no result for, then sends what was held back.
func (b *messageBuilder) answerPending() {
	for _, id := range b.pending {
		b.messages = append(b.messages, map[string]any{"role": "tool", "tool_call_id": id, "content": b.config.MissingToolResultText})
	}
	b.pending = nil
	b.flushDeferred()
}

// flushDeferred sends the held-back parts as one user message once no call is open.
func (b *messageBuilder) flushDeferred() {
	if len(b.pending) > 0 || len(b.deferred) == 0 {
		return
	}
	b.messages = append(b.messages, map[string]any{"role": "user", "content": b.deferred})
	b.deferred = nil
}

func (b *messageBuilder) last() map[string]any {
	if len(b.messages) == 0 {
		return nil
	}
	return b.messages[len(b.messages)-1]
}

func reasoningText(item *inputItem) string {
	var text strings.Builder
	parts, _ := decodeParts("", item.Content)
	for _, one := range parts {
		if one.Text != "" {
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(one.Text)
		}
	}
	if text.Len() == 0 {
		for _, summary := range item.Summary {
			if summary.Text != "" {
				if text.Len() > 0 {
					text.WriteString("\n\n")
				}
				text.WriteString(summary.Text)
			}
		}
	}
	return text.String()
}

func decodeParts(param string, raw json.RawMessage) ([]part, *apiError) {
	if len(raw) == 0 || isNull(raw) {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []part{{Type: "input_text", Text: text}}, nil
	}
	var parts []part
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, badRequest(param, "content must be a string or an array of parts")
	}
	return parts, nil
}

// userContent is a string when the message is one text part, else an array of parts.
func userContent(param string, parts []part) (any, *apiError) {
	if len(parts) == 1 && isTextPart(parts[0].Type) {
		return parts[0].Text, nil
	}
	converted := make([]map[string]any, 0, len(parts))
	for index, one := range parts {
		if isTextPart(one.Type) && one.Text == "" {
			continue
		}
		value, apiErr := convertPart(fmt.Sprintf("%s[%d]", param, index), &one)
		if apiErr != nil {
			return nil, apiErr
		}
		converted = append(converted, value)
	}
	return converted, nil
}

// textOf joins the text of assistant or system content; other parts are refused.
func textOf(param string, parts []part, allowRefusal bool) (string, *apiError) {
	var text strings.Builder
	for index, one := range parts {
		switch {
		case isTextPart(one.Type):
			text.WriteString(one.Text)
		case one.Type == "refusal" && allowRefusal:
			text.WriteString(one.Refusal)
		default:
			return "", badRequest(fmt.Sprintf("%s[%d]", param, index), "content part type %q is not supported in this message", one.Type)
		}
	}
	return text.String(), nil
}

func isTextPart(kind string) bool {
	return kind == "input_text" || kind == "output_text" || kind == "text"
}

func convertPart(param string, one *part) (map[string]any, *apiError) {
	switch {
	case isTextPart(one.Type):
		return map[string]any{"type": "text", "text": one.Text}, nil
	case one.Type == "input_image":
		if one.ImageURL == "" {
			return nil, badRequest(param, "input_image needs image_url; file_id images are not supported")
		}
		image := map[string]any{"url": one.ImageURL}
		if one.Detail != "" {
			image["detail"] = one.Detail
		}
		return map[string]any{"type": "image_url", "image_url": image}, nil
	case one.Type == "input_file":
		file := map[string]any{}
		switch {
		case one.FileData != "":
			file["file_data"] = one.FileData
		case one.FileID != "":
			file["file_id"] = one.FileID
		default:
			return nil, badRequest(param, "input_file needs file_data or file_id; file_url is not supported")
		}
		if one.Filename != "" {
			file["filename"] = one.Filename
		}
		return map[string]any{"type": "file", "file": file}, nil
	}
	return nil, badRequest(param, "content part type %q is not supported", one.Type)
}

func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}
