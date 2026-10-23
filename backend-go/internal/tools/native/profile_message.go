package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const profileMessageToolID = "native.profile_message"

var profileMessageInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "profile":{"type":"string","minLength":1,"description":"Profile to message; one of the available profiles."},
    "message":{"type":"string","minLength":1,"description":"The message to send. The profile sees nothing of this conversation."}
  },
  "required":["profile","message"],
  "additionalProperties":false
}`)

var profileMessageExample = json.RawMessage(`{"profile":"reviewer","message":"Check the change in internal/app/resolver.go and list what is wrong."}`)

// ErrProfileBusy is returned by a ProfileMessenger when the target profile is running.
var ErrProfileBusy = errors.New("profile is busy")

// ProfileBusyError is the busy error with what holds the target. HeldBy is the profile
// whose profile_message call runs on the target, empty when something else holds it.
type ProfileBusyError struct{ HeldBy string }

func (e *ProfileBusyError) Error() string { return ErrProfileBusy.Error() }

func (e *ProfileBusyError) Is(target error) bool { return target == ErrProfileBusy }

// ProfileFailedError is a failed target run with the client-safe message the run
// reported (provider status, token limit); it is not an internal error text.
type ProfileFailedError struct{ Message string }

func (e *ProfileFailedError) Error() string { return "profile run failed" }

// ErrProfileStopped is returned by a ProfileMessenger when the target run was stopped
// by the user while the caller was still waiting.
var ErrProfileStopped = errors.New("profile run was stopped")

// ErrProfileSameTranscript is returned by a ProfileMessenger when the target profile
// shares the calling profile's transcript, so the call can never succeed.
var ErrProfileSameTranscript = errors.New("profile shares the calling transcript")

// ErrProfileNoAnswer is returned by a ProfileMessenger when the target run finished
// without any answer text.
var ErrProfileNoAnswer = errors.New("profile finished without an answer")

// ErrProfileCycle is returned by a ProfileMessenger when the message would reach a profile
// already in the chain of messages that led to the caller, or the caller itself.
var ErrProfileCycle = errors.New("profile message would form a cycle")

// ErrProfileChainTooLong is returned by a ProfileMessenger when the message would make the
// chain of profiles longer than tools.native.profile_message.max_chain.
var ErrProfileChainTooLong = errors.New("profile message chain is too long")

type profileChainMaxKey struct{}

type childCallKey struct{}

// IsChildCall reports whether the profile_message call in ctx comes from a sub-agent run.
// Such a call has no transcript of its own: the messenger takes the calling profile's.
func IsChildCall(ctx context.Context) bool {
	child, _ := ctx.Value(childCallKey{}).(bool)
	return child
}

// WithProfileChainMax sets the longest chain the call in ctx may build.
func WithProfileChainMax(ctx context.Context, limit int) context.Context {
	return context.WithValue(ctx, profileChainMaxKey{}, limit)
}

// WithChildCall marks ctx as the context of a call made by a sub-agent run.
func WithChildCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, childCallKey{}, true)
}

// ProfileChainMax is the longest chain the call in ctx may build; zero when the caller did
// not say, and the messenger then uses the default.
func ProfileChainMax(ctx context.Context) int {
	limit, _ := ctx.Value(profileChainMaxKey{}).(int)
	return limit
}

// ProfileMessenger delivers one message to another profile and returns its final
// answer. The app package implements it, because it owns the sessions. ownerKey is the
// calling session's ExecutionContext.SessionKey. ctx ends when the caller stops or the
// tool's timeout passes; the messenger must stop promptly then. compact asks for the
// target's transcript to be compacted before the run, once per caller run and target.
type ProfileMessenger interface {
	Message(ctx context.Context, ownerKey string, target string, message string, busyWait time.Duration, compact bool) (string, error)
}

type profileMessageTool struct {
	settings  sessionfs.NativeProfileMessageSettings
	targets   []string
	messenger ProfileMessenger
}

// NewProfileMessageTool builds the profile_message tool over a messenger. targets are the
// profiles the model may message.
func NewProfileMessageTool(settings sessionfs.NativeProfileMessageSettings, targets []string, messenger ProfileMessenger) (tools.Tool, error) {
	if messenger == nil {
		return tools.Tool{}, errors.New("tools.native.profile_message needs a messenger")
	}
	if len(targets) == 0 {
		return tools.Tool{}, sessionfs.ErrProfileMessageNoTargets
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
	runtime := &profileMessageTool{settings: settings, targets: targets, messenger: messenger}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          profileMessageToolID,
			Source:      "native",
			Name:        "profile_message",
			Description: "Send a message to another profile and get its final answer." + profileMessageListText(targets),
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

func profileMessageListText(targets []string) string {
	return " Profiles: " + strings.Join(targets, ", ") + "."
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
	if !slices.Contains(p.targets, profile) {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "unknown_profile",
			fmt.Sprintf("Profile %s is not available to message", profile),
			tools.Information{Key: "available", Value: strings.Join(p.targets, ", ")}), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(p.settings.TimeoutMs)*time.Millisecond)
	defer cancel()
	callCtx = WithProfileChainMax(callCtx, p.settings.MaxChain)
	if execution.AgentDepth > 0 {
		callCtx = WithChildCall(callCtx)
	}
	reply, err := p.messenger.Message(callCtx, execution.SessionKey, profile, message, time.Duration(p.settings.BusyWaitMs)*time.Millisecond, p.settings.Compact == sessionfs.ProfileMessageCompactOn)
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
			return tools.ExecutionError(callID, tools.StatusError, "profile_busy", profileBusyText(profile, err, p.settings.BusyWaitMs)), nil
		case errors.Is(err, ErrProfileStopped):
			return tools.ExecutionError(callID, tools.StatusError, "profile_stopped",
				fmt.Sprintf("Profile %s was stopped", profile)), nil
		case errors.Is(err, ErrProfileNoAnswer):
			return tools.ExecutionError(callID, tools.StatusError, "profile_no_answer",
				fmt.Sprintf("Profile %s finished without an answer", profile)), nil
		case errors.Is(err, ErrProfileChainTooLong):
			return tools.ExecutionError(callID, tools.StatusError, "profile_chain_too_long",
				fmt.Sprintf("Profile %s cannot be messaged from here: the chain of profile messages would pass tools.native.profile_message.max_chain (%d)", profile, p.settings.MaxChain)), nil
		case errors.Is(err, ErrProfileCycle):
			return tools.ExecutionError(callID, tools.StatusError, "profile_cycle",
				fmt.Sprintf("Profile %s cannot be messaged from here: it would form a cycle", profile)), nil
		case callCtx.Err() != nil:
			return tools.ExecutionError(callID, tools.StatusError, "profile_timeout",
				fmt.Sprintf("Profile %s did not answer within %d ms", profile, p.settings.TimeoutMs)), nil
		}
		text := fmt.Sprintf("Profile %s failed", profile)
		var failed *ProfileFailedError
		if errors.As(err, &failed) && failed.Message != "" {
			text += ": " + failed.Message
		}
		return tools.ExecutionError(callID, tools.StatusError, "profile_failed", text), nil
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

func profileBusyText(profile string, err error, waitedMs int) string {
	text := fmt.Sprintf("Profile %s is busy", profile)
	var busy *ProfileBusyError
	if errors.As(err, &busy) && busy.HeldBy != "" {
		text += " with a message from profile " + busy.HeldBy
	} else {
		text += " with a user run or a maintenance action"
	}
	if waitedMs > 0 {
		text += fmt.Sprintf("; it was still busy after waiting %d ms", waitedMs)
	}
	return text + "; try again later"
}
