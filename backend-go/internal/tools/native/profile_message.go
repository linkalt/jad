package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const profileMessageToolID = "native.profile_message"

var profileMessageInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "profile":{"type":"string","minLength":1,"description":"Profile to message; one of the configured profiles."},
    "message":{"type":"string","minLength":1,"description":"The message to send. The profile sees nothing of this conversation."}
  },
  "required":["profile","message"],
  "additionalProperties":false
}`)

var profileMessageExample = json.RawMessage(`{"profile":"reviewer","message":"Check the change in internal/app/resolver.go and list what is wrong."}`)

// ErrProfileBusy is returned by a ProfileMessenger when the target profile is running.
var ErrProfileBusy = errors.New("profile is busy")

// ErrProfileStopped is returned by a ProfileMessenger when the target run was stopped
// by the user while the caller was still waiting.
var ErrProfileStopped = errors.New("profile run was stopped")

// ErrProfileSameTranscript is returned by a ProfileMessenger when the target profile
// shares the calling profile's transcript, so the call can never succeed.
var ErrProfileSameTranscript = errors.New("profile shares the calling transcript")

// ErrProfileNoAnswer is returned by a ProfileMessenger when the target run finished
// without any answer text.
var ErrProfileNoAnswer = errors.New("profile finished without an answer")

// ErrProfileCycle is returned by a ProfileMessenger when the message would reach the
// calling profile itself, close a cycle, or nest inside another profile message.
var ErrProfileCycle = errors.New("profile message would form a cycle")

// ProfileMessenger delivers one message to another profile and returns its final
// answer. The app package implements it, because it owns the sessions. ownerKey is the
// calling session's ExecutionContext.SessionKey. ctx ends when the caller stops or the
// tool's timeout passes; the messenger must stop promptly then.
type ProfileMessenger interface {
	Message(ctx context.Context, ownerKey string, target string, message string) (string, error)
}

type profileMessageTool struct {
	settings  sessionfs.NativeProfileMessageSettings
	messenger ProfileMessenger
}

// NewProfileMessageTool builds the profile_message tool over a messenger.
func NewProfileMessageTool(settings sessionfs.NativeProfileMessageSettings, messenger ProfileMessenger) (tools.Tool, error) {
	if messenger == nil {
		return tools.Tool{}, errors.New("tools.native.profile_message needs a messenger")
	}
	if len(settings.Profiles) == 0 {
		return tools.Tool{}, errors.New("tools.native.profile_message.profiles must not be empty")
	}
	if settings.TimeoutMs < 1 {
		return tools.Tool{}, errors.New("tools.native.profile_message.timeout_ms must be at least 1")
	}
	if settings.ResultMaxBytes < 1 {
		return tools.Tool{}, errors.New("tools.native.profile_message.result_max_bytes must be at least 1")
	}
	validator, err := tools.CompileSchema(profileMessageInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native profile_message schema: %w", err)
	}
	runtime := &profileMessageTool{settings: settings, messenger: messenger}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          profileMessageToolID,
			Source:      "native",
			Name:        "profile_message",
			Description: "Send a message to another profile and get its final answer." + profileMessageListText(settings),
			InputSchema: append(json.RawMessage(nil), profileMessageInputSchema...),
		},
		Validator:          validator,
		Approval:           approvalMode(settings.Approval),
		ExampleOperation:   profileMessageExample,
		MayChangeFiles:     func(map[string]any) bool { return true },
		NormalizeArguments: normalizeProfileMessageArguments,
		Execute:            runtime.execute,
	}, nil
}

func profileMessageListText(settings sessionfs.NativeProfileMessageSettings) string {
	return " Profiles: " + strings.Join(settings.Profiles, ", ") + "."
}

// normalizeProfileMessageArguments treats a blank profile or message as absent, so the
// schema reports it as missing, and trims the profile id.
func normalizeProfileMessageArguments(arguments map[string]any) map[string]any {
	for _, key := range []string{"profile", "message"} {
		value, ok := arguments[key].(string)
		if !ok {
			continue
		}
		if trimmed := strings.TrimSpace(value); trimmed == "" {
			delete(arguments, key)
		} else if key == "profile" {
			arguments[key] = trimmed
		}
	}
	return arguments
}

func (p *profileMessageTool) execute(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
	callID := execution.CallID
	profile, _ := arguments["profile"].(string)
	message, _ := arguments["message"].(string)
	known := false
	for _, candidate := range p.settings.Profiles {
		if candidate == profile {
			known = true
			break
		}
	}
	if !known {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "unknown_profile",
			fmt.Sprintf("Profile %s is not one of the configured profiles", profile),
			tools.Information{Key: "available", Value: strings.Join(p.settings.Profiles, ", ")}), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(p.settings.TimeoutMs)*time.Millisecond)
	defer cancel()
	reply, err := p.messenger.Message(callCtx, execution.SessionKey, profile, message)
	if err != nil {
		if ctx.Err() != nil {
			return tools.ExecutionError(callID, tools.StatusError, "cancelled",
				fmt.Sprintf("Message to profile %s was cancelled", profile)), nil
		}
		switch {
		case errors.Is(err, ErrProfileSameTranscript):
			return tools.ExecutionError(callID, tools.StatusError, "profile_same_transcript",
				fmt.Sprintf("Profile %s shares this conversation and cannot be called", profile)), nil
		case errors.Is(err, ErrProfileBusy):
			return tools.ExecutionError(callID, tools.StatusError, "profile_busy",
				fmt.Sprintf("Profile %s is running; try again later", profile)), nil
		case errors.Is(err, ErrProfileStopped):
			return tools.ExecutionError(callID, tools.StatusError, "profile_stopped",
				fmt.Sprintf("Profile %s was stopped", profile)), nil
		case errors.Is(err, ErrProfileNoAnswer):
			return tools.ExecutionError(callID, tools.StatusError, "profile_no_answer",
				fmt.Sprintf("Profile %s finished without an answer", profile)), nil
		case errors.Is(err, ErrProfileCycle):
			return tools.ExecutionError(callID, tools.StatusError, "profile_cycle",
				fmt.Sprintf("Profile %s cannot be messaged from here: it would form a cycle", profile)), nil
		case callCtx.Err() != nil:
			return tools.ExecutionError(callID, tools.StatusError, "profile_timeout",
				fmt.Sprintf("Profile %s did not answer within %d ms", profile, p.settings.TimeoutMs)), nil
		}
		return tools.ExecutionError(callID, tools.StatusError, "profile_failed",
			fmt.Sprintf("Profile %s failed", profile)), nil
	}
	text, cut := cutUTF8(strings.TrimSpace(reply), int64(p.settings.ResultMaxBytes))
	information := []tools.Information{{Key: "profile", Value: profile}}
	if cut {
		information = append(information, tools.Information{Key: "truncated", Value: true})
	}
	return tools.RenderToolResult(tools.ResultOptions{
		ID:          callID,
		Status:      tools.StatusOK,
		Summary:     fmt.Sprintf("Profile %s answered", profile),
		Information: information,
		Content:     &tools.Content{Encoding: "utf8", Text: text},
	})
}
