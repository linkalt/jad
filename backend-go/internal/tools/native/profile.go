package native

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const profileToolID = "native.profile"

// profileInputSchema is the input of native.profile. Its path follows transcripts: profile
// is one of the caller's profiles, profile:<category>/<name> or profile:<session>/<category>/<name>
// names another, session:<id> lists a session's profiles, and remote:<name>/... asks a remote.
var profileInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","minLength":1,"description":"What to act on. Omit it, or give self, for your own profile. profile:<category>/<name> (or profile:<session>/<category>/<name>) names another profile; session:<id> names a session, whose profiles action list shows. remote:<name>/profile:<session>/<category>/<name> asks a remote JAD server for info; only when allow_remote is on."},
    "action":{"type":"string","enum":["list","info","compact","reset","config"],"description":"list: the profiles of the session. info: what the profile holds, with its context tokens. compact: compact the profile's transcript now. reset: delete the profile's transcript and history. config: your own profile only: its file, its revision, the settings it runs with and which layer set each, and its private and data folders. compact and reset are refused to a sub-agent and to your own profile; config is refused to a sub-agent and to another profile. reset is refused while a run of the profile is active; compact waits for a live run to end."},
    "filters":{"type":"array","items":{"type":"string"},"description":"Only profiles whose name matches every filter, case-insensitive. A filter with * or ? is a wildcard pattern (dev*, *coder); any other is a substring. Only with action list."},
    "limit":{"type":"integer","minimum":1,"description":"Most items in this response; capped by the configuration. Only with action list."},
    "cursor":{"type":"string","minLength":1,"description":"Continue where the previous list response stopped; use the cursor it gave. Only with action list."}
  },
  "required":["action"],
  "additionalProperties":false
}`)

// ProfileActions is the port native.profile uses to read and change profiles. The resolver
// implements it: info reads the profile's latest usage, compact and reset go through the same
// maintenance the HTTP routes use. ownerKey is the session key of the calling run, so the
// session is looked up as the caller sees it (in the workspace layout, only in its workspace).
type ProfileActions interface {
	// ProfileInfo returns the input tokens of the profile's last completed response, or nil
	// when no response has completed (or none carried usage).
	ProfileInfo(ctx context.Context, ownerKey, session, profile string) (*int64, error)
	// CompactProfile compacts the profile's transcript now.
	CompactProfile(ctx context.Context, ownerKey, session, profile string) error
	// ResetProfile resets the profile's transcript and history; it refuses while a run is active.
	ResetProfile(ctx context.Context, ownerKey, session, profile string) error
	// ProfileConfig describes the caller's own profile: the session and profile come from
	// ownerKey only, never from the model's input.
	ProfileConfig(ctx context.Context, ownerKey string, run sessionfs.ProfileRun) (sessionfs.ProfileConfig, error)
}

// profileNoUsage is what info prints when no response has completed.
const profileNoUsage = "no usage yet"

// parseProfilePath reads the path of native.profile. An empty path is the caller's own profile.
func parseProfilePath(path string) (session, profile string, ok bool) {
	if path == "" {
		return "", "", true
	}
	kind, rest, found := strings.Cut(path, ":")
	if !found || rest == "" {
		return "", "", false
	}
	parts, ok := splitTranscriptName(rest)
	if !ok {
		return "", "", false
	}
	switch {
	case kind == "profile" && len(parts) == 2:
		return "", strings.Join(parts, "/"), true
	case kind == "profile" && len(parts) == 3:
		return parts[0], parts[1] + "/" + parts[2], true
	}
	return "", "", false
}

type profileTool struct {
	settings sessionfs.NativeProfileSettings
	own      TranscriptFiles
	profiles TranscriptProfiles
	actions  ProfileActions
	remotes  remoteSettings
}

// NewProfileTool constructs native.profile. actions may be nil: the actions then answer that
// profiles are unavailable, and info and list show nothing they cannot read.
func NewProfileTool(settings sessionfs.NativeProfileSettings, own TranscriptFiles, profiles TranscriptProfiles, actions ProfileActions) (tools.Tool, error) {
	return NewProfileToolWithRemotes(settings, own, profiles, actions, nil, nil)
}

// NewProfileToolWithRemotes is NewProfileTool plus the remotes the tool may read the info of.
func NewProfileToolWithRemotes(settings sessionfs.NativeProfileSettings, own TranscriptFiles, profiles TranscriptProfiles, actions ProfileActions, names []string, directory *remote.Directory) (tools.Tool, error) {
	validator, err := tools.CompileSchema(profileInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native profile schema: %w", err)
	}
	tool := &profileTool{settings: settings, own: own, profiles: profiles, actions: actions, remotes: remoteSettings{names: names, directory: directory}}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          profileToolID,
			Source:      "native",
			Name:        "profile",
			Description: profileDescription,
			InputSchema: append(json.RawMessage(nil), profileInputSchema...),
		},
		Validator:        validator,
		ReadOnly:         false,
		ExampleOperation: json.RawMessage(`{"action":"info"}`),
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return tool.execute(ctx, arguments, execution), nil
		},
	}, nil
}

const profileDescription = "Work on the profiles of a session: list them, show what a profile holds with its context tokens (info), compact its transcript or reset it, or show your own profile's configuration (config)."

func (t *profileTool) fail(execution tools.ExecutionContext, status tools.Status, code, summary string, information ...tools.Information) string {
	return tools.ExecutionError(execution.CallID, status, code, summary, information...)
}

func (t *profileTool) execute(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) string {
	action := stringArgument(arguments, "action")
	path := stringArgument(arguments, "path")
	if execution.AgentDepth > 0 && !t.settings.AllowChildren {
		return t.fail(execution, tools.StatusForbidden, "profile_children_denied", "A sub-agent may not use the profile tool: tools.native.profile.allow_children is false")
	}
	if strings.HasPrefix(path, remote.Prefix) {
		return t.executeRemote(ctx, action, path, execution)
	}
	if action == "config" {
		return t.config(ctx, path, execution)
	}
	if action == "list" && path != "" && !strings.HasPrefix(path, "session:") {
		// list takes a session (session:<id>) or nothing; a profile path lists one profile's info.
		return t.fail(execution, tools.StatusInvalidInput, "invalid_path", "list takes path session:<id> or no path", tools.Information{Key: "valid", Value: "session:<id>"})
	}
	filters := profileFilters(arguments)
	limit := int(t.settings.MaxEntries)
	if value, ok := safeInteger(arguments["limit"]); ok {
		limit = int(min(value, t.settings.MaxEntries))
	}
	cursor := stringArgument(arguments, "cursor")
	callerSession, callerProfile, hasCaller := strings.Cut(execution.SessionKey, "\x00")
	if path == "self" {
		// self is the caller's own profile; it is mapped after the list gate, so list refuses it above.
		if !hasCaller || callerSession == "" || callerProfile == "" {
			return t.fail(execution, tools.StatusForbidden, "profile_caller_unknown", "The caller's own profile is not known here: self needs a session and a profile")
		}
		path = ""
	}
	session, profile, ok := parseProfilePath(path)
	if action == "list" && strings.HasPrefix(path, "session:") {
		session, profile, ok = strings.TrimPrefix(path, "session:"), "", true
	}
	if !ok {
		return t.fail(execution, tools.StatusInvalidInput, "invalid_path", "path is not a form this tool accepts", tools.Information{Key: "valid", Value: "profile:<category>/<name>, profile:<session>/<category>/<name>"})
	}
	if profile == "" {
		profile = callerProfile
	}
	if session == "" {
		session = callerSession
	}
	if (action == "compact" || action == "reset") && execution.AgentDepth > 0 {
		return t.fail(execution, tools.StatusForbidden, "profile_children_denied", "A sub-agent may not "+action+" a profile")
	}
	if session != callerSession && !t.settings.AllowOtherSessions {
		return t.fail(execution, tools.StatusForbidden, "profile_session_denied", "Another session: tools.native.profile.allow_other_sessions is false")
	}
	if action != "list" && profile != callerProfile && !t.settings.AllowOtherProfiles {
		return t.fail(execution, tools.StatusForbidden, "profile_profile_denied", "Another profile: tools.native.profile.allow_other_profiles is false")
	}
	if (action == "compact" || action == "reset") && (session == "" || session == callerSession) && profile == callerProfile {
		return t.fail(execution, tools.StatusInvalidInput, "profile_self", "A profile may not "+action+" itself from inside its own run: name another profile, or call "+action+" outside a run")
	}
	if t.actions == nil && action != "list" {
		return t.fail(execution, tools.StatusError, "profile_unavailable", "Profiles are not available to this tool")
	}
	switch action {
	case "list":
		return t.list(ctx, execution, session, filters, limit, cursor)
	case "info":
		return t.info(ctx, execution, session, profile)
	case "compact":
		if err := t.actions.CompactProfile(ctx, execution.SessionKey, session, profile); err != nil {
			return t.fail(execution, tools.StatusError, "profile_compact_failed", err.Error())
		}
		return "compacted profile " + profile
	case "reset":
		if err := t.actions.ResetProfile(ctx, execution.SessionKey, session, profile); err != nil {
			return t.fail(execution, tools.StatusError, "profile_reset_failed", err.Error())
		}
		return "reset profile " + profile
	}
	return t.fail(execution, tools.StatusInvalidInput, "invalid_input", "action must be list, info, compact, reset or config")
}

// config shows the caller its own profile's configuration. Only the caller's own profile, from
// its execution context, is ever read: another profile is refused whatever allow_other_profiles
// says, and so is a sub-agent, whatever allow_children says. Plan mode does not refuse it: it
// changes nothing.
func (t *profileTool) config(ctx context.Context, path string, execution tools.ExecutionContext) string {
	if execution.AgentDepth > 0 {
		return t.fail(execution, tools.StatusForbidden, "profile_children_denied", "A sub-agent may not read a profile's config: config is for a profile's own runs")
	}
	callerSession, callerProfile, hasCaller := strings.Cut(execution.SessionKey, "\x00")
	if !hasCaller || callerSession == "" || callerProfile == "" {
		return t.fail(execution, tools.StatusForbidden, "profile_caller_unknown", "The caller's own profile is not known here: config needs a session and a profile")
	}
	if path != "" && path != "self" {
		session, profile, ok := parseProfilePath(path)
		if !ok {
			return t.fail(execution, tools.StatusInvalidInput, "invalid_path", "path is not a form this tool accepts", tools.Information{Key: "valid", Value: "self"})
		}
		if profile != callerProfile || (session != "" && session != callerSession) {
			return t.fail(execution, tools.StatusForbidden, "profile_config_self_only", "config shows only your own profile: omit path, or give self")
		}
	}
	if t.actions == nil {
		return t.fail(execution, tools.StatusError, "profile_unavailable", "Profiles are not available to this tool")
	}
	run := sessionfs.ProfileRun{PlanMode: execution.PlanMode}
	if execution.Environment != nil {
		run.HasEnvironment, run.Sandbox = true, execution.Environment.Sandbox
	}
	config, err := t.actions.ProfileConfig(ctx, execution.SessionKey, run)
	if err != nil {
		return t.fail(execution, tools.StatusError, "profile_config_failed", err.Error())
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return t.fail(execution, tools.StatusError, "profile_config_failed", err.Error())
	}
	return string(encoded)
}

// executeRemote reads the info of a profile on a remote JAD server, the only remote action. Compact
// and reset are refused here, before any request is made.
func (t *profileTool) executeRemote(ctx context.Context, action, path string, execution tools.ExecutionContext) string {
	if action != "info" {
		return t.fail(execution, tools.StatusForbidden, "profile_remote_denied", "A remote profile takes action info only: compact and reset act on this server's profiles")
	}
	if !t.settings.AllowRemote || t.remotes.directory == nil || len(t.remotes.names) == 0 {
		return t.fail(execution, tools.StatusForbidden, "remote_disabled", "Reading another JAD server is off: set allow_remote and remotes in this tool's settings")
	}
	name, _, _ := remote.Split(path)
	if !slices.Contains(t.remotes.names, name) {
		return t.fail(execution, tools.StatusInvalidInput, "unknown_remote", fmt.Sprintf("Remote %s is not available to this tool", name), tools.Information{Key: "available", Value: strings.Join(t.remotes.names, ", ")})
	}
	// The path is remote:<name>/profile:<session>/<category>/<name>: the kind is checked here and
	// the rest is the reference remote.ParseProfile reads.
	if !strings.HasPrefix(path, remote.Prefix+name+"/profile:") {
		return t.fail(execution, tools.StatusInvalidInput, "bad_remote_reference", "a remote path is remote:<name>/profile:<session>/<category>/<name>")
	}
	target, err := remote.ParseProfile(strings.Replace(path, "/profile:", "/", 1), t.remotes.directory.Entry)
	if err != nil {
		return t.fail(execution, tools.StatusInvalidInput, "bad_remote_reference", err.Error())
	}
	entry, err := t.remotes.directory.Entry(name)
	if err != nil {
		return t.fail(execution, tools.StatusError, "remote_unavailable", remoteProblem(name, err))
	}
	if !entry.Allows(target.Scope()) {
		return t.fail(execution, tools.StatusForbidden, "remote_not_allowed", fmt.Sprintf("Profile %s is not among the profiles this session may reach on remote %s", target.Scope(), name))
	}
	client, err := t.remotes.directory.Client(name)
	if err != nil {
		return t.fail(execution, tools.StatusError, "remote_unavailable", remoteProblem(name, err))
	}
	output, err := client.ProfileInfo(ctx, target.Session, target.Profile, execution.RemoteHops+1)
	if err != nil {
		return t.fail(execution, tools.StatusError, "remote_failed", err.Error())
	}
	return output
}

// info reports the context tokens of a profile: the input tokens of its last completed response.
// Before any response completed there is no usage, which is said, not reported as 0.
func (t *profileTool) info(ctx context.Context, execution tools.ExecutionContext, session, profile string) string {
	if t.profiles != nil {
		target, err := t.profiles.Profile(ctx, execution.SessionKey, session, profile)
		if err == nil && transcriptDenied(scopedPolicy(t.settings.Policy, execution), target.Files.Messages) {
			return t.fail(execution, tools.StatusForbidden, "profile_denied", "This profile's transcript is not readable by this tool: session_files.paths denies it")
		}
	}
	tokens, err := t.actions.ProfileInfo(ctx, execution.SessionKey, session, profile)
	if err != nil {
		return t.fail(execution, tools.StatusError, "profile_info_failed", err.Error())
	}
	return ProfileInfoText(profile, tokens)
}

// ProfileInfoText is the one line that reports a profile's context tokens; the local tool and the
// remote API both print it, so they cannot differ.
func ProfileInfoText(profile string, tokens *int64) string {
	if tokens == nil {
		return "profile " + profile + " | context tokens: " + profileNoUsage
	}
	return fmt.Sprintf("profile %s | context tokens: %d", profile, *tokens)
}

// profileFilters reads the filters argument: each is matched case-insensitively against the
// profile name, a wildcard with * and ? or a substring otherwise.
func profileFilters(arguments map[string]any) []toolSmithFilter {
	var filters []toolSmithFilter
	given, _ := arguments["filters"].([]any)
	for _, item := range given {
		if text, ok := item.(string); ok {
			if text = strings.ToLower(strings.TrimSpace(text)); text != "" {
				filters = append(filters, newToolSmithFilter(text))
			}
		}
	}
	return filters
}

type profileItem struct {
	name     string
	activity int64
	line     string
}

// list shows the profiles of a session, newest transcript first. A profile the caller may not
// see is left out (allow_other_profiles, the read path rules); the scan limit refuses a listing
// that would examine too many, and the limit pages the result.
func (t *profileTool) list(ctx context.Context, execution tools.ExecutionContext, session string, filters []toolSmithFilter, limit int, cursor string) string {
	callerSession, callerProfile, _ := strings.Cut(execution.SessionKey, "\x00")
	if session == "" {
		session = callerSession
	}
	if t.profiles == nil {
		return t.listResult(execution, session, nil, limit, cursor, 0)
	}
	names, sessions := t.profiles.Candidates(ctx, execution.SessionKey, session)
	if names == nil && session != callerSession && !containsName(sessions, session) {
		return t.fail(execution, tools.StatusNotFound, "no_such_session", "No session "+session)
	}
	var candidates []string
	for _, name := range names {
		if !t.settings.AllowOtherProfiles && name != callerProfile {
			continue
		}
		if matchesFilters(filters, strings.ToLower(name), "") {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) > int(t.settings.ScanMaxEntries) {
		return t.fail(execution, tools.StatusInvalidInput, "too_many", "More profiles match than tools.native.profile.scan_max_entries allows: add filters",
			tools.Information{Key: "matched", Value: len(candidates)}, tools.Information{Key: "scan_max_entries", Value: t.settings.ScanMaxEntries})
	}
	var items []profileItem
	hidden := 0
	for _, name := range candidates {
		if err := ctx.Err(); err != nil {
			return t.fail(execution, tools.StatusError, "profile_cancelled", "The call was cancelled")
		}
		target, err := t.profiles.Profile(ctx, execution.SessionKey, session, name)
		if err != nil {
			continue
		}
		own := target.Session == callerSession && target.Profile == callerProfile
		if !own && transcriptDenied(scopedPolicy(t.settings.Policy, execution), target.Files.Messages) {
			hidden++
			continue
		}
		items = append(items, t.item(target, own))
	}
	return t.listResult(execution, session, items, limit, cursor, hidden)
}

// item formats one profile line: when its transcript last changed, how large it is and whether a
// compaction backup exists, then the path for transcript and the file (hidden with HidePaths).
func (t *profileTool) item(target TranscriptTarget, own bool) profileItem {
	label := "profile " + target.Profile
	if own {
		label += " (yours)"
	}
	path := "profile:" + target.Session + "/" + target.Profile
	file := statTranscriptsFile(target.Files.Messages)
	item := profileItem{name: target.Profile}
	switch {
	case file.unreadable:
		item.line = label + " | unreadable: messages.jsonl is not a regular file inside its folder"
	case !file.exists:
		item.line = label + " | no transcript yet | path " + path + t.fileField(target.Files.Messages)
	default:
		item.activity = file.modified.UnixNano()
		backup := "no"
		if target.Files.Backup != "" {
			if info := statTranscriptsFile(target.Files.Backup); info.exists && !info.unreadable {
				backup = "yes"
			}
		}
		item.line = fmt.Sprintf("%s | active %s | %d bytes | backup %s | path %s%s", label, transcriptsTime(file.modified), file.size, backup, path, t.fileField(target.Files.Messages))
	}
	return item
}

// fileField is the " | file <path>" end of a line, empty when paths are hidden (the remote API).
func (t *profileTool) fileField(path string) string {
	if t.settings.HidePaths {
		return ""
	}
	return " | file " + path
}

// listResult orders the items newest first, pages them after the cursor and renders them.
func (t *profileTool) listResult(execution tools.ExecutionContext, session string, items []profileItem, limit int, cursor string, hidden int) string {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].activity != items[j].activity {
			return items[i].activity > items[j].activity
		}
		return items[i].name < items[j].name
	})
	if cursor != "" {
		after, ok := decodeProfileCursor(cursor)
		if !ok {
			return t.fail(execution, tools.StatusInvalidInput, "invalid_cursor", "cursor is not a cursor this tool gave")
		}
		start := sort.Search(len(items), func(i int) bool {
			if items[i].activity != after.Activity {
				return items[i].activity < after.Activity
			}
			return items[i].name > after.Name
		})
		items = items[start:]
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	summary := fmt.Sprintf("%d profiles of session %s", len(items), session)
	options := tools.ResultOptions{ID: execution.CallID, Status: tools.StatusOK, Summary: summary}
	var lines []string
	for _, item := range items {
		lines = append(lines, item.line)
	}
	if hidden > 0 {
		options.Information = append(options.Information, tools.Information{Key: "hidden", Value: fmt.Sprintf("%d not readable by this caller (read path rules, or allow_* switches)", hidden)})
	}
	if more {
		last := items[len(items)-1]
		options.Information = append(options.Information,
			tools.Information{Key: "truncated", Value: true},
			tools.Information{Key: "cursor", Value: encodeProfileCursor(profileCursor{Activity: last.activity, Name: last.name})})
	}
	if len(lines) > 0 {
		options.Content = &tools.Content{Text: strings.Join(lines, "\n")}
	}
	result, err := tools.RenderToolResult(options)
	if err != nil {
		return t.fail(execution, tools.StatusError, "list_failed", "The result could not be rendered", tools.Information{Key: "system_error", Value: err.Error()})
	}
	return result
}

// profileCursor continues a list after the item (Activity, Name), as transcripts' cursor does.
type profileCursor struct {
	Activity int64  `json:"a"`
	Name     string `json:"n"`
}

func encodeProfileCursor(cursor profileCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeProfileCursor(text string) (profileCursor, bool) {
	var cursor profileCursor
	raw, err := base64.RawURLEncoding.DecodeString(text)
	return cursor, err == nil && json.Unmarshal(raw, &cursor) == nil
}
