package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/transcript"
)

// CompactionSummarySettings are the normalized compaction.summary settings: before a
// compaction drops turns, the session's model writes a summary of them, which is kept in
// front of the retained turns. A nil *CompactionSummarySettings means off.
type CompactionSummarySettings struct {
	// Prompt is the instruction sent to the model in place of the session's prompt.
	Prompt string
	// Header starts the stored summary, so the model (and the next summary) can tell
	// what the item is.
	Header string
	// Role is the role of the stored summary message: developer or user.
	Role string
	// MaxInputBytes bounds the rendered transcript sent to the model; the newest lines
	// are kept (and an earlier summary, which comes first).
	MaxInputBytes int
	// MaxItemBytes bounds one rendered item (message, call or result).
	MaxItemBytes int
	// MaxOutputTokens, when above zero, replaces the model's max_output_tokens for the
	// summary request.
	MaxOutputTokens int64
	// TimeoutMS bounds the summary request.
	TimeoutMS int64
	// OnFailure is "compact" (drop the turns without a summary) or "skip" (do not compact).
	OnFailure string
}

const (
	CompactionSummaryOnFailureCompact = "compact"
	CompactionSummaryOnFailureSkip    = "skip"
)

// NewCompactionSummarizer returns a transcript.CompactionOptions.Summarize callback that
// asks the model behind streamer for a summary of the dropped items. onError, when not
// nil, is told about a summary that failed (the compaction then goes on without one, or
// is skipped, by settings.OnFailure). The request goes through ctx, so the run's cost
// meter counts it.
func NewCompactionSummarizer(ctx context.Context, streamer responses.Streamer, model ModelSettings, settings CompactionSummarySettings, onError func(error)) func([][]byte) (json.RawMessage, error) {
	return func(dropped [][]byte) (json.RawMessage, error) {
		item, err := summarizeItems(ctx, streamer, model, settings, dropped)
		if err == nil {
			return item, nil
		}
		if onError != nil {
			onError(err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if settings.OnFailure == CompactionSummaryOnFailureSkip {
			return nil, transcript.ErrCompactionSkipped
		}
		return nil, nil
	}
}

func summarizeItems(ctx context.Context, streamer responses.Streamer, model ModelSettings, settings CompactionSummarySettings, dropped [][]byte) (json.RawMessage, error) {
	if streamer == nil {
		return nil, errors.New("no model client for the compaction summary")
	}
	rendered := RenderItemsForSummary(dropped, settings.Header, settings.MaxItemBytes, settings.MaxInputBytes)
	if strings.TrimSpace(rendered) == "" {
		return nil, nil
	}
	request, err := summaryRequest(model, settings, rendered)
	if err != nil {
		return nil, err
	}
	if settings.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(settings.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	var text string
	var terminal error
	completed := false
	err = streamer.Stream(ctx, request, func(event responses.Event) error {
		switch event.Type {
		case "response.completed", "response.incomplete":
			completed = true
			text = responseOutputText(event.Raw)
		case "response.failed", "error":
			terminal = terminalFailureError(event.Raw, event.Type)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if terminal != nil {
		return nil, terminal
	}
	if !completed {
		return nil, errors.New("the summary response ended without response.completed")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("the model returned an empty summary")
	}
	return json.Marshal(promptMessageItem{
		Type:    "message",
		Role:    settings.Role,
		Content: []inputTextContent{{Type: "input_text", Text: settings.Header + "\n\n" + text}},
	})
}

type promptMessageItem struct {
	Type    string             `json:"type"`
	Role    string             `json:"role"`
	Content []inputTextContent `json:"content"`
}

// summaryRequest builds the request: the summary prompt in place of the session's prompt,
// the rendered transcript as the one user message, no tools and no output format.
func summaryRequest(model ModelSettings, settings CompactionSummarySettings, rendered string) (json.RawMessage, error) {
	fields := make(map[string]json.RawMessage)
	if len(model.Request) > 0 {
		if err := json.Unmarshal(model.Request, &fields); err != nil || fields == nil {
			return nil, errors.New("model request settings must be a JSON object")
		}
	}
	// A tool choice without tools is refused by the provider.
	delete(fields, "tool_choice")
	delete(fields, "parallel_tool_calls")
	delete(fields, "tools")
	if settings.MaxOutputTokens > 0 {
		fields["max_output_tokens"] = json.RawMessage(fmt.Sprint(settings.MaxOutputTokens))
	}
	request, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	user, err := json.Marshal(promptMessageItem{
		Type:    "message",
		Role:    "user",
		Content: []inputTextContent{{Type: "input_text", Text: rendered}},
	})
	if err != nil {
		return nil, err
	}
	return BuildRequest(ModelSettings{
		Model:      model.Model,
		Request:    request,
		PromptRole: model.PromptRole,
		PromptText: settings.Prompt,
	}, [][]byte{user})
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

// RenderItemsForSummary writes history items as plain text, one block per item: messages
// with their role, function calls with their name and arguments, results with their call
// id. Reasoning is left out. Each block is cut to maxItemBytes. When the whole is over
// maxInputBytes, the oldest blocks are left out, except an earlier summary (a message
// that starts with header), which is kept first.
func RenderItemsForSummary(items [][]byte, header string, maxItemBytes, maxInputBytes int) string {
	blocks := make([]string, 0, len(items))
	earlier := ""
	for _, item := range items {
		block, isSummary := renderSummaryItem(item, header)
		if block == "" {
			continue
		}
		if isSummary {
			earlier = cutText(block, maxInputBytes/2)
			continue
		}
		blocks = append(blocks, cutText(block, maxItemBytes))
	}
	budget := maxInputBytes
	if budget <= 0 {
		budget = int(^uint(0) >> 1)
	}
	budget -= len(earlier)
	start := len(blocks)
	for start > 0 && budget-len(blocks[start-1])-2 >= 0 {
		budget -= len(blocks[start-1]) + 2
		start--
	}
	var out strings.Builder
	if earlier != "" {
		out.WriteString(earlier)
		out.WriteString("\n\n")
	}
	if start > 0 {
		fmt.Fprintf(&out, "[%d older items left out]\n\n", start)
	}
	out.WriteString(strings.Join(blocks[start:], "\n\n"))
	return strings.TrimSpace(out.String())
}

func renderSummaryItem(raw []byte, header string) (string, bool) {
	var item struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Input     string          `json:"input"`
		CallID    string          `json:"call_id"`
		Output    json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return "", false
	}
	switch {
	case item.Type == "reasoning":
		return "", false
	case item.Type == "message" || (item.Type == "" && item.Role != ""):
		text := contentText(item.Content)
		if text == "" {
			return "", false
		}
		if header != "" && strings.HasPrefix(text, header) {
			return "Earlier summary:\n" + strings.TrimSpace(strings.TrimPrefix(text, header)), true
		}
		return item.Role + ":\n" + text, false
	case item.Type == "function_call":
		return fmt.Sprintf("tool call %s (%s):\n%s", item.Name, item.CallID, item.Arguments), false
	case item.Type == "custom_tool_call":
		return fmt.Sprintf("tool call %s (%s):\n%s", item.Name, item.CallID, item.Input), false
	case item.Type == "function_call_output" || item.Type == "custom_tool_call_output":
		return fmt.Sprintf("tool result (%s):\n%s", item.CallID, contentText(item.Output)), false
	default:
		return "[" + item.Type + "]", false
	}
}

// contentText returns a string content as it is, or the text parts of a content list
// (images and files become a marker).
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
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
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	pieces := make([]string, 0, len(parts))
	for _, part := range parts {
		switch {
		case part.Text != "":
			pieces = append(pieces, part.Text)
		case part.Type != "":
			pieces = append(pieces, "["+part.Type+"]")
		}
	}
	return strings.Join(pieces, "\n")
}

// cutText keeps the start of text up to limit bytes, on a character boundary, and says
// how much was left out. A limit of zero or less keeps everything.
func cutText(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n[%d bytes cut]", len(text)-cut)
}
