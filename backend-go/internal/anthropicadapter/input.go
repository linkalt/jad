package anthropicadapter

import (
	"encoding/json"
	"fmt"
)

// inputItem is one element of the Responses "input" array.
type inputItem struct {
	Type             string          `json:"type"`
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Arguments        string          `json:"arguments"`
	Output           json.RawMessage `json:"output"`
	EncryptedContent string          `json:"encrypted_content"`
}

// inputBuilder turns Responses items into alternating Anthropic messages.
//
// Anthropic requires every tool_use to be answered by a tool_result in the very
// next user message, and requires those results to come first in that message.
// Responses has no such rule, so the builder tracks the unanswered calls.
type inputBuilder struct {
	config  *Config
	profile *ModelProfile
	system  []anthropicBlock
	// messages are the finished and open messages in order.
	messages []anthropicMessage
	// pending holds tool_use ids of the last assistant message without a result.
	pending []string
}

// translateInput converts instructions and the input items.
func translateInput(config *Config, profile *ModelProfile, instructions string, raw json.RawMessage) ([]anthropicBlock, []anthropicMessage, *apiError) {
	builder := &inputBuilder{config: config, profile: profile}
	if instructions != "" {
		builder.system = append(builder.system, anthropicBlock{Type: "text", Text: instructions})
	}
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, nil, badRequest("input", "input is required")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		builder.addUser([]anthropicBlock{{Type: "text", Text: text}})
		return builder.finish()
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, badRequest("input", "input must be a string or an array of items")
	}
	for index, rawItem := range items {
		var item inputItem
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return nil, nil, badRequest("input", "input[%d] is not a valid item: %v", index, err)
		}
		if apiErr := builder.add(index, &item); apiErr != nil {
			return nil, nil, apiErr
		}
	}
	return builder.finish()
}

func (b *inputBuilder) finish() ([]anthropicBlock, []anthropicMessage, *apiError) {
	if apiErr := b.flushPending(); apiErr != nil {
		return nil, nil, apiErr
	}
	if len(b.messages) == 0 {
		return nil, nil, badRequest("input", "input has no content to send")
	}
	return b.system, b.messages, nil
}

func (b *inputBuilder) add(index int, item *inputItem) *apiError {
	switch item.Type {
	case "", "message":
		return b.addMessage(index, item)
	case "function_call":
		return b.addFunctionCall(index, item)
	case "function_call_output":
		return b.addFunctionResult(index, item)
	case "reasoning":
		return b.addReasoning(item)
	default:
		return unsupported("input", "input[%d]: item type %q is not supported", index, item.Type)
	}
}

func (b *inputBuilder) addMessage(index int, item *inputItem) *apiError {
	param := fmt.Sprintf("input[%d].content", index)
	switch item.Role {
	case "user":
		blocks, apiErr := convertParts(param, item.Content)
		if apiErr != nil {
			return apiErr
		}
		if apiErr := b.flushPending(); apiErr != nil {
			return apiErr
		}
		b.addUser(blocks)
	case "assistant":
		blocks, apiErr := convertParts(param, item.Content)
		if apiErr != nil {
			return apiErr
		}
		if apiErr := b.flushPending(); apiErr != nil {
			return apiErr
		}
		b.addAssistant(blocks)
	case "system", "developer":
		blocks, apiErr := convertTextParts(param, item.Content)
		if apiErr != nil {
			return apiErr
		}
		return b.addSystem(blocks)
	default:
		return badRequest(fmt.Sprintf("input[%d].role", index), "role %q is not supported", item.Role)
	}
	return nil
}

// addSystem puts leading system text into the top-level system field. After
// the conversation has started, the profile decides between a role=system
// message and a plain user message.
func (b *inputBuilder) addSystem(blocks []anthropicBlock) *apiError {
	if len(blocks) == 0 {
		return nil
	}
	if len(b.messages) == 0 {
		b.system = append(b.system, blocks...)
		return nil
	}
	if apiErr := b.flushPending(); apiErr != nil {
		return apiErr
	}
	for index := range blocks {
		blocks[index].folded = true
	}
	if b.profile.MidConversationSystem {
		b.messages = append(b.messages, anthropicMessage{Role: "system", Content: blocks})
		return nil
	}
	b.addUser(blocks)
	return nil
}

func (b *inputBuilder) addFunctionCall(index int, item *inputItem) *apiError {
	if item.CallID == "" || item.Name == "" {
		return badRequest(fmt.Sprintf("input[%d]", index), "function_call needs call_id and name")
	}
	// A call right after another call or assistant text joins that turn.
	b.addAssistant([]anthropicBlock{{
		Type: "tool_use", ID: item.CallID, Name: item.Name,
		Input: b.toolInput(item.Arguments),
	}})
	b.pending = append(b.pending, item.CallID)
	return nil
}

// toolInput parses function-call arguments. Anthropic needs a JSON object; a
// string that is not one is passed back under invalid_arguments_key so the
// model can see what it sent.
func (b *inputBuilder) toolInput(arguments string) json.RawMessage {
	if arguments == "" {
		return json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &object) == nil && object != nil {
		return compactJSON([]byte(arguments))
	}
	wrapped, _ := json.Marshal(map[string]string{b.config.InvalidArgumentsKey: arguments})
	return wrapped
}

func (b *inputBuilder) addFunctionResult(index int, item *inputItem) *apiError {
	param := fmt.Sprintf("input[%d].output", index)
	content, apiErr := convertResultContent(param, item.Output)
	if apiErr != nil {
		return apiErr
	}
	position := indexOf(b.pending, item.CallID)
	if position < 0 {
		// No open call carries this id (for example after compaction). Anthropic
		// would reject a lone tool_result, so keep the text as user content.
		text := anthropicBlock{Type: "text", Text: fmt.Sprintf("Result of tool call %s:", item.CallID)}
		b.addUser(append([]anthropicBlock{text}, content...))
		return nil
	}
	b.pending = append(b.pending[:position], b.pending[position+1:]...)
	b.addToolResult(anthropicBlock{Type: "tool_result", ToolUseID: item.CallID, Content: content})
	return nil
}

func (b *inputBuilder) addReasoning(item *inputItem) *apiError {
	blocks := decodeThinkingBlocks(item.EncryptedContent)
	if len(blocks) == 0 {
		return nil
	}
	if apiErr := b.flushPending(); apiErr != nil {
		return apiErr
	}
	b.addAssistant(blocks)
	return nil
}

// flushPending answers every unanswered tool call so the next message is valid.
func (b *inputBuilder) flushPending() *apiError {
	if len(b.pending) == 0 {
		return nil
	}
	if !b.config.SynthesizeMissingToolResults {
		return badRequest("input", "function_call %s has no function_call_output", b.pending[0])
	}
	for _, id := range b.pending {
		b.addToolResult(anthropicBlock{
			Type: "tool_result", ToolUseID: id, IsError: true,
			Content: []anthropicBlock{{Type: "text", Text: b.config.MissingToolResultText}},
		})
	}
	b.pending = nil
	return nil
}

// addUser appends blocks to the last user message or opens a new one.
func (b *inputBuilder) addUser(blocks []anthropicBlock) {
	if len(blocks) == 0 {
		return
	}
	if last := b.last(); last != nil && last.Role == "user" {
		last.Content = append(last.Content, blocks...)
		return
	}
	b.messages = append(b.messages, anthropicMessage{Role: "user", Content: blocks})
}

// addToolResult places a result after the results already in the user message
// but before any other block. Text can already be there: an orphan result (one
// whose call is not open) is kept as text while real calls are still pending.
func (b *inputBuilder) addToolResult(block anthropicBlock) {
	last := b.last()
	if last == nil || last.Role != "user" {
		b.messages = append(b.messages, anthropicMessage{Role: "user", Content: []anthropicBlock{block}})
		return
	}
	position := 0
	for position < len(last.Content) && last.Content[position].Type == "tool_result" {
		position++
	}
	last.Content = append(last.Content, anthropicBlock{})
	copy(last.Content[position+1:], last.Content[position:])
	last.Content[position] = block
}

func (b *inputBuilder) addAssistant(blocks []anthropicBlock) {
	if len(blocks) == 0 {
		return
	}
	if last := b.last(); last != nil && last.Role == "assistant" {
		last.Content = append(last.Content, blocks...)
		return
	}
	b.messages = append(b.messages, anthropicMessage{Role: "assistant", Content: blocks})
}

func (b *inputBuilder) last() *anthropicMessage {
	if len(b.messages) == 0 {
		return nil
	}
	return &b.messages[len(b.messages)-1]
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}
