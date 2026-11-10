package sessionfs

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
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

var (
	nativeProfileMessageKeys = []string{"deny_profiles", "timeout_ms", "busy_wait_ms", "result_max_bytes", "context_trigger", "compact", "compact_after", "max_chain", "max_queued_per_profile", "remotes", "sessions", "default_block_ms", "model_choice", "sandbox_choice"}
	// Same shape as the HTTP layer's profile name pattern; existence is not checked at load.
	profileMessageIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// NativeProfileMessageSettings holds the settings of native.profile_message. A nil
// pointer in SessionConfig means the tool is not configured.
type NativeProfileMessageSettings struct {
	// DenyProfiles are profile ids the model may not message, in configured order. The
	// targets are the caller's session profiles minus these and the caller itself.
	DenyProfiles []string
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
	// 2 lets a profile message another but that one message no further.
	MaxChain int
	// Sessions are globs of session ids (not root-qualified; '*' and '?' only) whose profiles
	// the model may message as <session>/<category>/<profile>, besides its own session's.
	// None by default, so a session reaches only its own profiles unless it says so.
	Sessions []string
	// Remotes are the names of the remote entries (the remotes folder) the model may message
	// with remote:<name>/...; none by default, so a session reaches no other server unless it says so.
	Remotes []string
	// ContextTrigger (context_trigger, or the legacy compact and compact_after) is what
	// happens to the target's transcript before the first message to it in each run of the
	// caller: nothing (default), a compaction, or a reset of its whole history.
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
}

func normalizeNativeProfileMessageSettings(setting map[string]json.RawMessage, settingPath string) (*NativeProfileMessageSettings, error) {
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
	for _, key := range keys {
		if !containsString(nativeProfileMessageKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeProfileMessageKeys, "")
		}
	}
	denyProfiles, err := normalizeProfileMessageDenyProfiles(fields["deny_profiles"], settingPath, section)
	if err != nil {
		return nil, err
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
	sessions, err := normalizeSessionGlobs(fields["sessions"], settingPath, section)
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
	return &NativeProfileMessageSettings{ModelChoice: modelChoice, SandboxChoice: sandboxChoice, Sessions: sessions, Remotes: remotes, DenyProfiles: denyProfiles, TimeoutMs: int(timeoutMS), DefaultBlockMs: defaultBlockMS, BusyWaitMs: int(busyWaitMS), MaxQueued: int(maxQueued), ResultMaxBytes: int(resultMaxBytes), MaxChain: int(maxChain), ContextTrigger: contextTrigger}, nil
}

// ErrProfileMessageNoTargets is the reason a profile_message tool cannot be built.
var ErrProfileMessageNoTargets = errors.New("profile_message has no profile to message: the session has no other profile, or every one is denied")

// ProfileMessageTargets returns the session's profiles the caller may message: every
// profile except the caller's own and the denied ones, in session order.
func ProfileMessageTargets(sessionProfiles []string, caller string, denied []string) []string {
	var targets []string
	for _, id := range sessionProfiles {
		if id != caller && !slices.Contains(denied, id) {
			targets = append(targets, id)
		}
	}
	return targets
}

// ProfileMessageNoTargetsError is the config error for a profile whose profile_message
// has no target; the tool catalog and `jad check` both report it.
func ProfileMessageNoTargetsError(settingPath, profile string) error {
	return configError("%s (profile %s): %v", settingPath, profile, ErrProfileMessageNoTargets)
}

func normalizeProfileMessageDenyProfiles(raw json.RawMessage, settingPath, section string) ([]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var profiles []string
	if json.Unmarshal(raw, &profiles) != nil {
		return nil, configError("%s: %s.deny_profiles must be an array of profile ids", settingPath, section)
	}
	seen := map[string]struct{}{}
	for _, id := range profiles {
		if !profileMessageIDPattern.MatchString(id) {
			return nil, configError("%s: %s.deny_profiles entry %q is not a valid profile id", settingPath, section, id)
		}
		if _, dup := seen[id]; dup {
			return nil, configError("%s: %s.deny_profiles lists %q more than once", settingPath, section, id)
		}
		seen[id] = struct{}{}
	}
	return profiles, nil
}

// sessionGlobPattern allows no '[', '/' or ':' so a glob can neither cross a segment nor name a root.
var sessionGlobPattern = regexp.MustCompile(`^[A-Za-z0-9*?][A-Za-z0-9._*?-]{0,127}$`)

func normalizeSessionGlobs(raw json.RawMessage, settingPath, section string) ([]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var globs []string
	if json.Unmarshal(raw, &globs) != nil || globs == nil {
		return nil, configError("%s: %s.sessions must be an array of session id globs", settingPath, section)
	}
	seen := map[string]struct{}{}
	for _, glob := range globs {
		if !sessionGlobPattern.MatchString(glob) {
			return nil, configError("%s: %s.sessions entry %q is not a session id glob (letters, digits, '.', '_', '-', '*' and '?')", settingPath, section, glob)
		}
		if _, dup := seen[glob]; dup {
			return nil, configError("%s: %s.sessions lists %q more than once", settingPath, section, glob)
		}
		seen[glob] = struct{}{}
	}
	return globs, nil
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
