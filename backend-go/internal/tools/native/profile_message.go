package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const profileMessageToolID = "native.profile_message"

// profileMessageModelProperty is the schema line of the model argument; the tool has it only
// with tools.native.profile_message.model_choice on.
const profileMessageModelProperty = `
    "model":{"type":"string","description":"Model ID the profile answers this message with, optionally with @<effort>: one of the models that profile lists. Omit to use the profile's model default."},`

// profileMessageSandboxProperty is the schema line of the sandbox argument; the tool has it
// only with tools.native.profile_message.sandbox_choice set.
const profileMessageSandboxProperty = `
    "sandbox":{"type":"string","description":"Sandbox template the profile answers this message in: one of the sandboxes listed below. Omit to use the profile's own sandbox. It can only narrow that sandbox; one that would widen it is refused."},`

var profileMessageInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "profile":{"type":"string","minLength":1,"description":"Profile to message: one of the available profiles, or <session>/<category>/<profile> for a profile of another session this session may reach."},
    "message":{"type":"string","minLength":1,"description":"The message to send. The profile sees nothing of this conversation."},` + profileMessageModelProperty + profileMessageSandboxProperty + `
    "block_ms":{"type":"integer","minimum":0,"description":"Most time to block for the answer before an id is returned; collect it with the async tool."}
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

// ContextTriggerError is a configured context_trigger action that was due and could not be
// carried out. The message is not sent: a reset that failed must not let the target answer
// from the history it was asked to drop. Reason is client-safe wording (the details, which
// may name files, go to the log); Err is the cause, for errors.Is and logs only.
type ContextTriggerError struct {
	// Action is sessionfs.ProfileMessageContextReset or ProfileMessageContextCompact.
	Action string
	Reason string
	Err    error
}

func (e *ContextTriggerError) Error() string {
	text := "context_trigger " + e.Action + " could not run: " + e.Reason
	if e.Err != nil {
		text += ": " + e.Err.Error()
	}
	return text
}

func (e *ContextTriggerError) Unwrap() error { return e.Err }

// ErrContextTriggerForeignReset is returned by a ProfileMessenger when the caller's
// context_trigger is reset and the target is in another workspace. Nothing is sent and nothing
// of the target is touched: the trigger comes from the caller's settings, which can come with a
// cloned repository, and the target workspace allowing the caller to message it did not allow
// the caller to clear its history before every message. A compact is not refused.
var ErrContextTriggerForeignReset = errors.New("context_trigger reset is not allowed across workspaces")

// ProfileTranscriptLabels is optionally implemented by a plan whose target transcript path must
// not be shown to the model (the target is in another workspace): the text the tool shows for
// the target's and the caller's transcripts instead of Transcripts' paths, which stay for
// the host's own reads.
type ProfileTranscriptLabels interface {
	TranscriptLabels() (target, caller string)
}

// ErrProfileStopped is returned by a ProfileMessenger when the target run was stopped
// by the user while the caller was still waiting.
var ErrProfileStopped = errors.New("profile run was stopped")

// ErrProfileUnknown is returned by a ProfileMessenger when the target session or profile
// does not exist. The tool answers it exactly like a target its settings do not allow.
var ErrProfileUnknown = errors.New("profile is unknown")

// ProfileModelError is returned by a ProfileMessenger when the target profile cannot run the
// model the call named: it does not list it, or the model does not offer the effort.
type ProfileModelError struct{ Model, Message string }

func (e *ProfileModelError) Error() string { return "model " + e.Model + ": " + e.Message }

// ErrProfileSameTranscript is returned by a ProfileMessenger when the target profile
// shares the calling profile's transcript, so the call can never succeed.
var ErrProfileSameTranscript = errors.New("profile shares the calling transcript")

// ErrProfileNoAnswer is returned by a ProfileMessenger when the target run finished
// without any answer text.
var ErrProfileNoAnswer = errors.New("profile finished without an answer")

// ErrProfileCycle is returned by a ProfileMessenger when the message would reach a profile
// already in the chain of messages that led to the caller, or the caller itself.
var ErrProfileCycle = errors.New("profile message would form a cycle")

// errProfileDepthExceeded refuses a message whose run would pass max_depth. The tool raises it
// itself, after the messenger's checks, so a cycle or a too long chain is still reported as such.
var errProfileDepthExceeded = errors.New("profile message run would pass max_depth")

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
	// Session is the session of Target, empty for the calling session.
	Session string
	Target  string
	Message string
	// Model is the model ID (optionally @<effort>) the target runs this message with; empty
	// runs the target's model default.
	Model string
	// ContextTrigger asks for the target's transcript to be compacted or reset before every run
	// it starts (each message, not once per caller run). Its After, when above zero, is the input tokens
	// of the target's context (read from its messages.jsonl) that must be passed; zero acts
	// on any target whose messages.jsonl has content. The zero value does nothing.
	ContextTrigger sessionfs.ProfileMessageContextTrigger
	// QueueWait is the longest the message may wait for a busy target; zero sets no limit
	// beyond Timeout. Timeout bounds the whole life of the message, queue time included.
	QueueWait time.Duration
	Timeout   time.Duration
	// MaxQueued is the most messages that may wait for one target; a message over it fails
	// at once as busy. Zero queues nobody.
	MaxQueued int
	// MaxRunsPerRoot is the caller's max_runs_per_root (tools.native.subagent, its default
	// without the section): a local message's run joins the caller's run tree (Caller.RunNode)
	// under the lower of it and the tree's ceiling, and is refused with ErrRunTreeLimit beyond
	// it, while it is queued as while it runs. Zero is no limit. The session cap is not the
	// caller's: the run executes in the target's session, under the target's own
	// max_runs_per_session (the messenger reads it from the target's settings).
	MaxRunsPerRoot int
	// Blocking says the calling run cannot go on until the message ends (or the call's
	// block_ms passes), so a message that would wait for a profile that is itself blocked
	// on the caller is refused as a cycle.
	Blocking bool
	// Environment is the calling run's (nil without a sandbox) and CallerPlace what its
	// placeholders stand for: the target's session is narrowed by it (D20), and a target that
	// would reach more than the caller is refused with a *ProfileSandboxError.
	Environment *tools.Environment
	CallerPlace sessionfs.SandboxPlace
	// Sandbox is the sandbox template the call picked (sandbox_choice, resolved in the
	// caller's session): it narrows the target's run further. Nil picks none.
	Sandbox *sessionfs.SandboxChoice
	// Caller is the calling run (design section 3.3, phase R1): the call's run scope, the
	// remote caller, and the model and sandbox names the call gave. Nothing reads it yet; the
	// fields above and the call's context values are what the messenger uses.
	Caller tools.CallerContext
}

// ProfileSandboxError refuses a message to a profile whose own sandbox reaches more than the
// caller's: the target would run outside the environment the caller is confined to. Choice is
// set when it is the call's sandbox choice that would reach more than the target's run gets.
type ProfileSandboxError struct {
	Message string
	Choice  string
}

func (e *ProfileSandboxError) Error() string { return e.Message }

// sandboxProblem is the sandbox_unavailable problem of a refused sandbox choice.
func (e *ProfileSandboxError) sandboxProblem() *sandboxProblem {
	widens := strings.TrimPrefix(e.Message, sessionfs.SandboxWidensPrefix)
	return &sandboxProblem{message: fmt.Sprintf("Sandbox %s reaches more than the profile's run would otherwise get, so it cannot be used here: %s", e.Choice, widens), widens: widens}
}

// FullAnswer reads the whole answer of an outcome whose Text was cut at its source.
type FullAnswer struct {
	// Fetch writes the whole answer to w and returns its size.
	Fetch func(w io.Writer) (int64, error)
}

// ProfileMessageOutcome is how a message ended. Text is the target's final answer, or the
// last text it wrote when it did not finish. Err is nil for an answer; otherwise one of the
// ErrProfile* values, a *ProfileFailedError or a *ProfileBusyError. Reason says who ended a
// stopped message. Trace points at the turn in the target's transcript. ResumedFrom and
// ConfigNote are set by a remote sub-agent resume and only then (see the remote view).
// Worktree is set when the target ran in a git worktree of its own (OpenDelegatedWorktree):
// its worktree_* lines, also when the run failed.
type ProfileMessageOutcome struct {
	Text string
	// Cut says Text is not the whole answer: its source (a remote server's view) already cut
	// it. A local answer reaches the outcome whole.
	Cut bool
	// Full, set with Cut when the source can give the whole answer, reads it (a remote's result
	// route); the async handle saves it behind ref. Nil for an answer that is whole or cannot be
	// had. It is a pointer so an outcome stays comparable.
	Full        *FullAnswer
	Err         error
	Reason      string
	Trace       string
	ResumedFrom string
	ConfigNote  string
	Worktree    *DelegatedWorktreeResult
}

// worktreeInformation are the worktree_* lines of the outcome; none without a worktree.
func (o ProfileMessageOutcome) worktreeInformation() []tools.Information {
	if o.Worktree == nil {
		return nil
	}
	return o.Worktree.Information
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

// ProfileMessageNotes is optionally implemented by a plan: extra information keys its result
// carries, for what the plan did not do as configured.
type ProfileMessageNotes interface {
	Notes() []tools.Information
}

// shownTranscripts are the transcripts the model is shown: the plan's labels when it has them
// (a target of another workspace), else the files themselves.
func shownTranscripts(plan ProfileMessagePlan) (target, caller string, known bool) {
	if labelled, ok := plan.(ProfileTranscriptLabels); ok {
		target, caller = labelled.TranscriptLabels()
		return target, caller, true
	}
	if files, ok := plan.(ProfileMessageTranscripts); ok {
		target, caller = files.Transcripts()
		return target, caller, true
	}
	return "", "", false
}

// transcriptInformation is the transcript key (the target's file) and caller_transcript key
// of a plan that knows its files, then the plan's notes.
func transcriptInformation(plan ProfileMessagePlan) []tools.Information {
	var information []tools.Information
	if noted, ok := plan.(ProfileMessageNotes); ok {
		information = append(information, noted.Notes()...)
	}
	target, caller, known := shownTranscripts(plan)
	if !known {
		return information
	}
	if target != "" {
		information = append(information, tools.Information{Key: "transcript", Value: target})
	}
	if caller != "" {
		information = append(information, tools.Information{Key: "caller_transcript", Value: caller})
	}
	return information
}

type profileMessageTool struct {
	settings sessionfs.NativeProfileMessageSettings
	// maxDepth is the calling config's max_depth (NativeSubagentSettings.RunMaxDepth): a message
	// whose run would be deeper is refused before it is sent, local or remote.
	maxDepth int
	// maxPerRoot is the calling config's max_runs_per_root (RunMaxRunsPerRoot) a local message's
	// run is admitted into the run tree with.
	maxPerRoot int
	targets    []string
	messenger  ProfileMessenger
	host       *AsyncHost
	remotes    remoteSettings
}

// NewProfileMessageTool builds the profile_message tool over a messenger. targets are the
// profiles the model may message. A non-nil host lets a message that outlasts block_ms
// continue as a handle of the async tool.
func NewProfileMessageTool(settings sessionfs.NativeProfileMessageSettings, targets []string, messenger ProfileMessenger, host *AsyncHost) (tools.Tool, error) {
	return NewProfileMessageToolWithRemotes(settings, targets, messenger, host, nil)
}

// NewProfileMessageToolWithRemotes is NewProfileMessageTool plus the remotes folder the
// session's tools.native.profile_message.remotes names are read from. A nil directory offers
// no remote target.
func NewProfileMessageToolWithRemotes(settings sessionfs.NativeProfileMessageSettings, targets []string, messenger ProfileMessenger, host *AsyncHost, remotes *remote.Directory) (tools.Tool, error) {
	return newProfileMessageTool(settings, nil, targets, messenger, host, remotes)
}

// newProfileMessageTool is NewProfileMessageToolWithRemotes under subagent, the
// tools.native.subagent section of the config whose catalog holds the tool (nil: none, whose
// defaults apply): its max_depth and run caps bound the runs the tool's messages start.
func newProfileMessageTool(settings sessionfs.NativeProfileMessageSettings, subagent *sessionfs.NativeSubagentSettings, targets []string, messenger ProfileMessenger, host *AsyncHost, remotes *remote.Directory) (tools.Tool, error) {
	if messenger == nil {
		return tools.Tool{}, errors.New("tools.native.profile_message needs a messenger")
	}
	remoteNames := settings.Remotes
	if remotes == nil {
		remoteNames = nil
	}
	if len(targets) == 0 && len(remoteNames) == 0 && len(settings.Sessions) == 0 {
		return tools.Tool{}, sessionfs.ErrProfileMessageNoTargets
	}
	if settings.TimeoutMs < 1 {
		return tools.Tool{}, errors.New("tools.native.profile_message.timeout_ms must be at least 1")
	}
	if settings.ResultMaxBytes < 1 {
		return tools.Tool{}, errors.New("tools.native.profile_message.result_max_bytes must be at least 1")
	}
	schema := optionalPropertiesSchema(profileMessageInputSchema, []optionalProperty{{profileMessageModelProperty, settings.ModelChoice}, {profileMessageSandboxProperty, settings.SandboxChoice != nil}})
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native profile_message schema: %w", err)
	}
	if host != nil {
		host.attachProfileMessage()
	}
	runtime := &profileMessageTool{settings: settings, maxDepth: int(subagent.RunMaxDepth()), maxPerRoot: int(subagent.RunMaxRunsPerRoot()), targets: targets, messenger: messenger, host: host, remotes: remoteSettings{names: remoteNames, directory: remotes}}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          profileMessageToolID,
			Source:      "native",
			Name:        "profile_message",
			Description: profileMessageDescription + profileMessageListText(targets, settings.Sessions, remoteNames) + sandboxChoiceText(settings.SandboxChoices),
			InputSchema: schema,
		},
		Validator:          validator,
		ExampleOperation:   profileMessageExample,
		MayChangeFiles:     func(map[string]any) bool { return true },
		NormalizeArguments: normalizeProfileMessageArguments,
		Execute:            runtime.execute,
	}, nil
}

const profileMessageDescription = "Send a message to another profile and wait up to block_ms for its final answer. If the profile answers in time you get the answer; otherwise (it is still working, or busy and your message waits its turn) you get an id and the message goes on: use the async tool to wait for, list, inspect or stop it."

func profileMessageListText(targets, sessions, remotes []string) string {
	text := ""
	if len(targets) > 0 {
		text = " Profiles: " + strings.Join(targets, ", ") + "."
	}
	if len(sessions) > 0 {
		text += " Profiles of other existing sessions are written <session>/<category>/<profile> (sessions: " + strings.Join(sessions, ", ") + "); in the workspace layout a session without a workspace is one of your own workspace, and a glob <workspace>:<session> is a session of another workspace that also has to accept yours, written <workspace>:<session>/<category>/<profile>; a workspace named remote is written local:remote:<session>/<category>/<profile>, since remote: always means a remote server."
	}
	if len(remotes) > 0 {
		text += " Remote profiles on other JAD servers are written remote:<name>/[<workspace>:]<session>/<category>/<profile> (remotes: " + strings.Join(remotes, ", ") + "); the remote decides what you may reach, and <workspace>: is needed only when the remote holds that session id in several workspaces."
	}
	return text
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
	for _, key := range []string{"profile", "message", "model", "sandbox"} {
		value, ok := arguments[key].(string)
		if !ok {
			continue
		}
		if trimmed := strings.TrimSpace(value); trimmed == "" {
			delete(arguments, key)
		} else if key != "message" {
			arguments[key] = trimmed
		}
	}
	return arguments
}

func (p *profileMessageTool) execute(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
	callID := execution.CallID
	profile, _ := arguments["profile"].(string)
	message, _ := arguments["message"].(string)
	model, _ := arguments["model"].(string)
	sandboxName, _ := arguments["sandbox"].(string)
	if _, _, isRemote := remote.Split(profile); isRemote {
		if model != "" {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "model_unavailable", "A remote profile answers with its remote's own model; leave model out"), nil
		}
		if sandboxName != "" {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, sandboxUnavailableCode, "A remote profile answers in its remote's own environment; leave sandbox out"), nil
		}
		return p.executeRemote(ctx, arguments, execution, profile, message)
	}
	var choice *sessionfs.SandboxChoice
	if sandboxName != "" {
		chosen, problem := lookupSandboxChoice(sandboxName, p.settings.SandboxChoices, "profile messages")
		if problem != nil {
			return sandboxUnavailable(callID, problem, p.settings.SandboxChoices), nil
		}
		choice = &chosen
	}
	session, target, allowed := p.localTarget(profile)
	if !allowed {
		return p.unknownProfile(callID, profile), nil
	}
	owner, canHandle := handleOwner(execution)
	handled := p.host != nil && canHandle
	block := p.blockFor(arguments, execution)
	environment := callerEnvironment(execution)
	request := ProfileMessageRequest{
		Blocking:       !handled || block > 0,
		OwnerKey:       execution.SessionKey,
		Session:        session,
		Target:         target,
		Message:        message,
		Model:          model,
		ContextTrigger: p.settings.ContextTrigger,
		QueueWait:      time.Duration(p.settings.BusyWaitMs) * time.Millisecond,
		Timeout:        time.Duration(p.settings.TimeoutMs) * time.Millisecond,
		MaxQueued:      p.settings.MaxQueued,
		MaxRunsPerRoot: p.maxPerRoot,
		Environment:    environment,
		CallerPlace:    sessionfs.SandboxPlace{Workspace: execution.WorkspaceRoot, CWD: execution.WorkspaceRoot, SessionDir: execution.SessionDir, ProfileData: callerProfileData(environment)},
		Sandbox:        choice,
		Caller:         profileMessageCaller(ctx, execution, model, sandboxName),
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
	if errors.Is(err, ErrProfileUnknown) {
		return p.unknownProfile(callID, profile), nil
	}
	var sandboxErr *ProfileSandboxError
	if errors.As(err, &sandboxErr) && sandboxErr.Choice != "" {
		return sandboxUnavailable(callID, sandboxErr.sandboxProblem(), p.settings.SandboxChoices), nil
	}
	if errors.As(err, &sandboxErr) {
		return tools.ExecutionError(callID, tools.StatusForbidden, "sandbox_widens",
			fmt.Sprintf("Profile %s cannot be messaged from this run: its sandbox reaches more than this run's (%s)", profile, sandboxErr.Message)), nil
	}
	var modelErr *ProfileModelError
	if errors.As(err, &modelErr) {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "model_unavailable",
			fmt.Sprintf("Profile %s cannot answer with model %s: %s", profile, modelErr.Model, modelErr.Message)), nil
	}
	if err != nil {
		return p.renderFailure(callID, profile, err, transcriptInformation(plan)...), nil
	}
	if p.depthExceeded(execution) {
		// Checked after Prepare, whose refusals (cycle, chain, same transcript, sandbox, model)
		// keep their precedence, and before Start, so nothing is queued, reset or compacted.
		return p.renderFailure(callID, profile, errProfileDepthExceeded), nil
	}
	if !handled {
		return p.runInline(ctx, callID, profile, plan)
	}
	return p.runHandle(ctx, callID, profile, message, owner, plan, block)
}

// explicitLocalPrefix written before a <workspace>:<session> says that the workspace is a local
// one: "remote:" always means a remote server, so a workspace named remote is written
// local:remote:<session>. Only the two-colon form is read this way; local:<session> stays the
// session of a workspace named local, as before.
const explicitLocalPrefix = "local:"

var localProfilePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// localTarget splits a local profile argument: <category>/<profile> of the calling session, or
// <session>/<category>/<profile> (or <workspace>:<session>/... in the workspace layout) when a tools.native.profile_message.sessions glob allows the
// session. A profile denied by deny_profiles is denied in every session.
func (p *profileMessageTool) localTarget(profile string) (session, target string, allowed bool) {
	switch len(strings.Split(profile, "/")) {
	default:
		return "", profile, slices.Contains(p.targets, profile)
	case 3:
		session, target, _ = strings.Cut(profile, "/")
		if rest, found := strings.CutPrefix(session, explicitLocalPrefix); found && strings.Contains(rest, ":") {
			session = rest
		}
		// A session written <workspace>:<session> is matched only by the globs that name a
		// workspace, and a bare one only by the globs that do not: a bare "*" has always meant
		// the calling workspace and must not start to mean every workspace.
		qualified := strings.Contains(session, ":")
		if !remote.SessionRefPattern.MatchString(session) || !localProfilePattern.MatchString(target) || slices.Contains(p.settings.DenyProfiles, target) {
			return "", "", false
		}
		for _, glob := range p.settings.Sessions {
			if sessionfs.NamesWorkspace(glob) != qualified {
				continue
			}
			if ok, err := path.Match(glob, session); err == nil && ok {
				return session, target, true
			}
		}
	}
	return "", "", false
}

// unknownProfile is the answer for a target that is not allowed and for one that does not
// exist, so the two cannot be told apart.
func (p *profileMessageTool) unknownProfile(callID, profile string) string {
	return tools.ExecutionError(callID, tools.StatusInvalidInput, "unknown_profile",
		fmt.Sprintf("Profile %s is not available to message", profile),
		tools.Information{Key: "available", Value: strings.Join(p.targets, ", ")})
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
		return p.renderFailure(callID, profile, outcome.Err, append(append([]tools.Information(nil), transcripts...), outcome.worktreeInformation()...)...), nil
	}
	text, cut := cutUTF8(strings.TrimSpace(outcome.Text), int64(p.settings.ResultMaxBytes))
	information := append([]tools.Information{{Key: "profile", Value: profile}}, transcripts...)
	if cut || outcome.Cut {
		information = append(information, tools.Information{Key: "truncated", Value: true})
	}
	information = append(information, outcome.worktreeInformation()...)
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
	case errors.Is(err, ErrContextTriggerForeignReset):
		return "context_trigger_refused", fmt.Sprintf("Profile %s was not sent the message: this run's context_trigger is reset, and a reset of a profile in another workspace is not allowed. Nothing was sent or changed", profile)
	case errors.Is(err, ErrRunTreeParentGone):
		return "profile_unavailable", fmt.Sprintf("Profile %s cannot be messaged from here: %s", profile, err)
	case errors.Is(err, ErrRunTreeLimit):
		return "profile_limit_reached", fmt.Sprintf("Profile %s cannot be messaged from here: this run tree or the profile's session already has the most runs it may have going (tools.native.subagent.max_runs_per_root, max_runs_per_session); try again once some have ended", profile)
	case errors.Is(err, errProfileDepthExceeded):
		return "profile_depth_exceeded", fmt.Sprintf("Profile %s cannot be messaged from this depth (tools.native.subagent.max_depth)", profile)
	case errors.Is(err, ErrProfileCycle):
		return "profile_cycle", fmt.Sprintf("Profile %s cannot be messaged from here: it would form a cycle", profile)
	case errors.Is(err, ErrProfileTimeout), errors.Is(err, context.DeadlineExceeded):
		return "profile_timeout", fmt.Sprintf("Profile %s did not answer within %d ms", profile, p.settings.TimeoutMs)
	case errors.Is(err, ErrRemoteLost):
		return "remote_lost", fmt.Sprintf("Profile %s was lost: the remote could not be reached. The message may still be running there; check the remote's transcripts before sending it again", profile)
	}
	var remoteFailed *RemoteFailedError
	if errors.As(err, &remoteFailed) {
		return remoteFailed.Code, remoteFailed.Message
	}
	var trigger *ContextTriggerError
	if errors.As(err, &trigger) {
		return "context_trigger_failed", fmt.Sprintf("Profile %s was not sent the message: the context_trigger %s of its history could not run (%s). The message was not sent, and the reset may need attention: check the profile before relying on its history", profile, trigger.Action, trigger.Reason)
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

// profileMessageCaller is the CallerContext of a profile_message call (phase R1): the run scope
// of the call, the remote caller the remote API marked, and the model and sandbox the call named.
func profileMessageCaller(ctx context.Context, execution tools.ExecutionContext, model, sandboxName string) tools.CallerContext {
	caller := execution.Caller()
	caller.RemoteCaller = remoteCallerOf(ctx)
	caller.ModelChoice, caller.SandboxChoice = model, sandboxName
	return caller
}

// callerEnvironment is the environment a profile_message target is narrowed by: the calling
// run's, in its plan-mode form for a call of a plan-mode run, so a caller that may not write
// cannot have a target write for it.
// callerProfileData is the data folder the caller's sandbox fills {profile_data} with.
func callerProfileData(environment *tools.Environment) string {
	if environment == nil || environment.Sandbox == nil {
		return ""
	}
	return environment.Sandbox.ProfileData
}

func callerEnvironment(execution tools.ExecutionContext) *tools.Environment {
	environment := execution.Environment
	if environment == nil || environment.Sandbox == nil || !execution.PlanMode {
		return environment
	}
	return &tools.Environment{Sandbox: environment.Sandbox.ForPlanMode()}
}

// depthExceeded reports whether the run a message from execution starts would pass max_depth: it
// runs one level below the caller's RunDepth, by the one depth rule sub-agent starts use. A remote
// target is checked the same way here; the remote server counts its own depth from 0.
func (p *profileMessageTool) depthExceeded(execution tools.ExecutionContext) bool {
	return depthExceeds(execution.RunDepth(), p.maxDepth)
}
