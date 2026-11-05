package session

import (
	"encoding/json"
	"fmt"
	"strings"
)

// storeStopMessage appends the message a stop hook asked the run to go on with. It is
// stored as steering input (transcript.SessionInputSteerField), like a steering message: it
// continues the run's turn, so compaction keeps it with the run's earlier rounds instead of
// starting a turn at it.
func (r *Runner) storeStopMessage(message string, options RunOptions) error {
	role := options.StopMessageRole
	if role == "" {
		role = "user"
	}
	type textPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	item, err := json.Marshal(struct {
		Type    string     `json:"type"`
		Role    string     `json:"role"`
		Content []textPart `json:"content"`
	}{"message", role, []textPart{{"input_text", message}}})
	if err != nil {
		return fmt.Errorf("encode the stop hook's message: %w", err)
	}
	if _, err := r.store.AppendSteerInputItemsAt(item); err != nil {
		return fmt.Errorf("persist the stop hook's message: %w", err)
	}
	return nil
}

// finalAnswerText is the output_text of the assistant messages of a completed response.
func finalAnswerText(event json.RawMessage) string {
	var envelope struct {
		Response struct {
			Output []struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(event, &envelope) != nil {
		return ""
	}
	var parts []string
	for _, item := range envelope.Response.Output {
		if item.Type != "message" || item.Role != "assistant" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" && part.Text != "" {
				parts = append(parts, part.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}
