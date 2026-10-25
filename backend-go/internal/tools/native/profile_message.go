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
    "message":{"type":"string","minLength":1,"description":"The message to send. The profile sees nothing of this conversation."},
    "block_ms":{"type":"integer","minimum":0,"description":"Most time to block for the answer. If the profile has not answered (or is busy and the message waits its turn) an id is returned; collect it with the async tool."}
  },
  "required":["profile","message"],
  "additionalProperties":false
}`)

var profileMessageExample = json.RawMessage(`{"profile":"reviewer","message":"Check the change in internal/app/resolver.go and list what is wrong."}`)

// ErrProfileBusy is returned by a ProfileMessenger when the target profile is running.
var ErrProfileBusy = errors.New("profile is busy")

// ProfileBusyError is the busy error with what holds the target. HeldBy is the profile
// whose profile_message call runs on the target, empty when something else holds it.
type ProfileBusyError struct {
	HeldBy string
	// Waited is the queue wait that ended without the target getting free; QueueFull is
	// the queue cap that refused the message. At most one is set.
	Waited    time.Duration
	QueueFull int
}

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

// ErrProfileTimeout is the outcome of a message that did not finish within
// tools.native.profile_message.timeout_ms (queue time included).
var ErrProfileTimeout = errors.New("profile did not answer in time")

// ProfileMessageRequest is one message to another profile. OwnerKey is the calling
// session's ExecutionContext.SessionKey.
type ProfileMessageRequest struct {
	OwnerKey string
	Target   string
	Message  string
	// Compact asks for the target's transcript to be compacted before the run, once per
	// caller run and target.
	Compact bool
	// QueueWait is the longest the message may wait for a busy target; zero sets no limit
	// beyond Timeout. Timeout bounds the whole life of the message, queue time included.
	QueueWait time.Duration
	Timeout   time.Duration
	// MaxQueued is the most messages that may wait for one target; a message over it fails
	// at once as busy. Zero queues nobody.
	MaxQueued int
	// Blocking says the calling run cannot go on until the message ends (or the call's
	// block_ms passes), so a message that would wait for a profile that is itself blocked
	// on the caller is refused as a cycle.
	Blocking bool
}

// ProfileMessageOutcome is how a message ended. Text is the target's final answer, or the
// last text it wrote when it did not finish. Err is nil for an answer; otherwise one of the
// ErrProfile* values, a *ProfileFailedError or a *ProfileBusyError. Reason says who ended a
// stopped message. Trace points at the turn in the target's transcript.
type ProfileMessageOutcome struct {
	Text   string
	Err    error
	Reason string
	Trace  string
}

// ProfileMessageRun is a message in flight. It outlives the call that started it: only Stop
// (or the message's own end) ends it.
type ProfileMessageRun interface {
	// Queued reports that the message waited for a busy target when it started.
	Queued() bool
	// Started is closed when the target run begins; Done when the message is over.
	Started() <-chan struct{}
	Done() <-chan struct{}
	// Outcome is valid once Done is closed.
	Outcome() ProfileMessageOutcome
	// Status describes a message that is still going, for the async status action.
	Status() string
	// Stop ends the message: a queued one leaves the queue, a running one stops the
	// target run. It does not wait.
	Stop(reason string)
}

// ProfileMessageBlocker is implemented by a run that tracks who is blocked on it, so loops
// of blocked profiles can be refused. BeginWait is for a later call that blocks on the
// message (the async tool's wait); EndStartWait ends the wait the sending call held.
type ProfileMessageBlocker interface {
	BeginWait() (end func())
	EndStartWait()
}

// ProfileMessagePlan is a message that passed every check and has not started yet, so the
// caller can reserve a handle before any side effect.
type ProfileMessagePlan interface {
	// Start queues or starts the message. ctx bounds only the start itself, never the run.
	Start(ctx context.Context) (ProfileMessageRun, error)
}

// ProfileMessenger delivers one message to another profile. The app package implements it,
// because it owns the sessions. Prepare fails with the ErrProfile* errors when the message
// can never be delivered (cycle, chain, same transcript).
type ProfileMessenger interface {
	Prepare(ctx context.Context, request ProfileMessageRequest) (ProfileMessagePlan, error)
}

// ProfileMessageTranscripts is optionally implemented by a plan: the full paths of the target
// profile's messages.jsonl and of the caller's own, empty when not known.
type ProfileMessageTranscripts interface {
	Transcripts() (target, caller string)
}

// transcriptInformation is the transcript key (the target's file) and caller_transcript key
// of a plan that knows its files.
func transcriptInformation(plan ProfileMessagePlan) []tools.Information {
	known, ok := plan.(ProfileMessageTranscripts)
	if !ok {
		return nil
	}
	target, caller := known.Transcripts()
	var information []tools.Information
	if target != "" {
		information = append(information, tools.Information{Key: "transcript", Value: target})
	}
	if caller != "" {
		information = append(information, tools.Information{Key: "caller_transcript", Value: caller})
	}
	return information
}

type profileMessageTool struct {
	settings  sessionfs.NativeProfileMessageSettings
	targets   []string
	messenger ProfileMessenger
	host      *AsyncHost
}

// NewProfileMessageTool builds the profile_message tool over a messenger. targets are the
// profiles the model may message. A non-nil host lets a message that outlasts block_ms
// continue as a handle of the async tool.
func NewProfileMessageTool(settings sessionfs.NativeProfileMessageSettings, targets []string, messenger ProfileMessenger, host *AsyncHost) (tools.Tool, error) {
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
	if host != nil {
		host.attachProfileMessage()
	}
	runtime := &profileMessageTool{settings: settings, targets: targets, messenger: messenger, host: host}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          profileMessageToolID,
			Source:      "native",
			Name:        "profile_message",
			Description: profileMessageDescription + profileMessageListText(targets),
			InputSchema: append(json.RawMessage(nil), profileMessageInputSchema...),
		},
		Validator:          validator,
		ExampleOperation:   profileMessageExample,
		MayChangeFiles:     func(map[string]any) bool { return true },
		NormalizeArguments: normalizeProfileMessageArguments,
		Execute:            runtime.execute,
	}, nil
}

const profileMessageDescription = "Send a message to another profile and wait up to block_ms for its final answer. If the profile answers in time you get the answer; otherwise (it is still working, or busy and your message waits its turn) you get an id and the message goes on: use the async tool to wait for, list, inspect or stop it."

func profileMessageListText(targets []string) string {
	return " Profiles: " + strings.Join(targets, ", ") + "."
}

// normalizeProfileMessageArguments treats a blank profile or message as absent, so the
// schema reports it as missing, and trims the profile id.
func normalizeProfileMessageArguments(arguments map[string]any) map[string]any {
	if block, ok := safeInteger(arguments["block_ms"]); ok {
		copied := make(map[string]any, len(arguments))
		for key, value := range arguments {
			copied[key] = value
		}
		copied["block_ms"] = json.Number(fmt.Sprint(max(block, 0)))
		arguments = copied
	}
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
	owner, canHandle := handleOwner(execution)
	handled := p.host != nil && canHandle
	block := p.blockFor(arguments)
	request := ProfileMessageRequest{
		Blocking:  !handled || block > 0,
		OwnerKey:  execution.SessionKey,
		Target:    profile,
		Message:   message,
		Compact:   p.settings.Compact == sessionfs.ProfileMessageCompactOn,
		QueueWait: time.Duration(p.settings.BusyWaitMs) * time.Millisecond,
		Timeout:   time.Duration(p.settings.TimeoutMs) * time.Millisecond,
		MaxQueued: p.settings.MaxQueued,
	}
	if !handled && p.settings.BusyWaitMs == 0 {
		// Nobody can collect a queued message later, so a busy target fails at once unless
		// busy_wait_ms says how long this call may hold its run.
		request.MaxQueued = 0
	}
	callCtx := WithProfileChainMax(ctx, p.settings.MaxChain)
	if execution.AgentDepth > 0 {
		callCtx = WithChildCall(callCtx)
	}
	plan, err := p.messenger.Prepare(callCtx, request)
	if err != nil {
		return p.renderFailure(callID, profile, err, transcriptInformation(plan)...), nil
	}
	if !handled {
		return p.runInline(ctx, callID, profile, plan)
	}
	return p.runHandle(ctx, callID, profile, message, owner, plan, block)
}

func cancelledMessage(callID, profile string) string {
	return tools.ExecutionError(callID, tools.StatusError, "cancelled", fmt.Sprintf("Message to profile %s was cancelled", profile))
}

// runInline is the call of a session or child that cannot collect a handle later: it waits
// for the end of the message, and ending the call stops the target run.
func (p *profileMessageTool) runInline(ctx context.Context, callID, profile string, plan ProfileMessagePlan) (string, error) {
	run, err := plan.Start(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return cancelledMessage(callID, profile), nil
		}
		return p.renderFailure(callID, profile, err), nil
	}
	if blocker, ok := run.(ProfileMessageBlocker); ok {
		defer blocker.EndStartWait()
	}
	select {
	case <-run.Done():
	case <-ctx.Done():
		run.Stop("caller_cancelled")
		return cancelledMessage(callID, profile), nil
	}
	return p.renderOutcome(callID, profile, run.Outcome(), transcriptInformation(plan))
}

// renderOutcome renders how a message ended: the answer, or the named failure.
func (p *profileMessageTool) renderOutcome(callID, profile string, outcome ProfileMessageOutcome, transcripts []tools.Information) (string, error) {
	if outcome.Err != nil {
		return p.renderFailure(callID, profile, outcome.Err, transcripts...), nil
	}
	text, cut := cutUTF8(strings.TrimSpace(outcome.Text), int64(p.settings.ResultMaxBytes))
	information := append([]tools.Information{{Key: "profile", Value: profile}}, transcripts...)
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

func (p *profileMessageTool) renderFailure(callID, profile string, err error, information ...tools.Information) string {
	code, message := p.failure(profile, err)
	return tools.ExecutionError(callID, tools.StatusError, code, message, information...)
}

// failure names a message that did not end in an answer: the error code and its text.
func (p *profileMessageTool) failure(profile string, err error) (code, message string) {
	switch {
	case errors.Is(err, ErrProfileSameTranscript):
		return "profile_same_transcript", fmt.Sprintf("Profile %s shares this conversation and cannot be called", profile)
	case errors.Is(err, ErrProfileBusy):
		return "profile_busy", profileBusyText(profile, err)
	case errors.Is(err, ErrProfileStopped):
		return "profile_stopped", fmt.Sprintf("Profile %s was stopped", profile)
	case errors.Is(err, ErrProfileNoAnswer):
		return "profile_no_answer", fmt.Sprintf("Profile %s finished without an answer", profile)
	case errors.Is(err, ErrProfileChainTooLong):
		return "profile_chain_too_long", fmt.Sprintf("Profile %s cannot be messaged from here: the chain of profile messages would pass tools.native.profile_message.max_chain (%d)", profile, p.settings.MaxChain)
	case errors.Is(err, ErrProfileCycle):
		return "profile_cycle", fmt.Sprintf("Profile %s cannot be messaged from here: it would form a cycle", profile)
	case errors.Is(err, ErrProfileTimeout), errors.Is(err, context.DeadlineExceeded):
		return "profile_timeout", fmt.Sprintf("Profile %s did not answer within %d ms", profile, p.settings.TimeoutMs)
	}
	message = fmt.Sprintf("Profile %s failed", profile)
	var failed *ProfileFailedError
	if errors.As(err, &failed) && failed.Message != "" {
		message += ": " + failed.Message
	}
	return "profile_failed", message
}

func profileBusyText(profile string, err error) string {
	text := fmt.Sprintf("Profile %s is busy", profile)
	var busy *ProfileBusyError
	held := ""
	if errors.As(err, &busy) {
		held = busy.HeldBy
	}
	if held != "" {
		text += " with a message from profile " + held
	} else {
		text += " with a user run or a maintenance action"
	}
	if busy != nil && busy.Waited > 0 {
		text += fmt.Sprintf("; it was still busy after waiting %d ms", busy.Waited.Milliseconds())
	}
	if busy != nil && busy.QueueFull > 0 {
		text += fmt.Sprintf("; %d messages are already waiting for it (tools.native.profile_message.max_queued_per_profile)", busy.QueueFull)
	}
	return text + "; try again later"
}
