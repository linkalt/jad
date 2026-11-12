package native

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const transcriptsToolID = "native.transcripts"

var transcriptsInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","minLength":1,"description":"What to list: profile:<category>/<name> (or profile:<session>/<category>/<name>) for the sub-agent runs of one profile; sessions for the sessions; remote:<name>/session:<id> (or remote:<name>) for a remote JAD server's sessions and remote:<name>/profile:<session>/<category>/<name> for its runs, only when allow_remote is on. The profiles of a session are listed by the profile tool (action list)."},
    "filters":{"type":"array","items":{"type":"string"},"description":"Only items whose name matches every filter, case-insensitive. A filter with * or ? is a wildcard pattern (dev*, *coder); any other is a substring. For runs the agent name and the parent call id are matched too."},
    "status":{"type":"string","enum":["queued","running","done","failed","timed_out","stopped","lost"],"description":"Only sub-agent runs in this state. Only with a profile: path."},
    "limit":{"type":"integer","minimum":1,"description":"Most items in this response; capped by the configuration."},
    "cursor":{"type":"string","minLength":1,"description":"Continue where the previous response stopped; use the cursor it gave. Not with path, filters or status."}
  },
  "additionalProperties":false
}`)

const transcriptsDescription = "List the sub-agent runs of a profile, newest first, or the sessions. Each line gives the exact path to pass to transcript and the full messages.jsonl file path, with size and time of last activity."

// transcriptsKinds are the forms of path, as the error for a bad one lists them.
const transcriptsKinds = "sessions, session:<id>, profile:<category>/<name>, profile:<session>/<category>/<name>"

// TranscriptSessions names the sessions a listing may show to the caller (ownerKey, the
// session key of the calling run). The profile port finds the rest.
type TranscriptSessions interface {
	Sessions(ctx context.Context, ownerKey string) []string
}

// transcriptsLevel is what a call lists: the sessions, the profiles of a session (an empty
// session is the caller's) or the sub-agent runs of a profile.
type transcriptsLevel struct {
	kind    string // "sessions", "profiles" or "runs"
	session string
	profile string
}

// text is the path that selects this level; it is what a cursor carries.
func (l transcriptsLevel) text() string {
	switch l.kind {
	case "sessions":
		return "sessions"
	case "runs":
		if l.session == "" {
			return "profile:" + l.profile
		}
		return "profile:" + l.session + "/" + l.profile
	}
	if l.session == "" {
		return ""
	}
	return "session:" + l.session
}

// parseTranscriptsPath reads the path input; ok is false for anything but the listed forms.
func parseTranscriptsPath(path string) (transcriptsLevel, bool) {
	if path == "" {
		return transcriptsLevel{kind: "profiles"}, true
	}
	if path == "sessions" {
		return transcriptsLevel{kind: "sessions"}, true
	}
	kind, rest, found := strings.Cut(path, ":")
	if !found || rest == "" {
		return transcriptsLevel{}, false
	}
	parts, ok := splitTranscriptName(rest)
	if !ok {
		return transcriptsLevel{}, false
	}
	switch {
	case kind == "session" && len(parts) == 1:
		return transcriptsLevel{kind: "profiles", session: parts[0]}, true
	case kind == "profile" && len(parts) == 2:
		return transcriptsLevel{kind: "runs", profile: strings.Join(parts, "/")}, true
	case kind == "profile" && len(parts) == 3:
		return transcriptsLevel{kind: "runs", session: parts[0], profile: parts[1] + "/" + parts[2]}, true
	}
	return transcriptsLevel{}, false
}

// transcriptsCursor continues a listing after the item (Activity, Key): items are ordered by
// Activity descending then Key ascending, so the next page is whatever sorts after that pair
// whatever was added or touched since.
type transcriptsCursor struct {
	Level    string   `json:"l"`
	Filters  []string `json:"f,omitempty"`
	Status   string   `json:"s,omitempty"`
	Activity int64    `json:"a"`
	Key      string   `json:"k"`
}

func encodeTranscriptsCursor(cursor transcriptsCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeTranscriptsCursor(text string) (transcriptsCursor, bool) {
	var cursor transcriptsCursor
	raw, err := base64.RawURLEncoding.DecodeString(text)
	return cursor, err == nil && json.Unmarshal(raw, &cursor) == nil
}

type transcriptsTool struct {
	settings sessionfs.NativeTranscriptsSettings
	own      TranscriptFiles
	profiles TranscriptProfiles
	sessions TranscriptSessions
	runs     *SubagentManager
	remotes  remoteReader
}

// NewTranscriptsTool constructs native.transcripts. profiles, sessions and runs find what the
// own session does not hold and may be nil; the listing then shows what it can.
func NewTranscriptsTool(settings sessionfs.NativeTranscriptsSettings, own TranscriptFiles, profiles TranscriptProfiles, sessions TranscriptSessions, runs *SubagentManager) (tools.Tool, error) {
	return NewTranscriptsToolWithRemotes(settings, own, profiles, sessions, runs, nil)
}

// NewTranscriptsToolWithRemotes is NewTranscriptsTool plus the remotes folder; it lists a remote
// only when settings.AllowRemote is on and the name is in settings.Remotes.
func NewTranscriptsToolWithRemotes(settings sessionfs.NativeTranscriptsSettings, own TranscriptFiles, profiles TranscriptProfiles, sessions TranscriptSessions, runs *SubagentManager, remotes *remote.Directory) (tools.Tool, error) {
	validator, err := tools.CompileSchema(transcriptsInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native transcripts schema: %w", err)
	}
	tool := &transcriptsTool{settings: settings, own: own, profiles: profiles, sessions: sessions, runs: runs, remotes: remoteReader{allow: settings.AllowRemote, names: settings.Remotes, directory: remotes}}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          transcriptsToolID,
			Source:      "native",
			Name:        "transcripts",
			Description: transcriptsDescription,
			InputSchema: append(json.RawMessage(nil), transcriptsInputSchema...),
		},
		Validator:        validator,
		ReadOnly:         true,
		ExampleOperation: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return tool.execute(ctx, arguments, execution), nil
		},
	}, nil
}

func (t *transcriptsTool) fail(execution tools.ExecutionContext, status tools.Status, code, summary string, information ...tools.Information) string {
	return tools.ExecutionError(execution.CallID, status, code, summary, information...)
}

// transcriptsRequest is a call after its input was checked.
type transcriptsRequest struct {
	level   transcriptsLevel
	filters []toolSmithFilter
	texts   []string
	status  string
	limit   int
	after   *transcriptsCursor
}

func (t *transcriptsTool) execute(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) string {
	if execution.AgentDepth > 0 && !t.settings.AllowChildren {
		return t.fail(execution, tools.StatusForbidden, "transcripts_children_denied", "A sub-agent may not list transcripts: tools.native.transcripts.allow_children is false")
	}
	if name, _, wrapped := splitRemoteCursor(stringArgument(arguments, "cursor")); wrapped {
		return t.executeRemote(ctx, arguments, execution, name)
	}
	if path := stringArgument(arguments, "path"); strings.HasPrefix(path, remote.Prefix) {
		name, _, _ := remote.Split(path)
		return t.executeRemote(ctx, arguments, execution, name)
	}
	request, failure := t.parse(arguments, execution)
	if failure != "" {
		return failure
	}
	if failure = t.gate(request.level, execution); failure != "" {
		return failure
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.settings.TimeoutMs)*time.Millisecond)
	defer cancel()
	return t.list(ctx, request, execution)
}

func (t *transcriptsTool) parse(arguments map[string]any, execution tools.ExecutionContext) (transcriptsRequest, string) {
	var request transcriptsRequest
	path, _ := arguments["path"].(string)
	cursorText, hasCursor := arguments["cursor"].(string)
	status, _ := arguments["status"].(string)
	given, hasFilters := arguments["filters"].([]any)
	for _, item := range given {
		text, ok := item.(string)
		if !ok {
			return request, t.fail(execution, tools.StatusInvalidInput, "invalid_input", "filters must be strings")
		}
		if text = strings.ToLower(strings.TrimSpace(text)); text != "" {
			request.texts = append(request.texts, text)
			request.filters = append(request.filters, newToolSmithFilter(text))
		}
	}
	if hasCursor && (path != "" || hasFilters || status != "") {
		return request, t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "A cursor already names its listing, filters and status; give only the cursor (and limit)")
	}
	request.limit = int(t.settings.MaxEntries)
	if value, ok := safeInteger(arguments["limit"]); ok {
		request.limit = int(min(value, t.settings.MaxEntries))
	}
	if hasCursor {
		cursor, ok := decodeTranscriptsCursor(cursorText)
		if !ok {
			return request, t.fail(execution, tools.StatusInvalidInput, "invalid_cursor", "cursor is not a cursor this tool gave")
		}
		path, status, request.texts = cursor.Level, cursor.Status, cursor.Filters
		for _, text := range request.texts {
			request.filters = append(request.filters, newToolSmithFilter(text))
		}
		request.after = &cursor
	}
	level, ok := parseTranscriptsPath(path)
	if !ok {
		return request, t.fail(execution, tools.StatusInvalidInput, "invalid_path", "path is not a form this tool lists", tools.Information{Key: "valid", Value: transcriptsKinds})
	}
	if status != "" && level.kind != "runs" {
		return request, t.fail(execution, tools.StatusInvalidInput, "invalid_input", "status only applies to the sub-agent runs of a profile: give path profile:<category>/<name>")
	}
	request.level, request.status = level, status
	return request, ""
}

// gate refuses a level the switches do not allow. A session other than the caller's is
// judged by the name given, so an alias of the caller's own session is refused as well.
func (t *transcriptsTool) gate(level transcriptsLevel, execution tools.ExecutionContext) string {
	callerSession, callerProfile, _ := strings.Cut(execution.SessionKey, "\x00")
	otherSession := level.session != "" && level.session != callerSession
	if otherSession && !t.settings.AllowOtherSessions {
		return t.fail(execution, tools.StatusForbidden, "transcripts_session_denied", "Another session: tools.native.transcripts.allow_other_sessions is false")
	}
	if level.kind == "runs" && level.profile != callerProfile && !t.settings.AllowOtherProfiles {
		return t.fail(execution, tools.StatusForbidden, "transcripts_profile_denied", "Another profile: tools.native.transcripts.allow_other_profiles is false")
	}
	return ""
}
