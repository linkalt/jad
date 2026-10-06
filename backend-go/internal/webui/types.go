// Package webui defines the frontend-facing transcript message contract and
// normalization helpers shared by HTTP handlers and session catalog views.
package webui

import (
	"encoding/json"
	"reflect"
	"strings"
	"unicode/utf8"
)

// PartTruncation reports the UTF-8 byte size before a part was capped for transport.
type PartTruncation struct {
	Bytes int `json:"bytes"`
}

// Part is one renderable segment of a Web UI message.
type Part struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	CallID    string          `json:"callId,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments *string         `json:"arguments,omitempty"`
	Output    string          `json:"output,omitempty"`
	Status    string          `json:"status,omitempty"`
	Partial   bool            `json:"partial,omitempty"`
	Truncated *PartTruncation `json:"truncated,omitempty"`
}

// Step describes the part range produced by one assistant response step.
type Step struct {
	ID        string `json:"id"`
	StartPart int    `json:"startPart"`
	EndPart   int    `json:"endPart"`
	HasTools  bool   `json:"hasTools"`
}

// Message is the frontend's normalized transcript message shape.
type Interaction struct {
	ID string `json:"id"`
	// Kind is "approval" for a yes or no on one call; absent means an editable draft.
	Kind         string          `json:"kind,omitempty"`
	Tool         string          `json:"tool"`
	ToolCallID   string          `json:"tool_call_id"`
	Draft        json.RawMessage `json:"draft"`
	Title        string          `json:"title,omitempty"`
	CreatedAt    string          `json:"created_at"`
	ExpiresAt    string          `json:"expires_at"`
	MaxExpiresAt string          `json:"max_expires_at"`
	ExtendMS     int64           `json:"extend_ms"`
}

type Message struct {
	ID               string       `json:"id"`
	Sender           string       `json:"sender"`
	Text             string       `json:"text"`
	Timestamp        string       `json:"timestamp"`
	Parts            []Part       `json:"parts"`
	Steps            []Step       `json:"steps,omitempty"`
	Status           string       `json:"status,omitempty"`
	ReasoningContent string       `json:"reasoningContent,omitempty"`
	Cursor           *int64       `json:"cursor,omitempty"`
	Pending          *Interaction `json:"pending,omitempty"`
}

// TokenUsage contains token counts reported by one or more Responses.
type TokenUsage struct {
	InputTokens       *int64 `json:"input_tokens,omitempty"`
	OutputTokens      *int64 `json:"output_tokens,omitempty"`
	TotalTokens       *int64 `json:"total_tokens,omitempty"`
	CachedInputTokens *int64 `json:"cached_input_tokens,omitempty"`
}

// CurrentResponseUsage identifies the most recent completed response.
type CurrentResponseUsage struct {
	TokenUsage
	Turn       int64  `json:"turn"`
	Step       int64  `json:"step"`
	ResponseID string `json:"response_id,omitempty"`
}

// UsageSummary mirrors the frontend's session usage contract.
type UsageSummary struct {
	TokenUsage
	CompletedResponses int64                 `json:"completed_responses"`
	ResponsesWithUsage int64                 `json:"responses_with_usage"`
	TurnCount          int64                 `json:"turn_count"`
	HistoryPartial     bool                  `json:"history_partial"`
	Current            *CurrentResponseUsage `json:"current,omitempty"`
}

// TranscriptRecord supplies a stable byte offset and origin for message IDs.
type TranscriptRecord struct {
	Offset    int64
	Prefix    string
	Raw       json.RawMessage
	Cursor    int64
	HasCursor bool
}

// StreamEvent is one normalized incremental SSE event.
type StreamEvent struct {
	Name string
	Data any
}

// CapMessages truncates oversized parts at valid UTF-8 boundaries. Cursors map
// message IDs to their transcript turn so clients can fetch the whole message.
func CapMessages(messages []Message, maxInlineBytes int, cursors map[string]int64) []Message {
	cappedMessages := append([]Message(nil), messages...)
	for messageIndex := range cappedMessages {
		message := cappedMessages[messageIndex]
		transcriptCursor := message.Cursor
		message.Cursor = nil
		if maxInlineBytes <= 0 {
			cappedMessages[messageIndex] = message
			continue
		}
		parts := append([]Part(nil), message.Parts...)
		capped := false
		for partIndex := range parts {
			part := &parts[partIndex]
			var value string
			var set func(string)
			switch part.Type {
			case "text", "reasoning":
				value = part.Text
				set = func(text string) { part.Text = text }
			case "tool_call":
				if part.Arguments == nil {
					continue
				}
				value = *part.Arguments
				set = func(text string) { part.Arguments = &text }
			case "tool_result":
				value = part.Output
				set = func(text string) { part.Output = text }
			default:
				continue
			}
			fullBytes := len([]byte(value))
			if fullBytes <= maxInlineBytes {
				continue
			}
			cut := maxInlineBytes
			for cut > 0 && cut < len(value) && (value[cut]&0xc0) == 0x80 {
				cut--
			}
			set(value[:cut])
			part.Truncated = &PartTruncation{Bytes: fullBytes}
			capped = true
		}
		if capped {
			message.Parts = parts
			message.Text, message.ReasoningContent = FieldsFromParts(parts)
			if cursor, ok := cursors[message.ID]; ok {
				message.Cursor = &cursor
			} else if transcriptCursor != nil {
				message.Cursor = transcriptCursor
			}
		}
		cappedMessages[messageIndex] = message
	}
	return cappedMessages
}

// FieldsFromParts recomputes the message-level text summaries after part edits.
func FieldsFromParts(parts []Part) (text, reasoningContent string) {
	var textParts []string
	var reasoningParts []string
	for _, part := range parts {
		switch part.Type {
		case "text":
			textParts = append(textParts, part.Text)
		case "reasoning":
			reasoningParts = append(reasoningParts, part.Text)
		}
	}
	text = strings.Join(textParts, "")
	reasoningContent = strings.TrimSpace(strings.Join(reasoningParts, "\n\n"))
	return text, reasoningContent
}

// Increments compares two streaming snapshots and returns the smallest
// frontend-compatible updates, falling back to a complete ui.message event.
func Increments(previous, next Message) []StreamEvent {
	if previous.ID != next.ID || previous.Status != next.Status || !reflect.DeepEqual(previous.Steps, next.Steps) || len(next.Parts) < len(previous.Parts) {
		return []StreamEvent{{Name: "ui.message", Data: map[string]any{"message": next}}}
	}
	changed := 0
	for changed < len(previous.Parts) && reflect.DeepEqual(previous.Parts[changed], next.Parts[changed]) {
		changed++
	}
	if changed == len(previous.Parts) && changed == len(next.Parts) {
		return nil
	}
	events := make([]StreamEvent, 0, len(next.Parts)-changed+1)
	index := changed
	if index < len(previous.Parts) {
		if appendText, ok := appendedText(previous.Parts[index], next.Parts[index]); ok {
			events = append(events, StreamEvent{Name: "ui.delta", Data: map[string]any{"id": next.ID, "index": index, "append": appendText}})
		} else {
			events = append(events, StreamEvent{Name: "ui.part", Data: map[string]any{"id": next.ID, "index": index, "part": next.Parts[index]}})
		}
		index++
	}
	for ; index < len(next.Parts); index++ {
		events = append(events, StreamEvent{Name: "ui.part", Data: map[string]any{"id": next.ID, "index": index, "part": next.Parts[index]}})
	}
	return events
}

func appendedText(previous, next Part) (string, bool) {
	if previous.Type != next.Type || previous.Truncated != nil || next.Truncated != nil {
		return "", false
	}
	var before, after string
	switch previous.Type {
	case "text", "reasoning":
		before, after = previous.Text, next.Text
	case "tool_call":
		if previous.CallID != next.CallID || previous.Name != next.Name || previous.Partial != next.Partial || previous.Arguments == nil || next.Arguments == nil {
			return "", false
		}
		before, after = *previous.Arguments, *next.Arguments
	default:
		return "", false
	}
	if !strings.HasPrefix(after, before) || len(after) == len(before) {
		return "", false
	}
	appendText := after[len(before):]
	if !utf8.ValidString(appendText) {
		return "", false
	}
	return appendText, true
}
