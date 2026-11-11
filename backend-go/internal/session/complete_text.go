package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"jad/backend-go/internal/responses"
)

// TextMessage is one message of a one-shot completion.
type TextMessage struct {
	// Role is user or assistant.
	Role string
	Text string
}

// CompleteText asks the model one question outside any run and returns its text: system
// in place of the session's prompt, the messages as the input, no tools, and
// max_output_tokens set when it is above 0. timeout bounds the request when above 0. An
// MCP server's sampling request is answered with it.
func CompleteText(ctx context.Context, streamer responses.Streamer, model ModelSettings, system string, messages []TextMessage, maxOutputTokens int64, timeout time.Duration) (string, error) {
	if streamer == nil {
		return "", errors.New("no model client")
	}
	if len(messages) == 0 {
		return "", errors.New("a completion needs a message")
	}
	fields := make(map[string]json.RawMessage)
	if len(model.Request) > 0 {
		if err := json.Unmarshal(model.Request, &fields); err != nil || fields == nil {
			return "", errors.New("model request settings must be a JSON object")
		}
	}
	delete(fields, "tool_choice")
	delete(fields, "parallel_tool_calls")
	delete(fields, "tools")
	if maxOutputTokens > 0 {
		fields["max_output_tokens"] = json.RawMessage(fmt.Sprint(maxOutputTokens))
	}
	request, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	items := make([][]byte, 0, len(messages))
	for _, message := range messages {
		var item []byte
		switch message.Role {
		case "user":
			item, err = json.Marshal(promptMessageItem{Type: "message", Role: "user", Content: []inputTextContent{{Type: "input_text", Text: message.Text}}})
		case "assistant":
			item, err = json.Marshal(struct {
				Type    string             `json:"type"`
				Role    string             `json:"role"`
				Content []inputTextContent `json:"content"`
			}{"message", "assistant", []inputTextContent{{Type: "output_text", Text: message.Text}}})
		default:
			return "", fmt.Errorf("message role %q is not user or assistant", message.Role)
		}
		if err != nil {
			return "", err
		}
		items = append(items, item)
	}
	role := model.PromptRole
	if role == "" {
		role = "developer"
	}
	body, err := BuildRequest(ModelSettings{Model: model.Model, Request: request, PromptRole: role, PromptText: system}, items)
	if err != nil {
		return "", err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var text string
	var terminal error
	completed := false
	err = streamer.Stream(ctx, body, func(event responses.Event) error {
		switch event.Type {
		case "response.completed", "response.incomplete":
			completed = true
			text = responseOutputText(event.Raw)
		case "response.failed", "error":
			terminal = terminalFailureError(event.Raw, event.Type)
		}
		return nil
	})
	switch {
	case err != nil:
		return "", err
	case terminal != nil:
		return "", terminal
	case !completed:
		return "", errors.New("the response ended without response.completed")
	}
	return strings.TrimSpace(text), nil
}

// promptMessageItem is a message input item with text content.
type promptMessageItem struct {
	Type    string             `json:"type"`
	Role    string             `json:"role"`
	Content []inputTextContent `json:"content"`
}

// responseOutputText joins the output_text parts of the assistant messages of a terminal
// response event.
func responseOutputText(raw json.RawMessage) string {
	var event struct {
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
	if json.Unmarshal(raw, &event) != nil {
		return ""
	}
	var text strings.Builder
	for _, item := range event.Response.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" {
				text.WriteString(part.Text)
			}
		}
	}
	return text.String()
}
