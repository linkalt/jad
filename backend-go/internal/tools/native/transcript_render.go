package native

import (
	"encoding/json"
	"regexp"
	"strings"
)

var transcriptDataURL = regexp.MustCompile(`data:[A-Za-z0-9.+/-]+;base64,[A-Za-z0-9+/=]{64,}`)

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
