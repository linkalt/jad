package anthropicadapter

import (
	"encoding/json"
	"strings"
	"time"
)

// responseObject is the Responses API "response" object the adapter returns.
type responseObject struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	Status            string             `json:"status"`
	Model             string             `json:"model"`
	Output            []map[string]any   `json:"output"`
	Usage             *responsesUsage    `json:"usage,omitempty"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details"`
	Error             *responseError     `json:"error"`
}

type incompleteDetails struct {
	Reason string `json:"reason"`
}

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responsesUsage struct {
	InputTokens         int            `json:"input_tokens"`
	InputTokensDetails  map[string]int `json:"input_tokens_details"`
	OutputTokens        int            `json:"output_tokens"`
	OutputTokensDetails map[string]int `json:"output_tokens_details"`
	TotalTokens         int            `json:"total_tokens"`
}

// translateResponse converts a complete Anthropic message.
func translateResponse(message *anthropicResponse, now time.Time) *responseObject {
	response := &responseObject{
		ID:        responseID(message.ID),
		Object:    "response",
		CreatedAt: now.Unix(),
		Model:     message.Model,
		Output:    outputItems(message.Content, message.ID),
	}
	response.Status, response.IncompleteDetails = statusFor(message.StopReason)
	usage := translateUsage(&message.Usage)
	response.Usage = &usage
	return response
}

// responseID turns an Anthropic message id into a Responses id.
func responseID(messageID string) string {
	if messageID == "" {
		return "resp_adapter"
	}
	return "resp_" + strings.TrimPrefix(messageID, "msg_")
}

// statusFor maps stop_reason. A stop that cut the answer short is incomplete,
// which both agent backends already treat as a normal end with a notice.
func statusFor(stopReason string) (string, *incompleteDetails) {
	switch stopReason {
	case "max_tokens", "model_context_window_exceeded":
		return "incomplete", &incompleteDetails{Reason: "max_output_tokens"}
	case "refusal":
		return "incomplete", &incompleteDetails{Reason: "content_filter"}
	case "pause_turn":
		return "incomplete", &incompleteDetails{Reason: "pause_turn"}
	}
	return "completed", nil
}

// translateUsage maps token counts. Anthropic reports cached tokens apart from
// input_tokens; Responses counts them inside input_tokens.
func translateUsage(usage *anthropicUsage) responsesUsage {
	input := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	return responsesUsage{
		InputTokens: input,
		InputTokensDetails: map[string]int{
			"cached_tokens":         usage.CacheReadInputTokens,
			"cache_creation_tokens": usage.CacheCreationInputTokens,
		},
		OutputTokens:        usage.OutputTokens,
		OutputTokensDetails: map[string]int{"reasoning_tokens": 0},
		TotalTokens:         input + usage.OutputTokens,
	}
}

// outputItems converts content blocks to Responses output items. Consecutive
// thinking blocks become one reasoning item; each text block is one message.
func outputItems(blocks []anthropicBlock, messageID string) []map[string]any {
	items := []map[string]any{}
	var thinking []anthropicBlock
	flush := func() {
		if len(thinking) > 0 {
			items = append(items, reasoningItem(len(items), messageID, thinking))
			thinking = nil
		}
	}
	for _, block := range blocks {
		switch block.Type {
		case "thinking", "redacted_thinking":
			thinking = append(thinking, block)
		case "text":
			flush()
			items = append(items, messageItem(len(items), messageID, block.Text))
		case "tool_use":
			flush()
			items = append(items, functionCallItem(block))
		}
	}
	flush()
	return items
}

func itemID(prefix, messageID string, index int) string {
	return prefix + "_" + strings.TrimPrefix(messageID, "msg_") + "_" + itoa(index)
}

func reasoningItem(index int, messageID string, blocks []anthropicBlock) map[string]any {
	item := map[string]any{
		"type":    "reasoning",
		"id":      itemID("rs", messageID, index),
		"summary": []any{},
	}
	var text strings.Builder
	for _, block := range blocks {
		if block.Thinking != nil && *block.Thinking != "" {
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(*block.Thinking)
		}
	}
	if text.Len() > 0 {
		item["content"] = []map[string]any{{"type": "reasoning_text", "text": text.String()}}
	}
	if encoded := encodeThinkingBlocks(blocks); encoded != "" {
		item["encrypted_content"] = encoded
	}
	return item
}

func messageItem(index int, messageID, text string) map[string]any {
	return map[string]any{
		"type":   "message",
		"id":     itemID("msg", messageID, index),
		"status": "completed",
		"role":   "assistant",
		"content": []map[string]any{{
			"type": "output_text", "text": text, "annotations": []any{},
		}},
	}
}

func functionCallItem(block anthropicBlock) map[string]any {
	arguments := "{}"
	if len(block.Input) > 0 {
		arguments = string(compactJSON(block.Input))
	}
	return map[string]any{
		"type":      "function_call",
		"id":        "fc_" + strings.TrimPrefix(block.ID, "toolu_"),
		"call_id":   block.ID,
		"name":      block.Name,
		"arguments": arguments,
		"status":    "completed",
	}
}

func itoa(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
