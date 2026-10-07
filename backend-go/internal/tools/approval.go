package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
)

// Approval error codes. A call that is not approved becomes an ordinary blocked
// function_call_output carrying one of these, never a failed run.
const (
	ApprovalDenied      = "approval_denied"
	ApprovalTimeout     = "approval_timeout"
	ApprovalCancelled   = "approval_cancelled"
	ApprovalUnavailable = "approval_unavailable"
)

// ApprovalCall is one validated call that waits for the user's decision.
type ApprovalCall struct {
	CallID string
	// ModelToolName is the name the model used, which the question is filed under.
	ModelToolName string
	// Tool is the tool's own name; for an MCP tool it is the remote name.
	Tool string
	// Server is the MCP server the call goes to, empty for a native tool.
	Server string
	Args   map[string]any
	// RawArguments is the provider's argument text, kept for the preview of an
	// MCP call because it preserves the model's key order.
	RawArguments json.RawMessage
}

// ApprovalVerdict is the outcome of asking: run the call, or not and why.
type ApprovalVerdict struct {
	Allowed bool
	Code    string
	Reason  string
}

// ApprovalRequester decides whether a call to a tool whose approval is "ask" may run.
type ApprovalRequester func(context.Context, ApprovalCall) ApprovalVerdict

// ApprovalOptions are the session-wide values shared by every tool whose approval is "ask".
type ApprovalOptions struct {
	TimeoutMS    int64
	ExtendMS     int64
	MaxTimeoutMS int64
	// HeadlessAllow lets an unattended run proceed; otherwise a call nobody can answer is blocked.
	HeadlessAllow bool
	// PreviewBytes bounds each text field shown in the question.
	PreviewBytes int64
}

// NewApprovalRequester turns one call into a question for the user and the
// answer into a verdict. ask is nil when no client can answer.
//
// Questions are asked one at a time: the broker holds a single pending
// question, and two calls of one response may both need a decision. The
// requester is meant to live for one tool round, so its mutex is that round's
// queue. Calls that need no decision never reach it and are not held up.
func NewApprovalRequester(options ApprovalOptions, ask InteractionHandler) ApprovalRequester {
	var gate sync.Mutex
	return func(ctx context.Context, call ApprovalCall) ApprovalVerdict {
		if !CallNeedsApproval(call.Tool, call.Args) {
			return ApprovalVerdict{Allowed: true}
		}
		if ask == nil {
			if options.HeadlessAllow {
				return ApprovalVerdict{Allowed: true}
			}
			return ApprovalVerdict{
				Code:   ApprovalUnavailable,
				Reason: "This call needs the user's approval and no client is connected that can give it.",
			}
		}
		preview, err := ApprovalPreview(call, options.PreviewBytes)
		if err != nil {
			return ApprovalVerdict{Code: ApprovalUnavailable, Reason: "The call could not be shown to the user, so it was not run."}
		}
		gate.Lock()
		defer gate.Unlock()
		answer, err := ask(ctx, InteractionIdentity{Tool: call.ModelToolName, ToolCallID: call.CallID}, InteractionQuestion{
			Kind:         InteractionKindApproval,
			Draft:        preview.Draft,
			Title:        preview.Title,
			TimeoutMS:    options.TimeoutMS,
			ExtendMS:     options.ExtendMS,
			MaxTimeoutMS: options.MaxTimeoutMS,
		})
		if err != nil {
			return ApprovalVerdict{Code: ApprovalUnavailable, Reason: "The approval question could not be asked, so the call was not run."}
		}
		switch {
		case answer.Outcome == InteractionCancelled:
			return ApprovalVerdict{Code: ApprovalCancelled, Reason: "The run was stopped before the call was approved."}
		case answer.Outcome == InteractionTimedOut:
			return ApprovalVerdict{Code: ApprovalTimeout, Reason: "The user did not decide in time; the call was not run."}
		case answer.Decision == InteractionAccept:
			return ApprovalVerdict{Allowed: true}
		}
		reason := "The user declined this call."
		if answer.Reason != nil && *answer.Reason != "" {
			reason = *answer.Reason
		}
		return ApprovalVerdict{Code: ApprovalDenied, Reason: reason}
	}
}

// CallNeedsApproval reports whether approval applies to a call of toolName.
// For process only a start launches anything; status, wait, list and stop act
// on what the user already approved. `action` is inferred from `command` like
// the tool does.
func CallNeedsApproval(toolName string, args map[string]any) bool {
	if toolName != "process" {
		return true
	}
	if action, _ := args["action"].(string); strings.TrimSpace(action) != "" {
		return strings.TrimSpace(action) == "start"
	}
	command, _ := args["command"].(string)
	return strings.TrimSpace(command) != ""
}

// Preview is what the user is shown: a short title and the call's own fields.
type Preview struct {
	Title string
	Draft json.RawMessage
}

type previewField struct {
	key   string
	value any
}

// ApprovalPreview builds the question's content. Only fields that say what will
// happen are shown; long text is cut to maxBytes and says how long it really was.
// Keys keep the order the TypeScript backend writes them in.
func ApprovalPreview(call ApprovalCall, maxBytes int64) (Preview, error) {
	fields := []previewField{{"tool", call.Tool}}
	title := "Run " + call.Tool
	switch {
	case call.Server != "":
		fields = append(fields, previewField{"server", call.Server})
		fields = appendText(fields, "arguments", compactArguments(call), maxBytes)
		title = "Run " + call.Server + " tool " + call.Tool
	case call.Tool == "bash":
		fields = appendString(fields, call.Args, "command", maxBytes)
		// The text piped to the command is part of what runs (`python3 -` alone says nothing).
		fields = appendString(fields, call.Args, "stdin", maxBytes)
		fields = appendPresent(fields, call.Args, "cwd")
		fields = appendPresent(fields, call.Args, "timeout_ms")
		title = "Run shell command"
	case call.Tool == "exec":
		// The code is what runs; the language alone says nothing.
		fields = appendString(fields, call.Args, "lang", maxBytes)
		fields = appendString(fields, call.Args, "code", maxBytes)
		fields = appendPresent(fields, call.Args, "args")
		fields = appendPresent(fields, call.Args, "cwd")
		fields = appendPresent(fields, call.Args, "timeout_ms")
		title = "Run code"
	case call.Tool == "process":
		fields = append(fields, previewField{"action", "start"})
		fields = appendString(fields, call.Args, "command", maxBytes)
		// The text written to the process is part of what runs (`python3 -` alone says nothing).
		fields = appendString(fields, call.Args, "stdin", maxBytes)
		fields = appendPresent(fields, call.Args, "cwd")
		title = "Start background process"
	case call.Tool == "write":
		fields = appendString(fields, call.Args, "path", maxBytes)
		fields = appendPresent(fields, call.Args, "contentEncoding")
		if content, ok := call.Args["content"].(string); ok {
			fields = append(fields, previewField{"content_bytes", len(content)})
			if encoding, _ := call.Args["contentEncoding"].(string); encoding != "base64" {
				fields = appendText(fields, "content", content, maxBytes)
			}
		}
		title = "Write file"
	case call.Tool == "edit":
		fields = appendString(fields, call.Args, "path", maxBytes)
		fields = appendString(fields, call.Args, "oldText", maxBytes)
		fields = appendString(fields, call.Args, "newText", maxBytes)
		fields = appendPresent(fields, call.Args, "replaceAll")
		title = "Edit file"
	default:
		fields = appendText(fields, "arguments", compactArguments(call), maxBytes)
	}
	draft, err := encodeOrdered(fields)
	if err != nil {
		return Preview{}, err
	}
	return Preview{Title: title, Draft: draft}, nil
}

// compactArguments is the call's arguments as one line, in the model's key order.
func compactArguments(call ApprovalCall) string {
	if len(call.RawArguments) > 0 {
		var compact bytes.Buffer
		if json.Compact(&compact, call.RawArguments) == nil {
			return compact.String()
		}
	}
	encoded, err := json.Marshal(call.Args)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func appendString(fields []previewField, args map[string]any, name string, maxBytes int64) []previewField {
	value, ok := args[name].(string)
	if !ok {
		return fields
	}
	return appendText(fields, name, value, maxBytes)
}

// appendText adds text cut to maxBytes, plus `<name>_truncated_bytes` when it was cut.
func appendText(fields []previewField, name, text string, maxBytes int64) []previewField {
	cut, truncated := CutText(text, maxBytes)
	fields = append(fields, previewField{name, cut})
	if truncated {
		fields = append(fields, previewField{name + "_truncated_bytes", len(text)})
	}
	return fields
}

func appendPresent(fields []previewField, args map[string]any, name string) []previewField {
	if value, exists := args[name]; exists {
		return append(fields, previewField{name, value})
	}
	return fields
}

// CutText cuts text to at most maxBytes of UTF-8 without leaving half a character.
func CutText(text string, maxBytes int64) (string, bool) {
	if int64(len(text)) <= maxBytes {
		return text, false
	}
	end := int(maxBytes)
	// A UTF-8 continuation byte starts with 0b10; stepping back to a lead byte drops a split character.
	for end > 0 && text[end]&0xc0 == 0x80 {
		end--
	}
	return text[:end], true
}

// encodeOrdered writes the fields as one JSON object in the given order, without
// escaping <, > and & the way encoding/json does by default.
func encodeOrdered(fields []previewField) (json.RawMessage, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	for index, field := range fields {
		if index > 0 {
			out.WriteByte(',')
		}
		if err := writeJSONValue(&out, field.key); err != nil {
			return nil, err
		}
		out.WriteByte(':')
		if err := writeJSONValue(&out, field.value); err != nil {
			return nil, err
		}
	}
	out.WriteByte('}')
	return json.RawMessage(out.Bytes()), nil
}

func writeJSONValue(out *bytes.Buffer, value any) error {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	out.Write(bytes.TrimRight(encoded.Bytes(), "\n"))
	return nil
}
