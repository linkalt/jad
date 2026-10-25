package native

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var transcriptDataURL = regexp.MustCompile(`data:[A-Za-z0-9.+/-]+;base64,[A-Za-z0-9+/=]{64,}`)

// cutMiddle keeps the head (70%) and the tail (30%) of text longer than limit bytes, on
// character boundaries, and says how much it left out.
func cutMiddle(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	head := limit * 7 / 10
	tail := limit - head
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	start := len(text) - tail
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return fmt.Sprintf("%s [... truncated %d bytes ...] %s", text[:head], start-head, text[start:])
}

type transcriptRecord struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	Response  *struct {
		Output []json.RawMessage `json:"output"`
	} `json:"response"`
}

// transcriptText is the text of a message content or a tool output: a string, or the text of
// each part, with images shown as a marker.
func transcriptText(raw json.RawMessage) string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		pieces := make([]string, 0, len(parts))
		for _, part := range parts {
			if strings.Contains(part.Type, "image") {
				pieces = append(pieces, "[image]")
			} else if part.Text != "" {
				pieces = append(pieces, part.Text)
			}
		}
		return strings.Join(pieces, "\n")
	}
	return string(raw)
}

// renderTranscriptRecord turns one transcript line into entries, one per message, tool call
// or tool result. Reasoning is not shown. limit cuts each payload; zero keeps it whole.
func renderTranscriptRecord(data []byte, turn int64, limit int) []string {
	var record transcriptRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return []string{fmt.Sprintf("#%d [unreadable record]", turn)}
	}
	entry := func(label, body string) string {
		body = transcriptDataURL.ReplaceAllStringFunc(body, func(match string) string {
			return fmt.Sprintf("[data URL %d bytes]", len(match))
		})
		return fmt.Sprintf("#%d %s%s", turn, label, cutMiddle(body, limit))
	}
	switch {
	case record.Type == "function_call_output":
		return []string{entry("result ["+record.CallID+"] ", transcriptText(record.Output))}
	case record.Type == "response.completed" && record.Response != nil:
		var entries []string
		for _, raw := range record.Response.Output {
			var item transcriptRecord
			if json.Unmarshal(raw, &item) != nil {
				continue
			}
			switch item.Type {
			case "message":
				entries = append(entries, entry("assistant: ", transcriptText(item.Content)))
			case "function_call":
				entries = append(entries, entry("call "+item.Name+" ["+item.CallID+"] ", transcriptText(item.Arguments)))
			case "reasoning":
			default:
				entries = append(entries, fmt.Sprintf("#%d [%s]", turn, item.Type))
			}
		}
		return entries
	case record.Type == "message" || record.Role != "":
		role := record.Role
		if role == "" {
			role = "message"
		}
		return []string{entry(role+": ", transcriptText(record.Content))}
	}
	return []string{fmt.Sprintf("#%d [%s]", turn, record.Type)}
}
