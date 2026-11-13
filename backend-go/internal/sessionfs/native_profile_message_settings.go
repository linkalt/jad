package sessionfs

import (
	"encoding/json"
	"regexp"
	"strings"
)

const (
	defaultNativeProfileMessageTimeoutMS      int64 = 600_000
	maxNativeProfileMessageTimeoutMS          int64 = maxToolTimeoutMs
	defaultNativeProfileMessageResultMaxBytes int64 = 16 * 1024
	maxNativeProfileMessageMaxChain           int64 = 8
	defaultNativeProfileMessageMaxQueued      int64 = 4
	maxNativeProfileMessageMaxQueued          int64 = 256
)

// DefaultNativeAgentBlockMs is the default_block_ms of subagent and profile_message: a model
// run usually takes longer than a shell command, so these calls wait longer for the answer
// than tools.native.async default_block_ms (which the other start tools use) before they
// hand back an id to collect with async.
const DefaultNativeAgentBlockMs int64 = 120_000

// DefaultProfileMessageMaxChain is the longest chain of profiles a message may travel
// through when max_chain is not set: the caller and one profile it reaches.
const DefaultProfileMessageMaxChain = 2

// nativeProfileMessageKeys are the keys the section knows. deny_profiles and sessions (the
// removed v1 keys) are refused before this list is looked at, so that they get their replacement
// hint, not a generic unknown key.
var nativeProfileMessageKeys = []string{"deny", "timeout_ms", "busy_wait_ms", "result_max_bytes", "context_trigger", "max_chain", "max_queued_per_profile", "remotes", "default_block_ms", "model_choice", "sandbox_choice"}

// NativeProfileMessageSettings holds the settings of native.profile_message. A nil
// pointer in SessionConfig means the tool is not configured.
type NativeProfileMessageSettings struct {
	// TimeoutMs bounds the whole call, the time queued behind a busy target included.
	TimeoutMs int
	// DefaultBlockMs is how long a call that sends no block_ms waits for the answer before
	// it returns an id, clamped to tools.native.async max_block_ms. Zero (settings built in
	// code) means the async default_block_ms.
	// A call that later calls of its response wait behind (ExecutionContext.BatchWaiting) waits
	// the async default_block_ms instead.
	DefaultBlockMs int64
	// BusyWaitMs is the longest a call may stay queued behind a busy target before it fails
	// with profile_busy; 0 (default) sets no limit of its own, so TimeoutMs bounds the wait.
	BusyWaitMs int
	// MaxQueued is the most calls that may wait for one target profile at once; a call
	// over it fails at once with profile_busy. 0 queues nobody: a busy target fails at once.
	MaxQueued int
	// ResultMaxBytes is the most reply text one call returns.
	ResultMaxBytes int
	// MaxChain is the most profiles one message chain may hold, the first caller included.
	// 2 lets a profile message another but that one message no further. It is a cap of its
	// own beside tools.native.subagent.max_depth (NativeSubagentSettings.RunMaxDepth): a message
	// must pass both, so a longer max_chain never lets a chain run deeper than max_depth.
	MaxChain int
	// Remotes are the names of the remote entries (the remotes folder) the model may message
	// with remote:<name>/...; none by default, so a session reaches no other server unless it says so.
	Remotes []string
	// ContextTrigger (context_trigger) is what happens to the target's transcript before a
	// message: nothing (default), a compaction before every message, or a reset of its whole
	// history before every message.
	ContextTrigger ProfileMessageContextTrigger
	// ModelChoice (model_choice, off by default) gives the tool a model argument, the model
	// the target answers with. Off, the tool's definition is the one it had before the model
	// argument existed, and a call cannot name a model.
	ModelChoice bool
	// SandboxChoice (sandbox_choice, off by default) are the sandbox templates a call's sandbox
	// argument may pick for the target's run; SandboxChoices are them resolved by the
	// catalog-layout loader in the caller's session.
	SandboxChoice  []string
	SandboxChoices []SandboxChoice `json:"-"`
	// Caller is the calling session. Addresses are read by the grammar of message_address.go.
	// Set by the session loader, never by a setting.
	Caller MessageCaller `json:"-"`
	// Workspaces are the ids of the other workspaces a message may reach by local:<workspace>/...:
	// the listed workspaces of this process, when the caller's own workspace is listed too. It
	// only feeds the tool's description; the messenger decides.
	Workspaces []string `json:"-"`
	// Deny are the toolset's deny rules (workspace layout; tools.native.profile_message.deny):
	// calls this toolset's profiles may not make. GlobalDeny and WorkspaceDeny are the rules of
	// G/global.json and of the caller's workspace.json, set by the session loader; the messenger
	// judges every call by all three again, this copy only keeps denied profiles out of the tool
	// description.
	Deny          []MessageRule `json:"-"`
	GlobalDeny    []MessageRule `json:"-"`
	WorkspaceDeny []MessageRule `json:"-"`
}

// removedProfileMessageSessionsHint: the sessions globs of the retired v1 layout no longer gate
// anything; a profile reaches any profile of a session of its workspace and of a listed
// workspace, and what it may not reach is a deny rule (design §30).
const removedProfileMessageSessionsHint = "other sessions and listed workspaces are reachable without it, written <session>/<category>/<profile> or local:<workspace>/<session>/<category>/<profile>; delete the key, and use deny to restrict what a profile may reach"

// removedProfileMessageDenyProfilesHint: deny_profiles named profiles by <category>/<profile> in
// the caller's workspace; deny is the one general rule list. The replacement for each entry is
// {"to": "local:*/*/<category>/<profile>"}, which refuses at least what the entry refused.
const removedProfileMessageDenyProfilesHint = `replace each entry <category>/<profile> with a deny rule {"to": "local:*/*/<category>/<profile>"} in tools.native.profile_message.deny, and delete the key`

// normalizeNativeProfileMessageSettings reads tools.native.profile_message of the calling session
// caller. The keys of the retired v1 layout (sessions, deny_profiles) are errors that say what
// replaces them.
func normalizeNativeProfileMessageSettings(setting map[string]json.RawMessage, settingPath string, caller MessageCaller) (*NativeProfileMessageSettings, error) {
	nativeRaw, exists, err := nativeEnvelope(setting, settingPath)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["profile_message"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.profile_message"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	if err := rejectRemovedApproval(fields, section, settingPath); err != nil {
		return nil, err
	}
	for _, key := range []string{"compact", "compact_after"} {
		if _, removed := fields[key]; removed {
			return nil, configError("%s: %s.%s was removed; %s", settingPath, section, key, removedProfileMessageCompactHint)
		}
	}
	if _, removed := fields["sessions"]; removed {
		return nil, configError("%s: %s.sessions was removed with workspace layout v1; %s", settingPath, section, removedProfileMessageSessionsHint)
	}
	if _, removed := fields["deny_profiles"]; removed {
		return nil, configError("%s: %s.deny_profiles was removed with workspace layout v1; %s", settingPath, section, removedProfileMessageDenyProfilesHint)
	}
	for _, key := range keys {
		if !containsString(nativeProfileMessageKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeProfileMessageKeys, "")
		}
	}
	var deny []MessageRule
	if raw, present := fields["deny"]; present {
		if deny, err = parseMessageRules(raw, settingPath+": "+section+".deny", false, true, ""); err != nil {
			return nil, err
		}
	}
	timeoutMS, err := configuredInteger(fields, "timeout_ms", defaultNativeProfileMessageTimeoutMS, 1, maxNativeProfileMessageTimeoutMS, settingPath, section)
	if err != nil {
		return nil, err
	}
	busyWaitMS, err := configuredInteger(fields, "busy_wait_ms", 0, 0, maxNativeProfileMessageTimeoutMS, settingPath, section)
	if err != nil {
		return nil, err
	}
	if busyWaitMS > timeoutMS {
		return nil, configError("%s: %s.busy_wait_ms must not exceed timeout_ms", settingPath, section)
	}
	defaultBlockMS, err := configuredInteger(fields, "default_block_ms", DefaultNativeAgentBlockMs, 1, maxToolTimeoutMs, settingPath, section)
	if err != nil {
		return nil, err
	}
	resultMaxBytes, err := configuredInteger(fields, "result_max_bytes", defaultNativeProfileMessageResultMaxBytes, 1, maxNativeToolBytes, settingPath, section)
	if err != nil {
		return nil, err
	}
	maxChain, err := configuredInteger(fields, "max_chain", int64(DefaultProfileMessageMaxChain), 2, maxNativeProfileMessageMaxChain, settingPath, section)
	if err != nil {
		return nil, err
	}
	maxQueued, err := configuredInteger(fields, "max_queued_per_profile", defaultNativeProfileMessageMaxQueued, 0, maxNativeProfileMessageMaxQueued, settingPath, section)
	if err != nil {
		return nil, err
	}
	contextTrigger, err := normalizeProfileMessageContextTrigger(fields, settingPath, section)
	if err != nil {
		return nil, err
	}
	remotes, err := normalizeRemoteNames(fields["remotes"], settingPath, section)
	if err != nil {
		return nil, err
	}
	modelChoice, err := bashBool(fields, "model_choice", false, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	sandboxChoice, err := normalizeSandboxChoiceNames(fields["sandbox_choice"], settingPath, section)
	if err != nil {
		return nil, err
	}
	return &NativeProfileMessageSettings{Caller: caller, Deny: deny, ModelChoice: modelChoice, SandboxChoice: sandboxChoice, Remotes: remotes, TimeoutMs: int(timeoutMS), DefaultBlockMs: defaultBlockMS, BusyWaitMs: int(busyWaitMS), MaxQueued: int(maxQueued), ResultMaxBytes: int(resultMaxBytes), MaxChain: int(maxChain), ContextTrigger: contextTrigger}, nil
}

// ProfileMessageTargets returns the session's profiles other than the caller's own, in session
// order: the profiles the tool description lists (before the deny rules are applied to them).
func ProfileMessageTargets(sessionProfiles []string, caller string) []string {
	var targets []string
	for _, id := range sessionProfiles {
		if id != caller {
			targets = append(targets, id)
		}
	}
	return targets
}

var remoteNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// normalizeRemoteNames reads a tool's remotes allow-list: distinct remote entry names.
func normalizeRemoteNames(raw json.RawMessage, settingPath, section string) ([]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var names []string
	if json.Unmarshal(raw, &names) != nil || names == nil {
		return nil, configError("%s: %s.remotes must be an array of remote names", settingPath, section)
	}
	seen := map[string]struct{}{}
	for _, name := range names {
		if !remoteNamePattern.MatchString(name) {
			return nil, configError("%s: %s.remotes entry %q is not a remote name (letters, digits, '_' and '-')", settingPath, section, name)
		}
		if _, dup := seen[name]; dup {
			return nil, configError("%s: %s.remotes lists %q more than once", settingPath, section, name)
		}
		seen[name] = struct{}{}
	}
	return names, nil
}
