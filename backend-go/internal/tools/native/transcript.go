package native

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const transcriptToolID = "native.transcript"

var transcriptInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "list":{"type":"boolean","description":"List the transcript files that can be read, with their size and time of last change, instead of reading."},
    "profile":{"type":"string","minLength":1,"description":"Read another profile's transcript, as <category>/<name>, in this session or in the session named by session. Not with subagent or path."},
    "session":{"type":"string","minLength":1,"description":"Session reference; only with profile."},
    "subagent":{"type":"string","pattern":"^sub_[0-9a-f]{8}$","description":"Read the transcript of a sub-agent run by its run id (sub_xxxxxxxx). Not with profile or path."},
    "path":{"type":"string","minLength":1,"description":"Read a transcript file by path; it must be a messages.jsonl transcript and pass the read path rules. Off unless tools.native.transcript.allow_path is true. Not with profile or subagent."},
    "turns":{"type":"string","minLength":1,"description":"Which turns to read: 3, 3-7, -5 (the last five) or 12- (from turn 12). A turn is a user message and the answer to it. Without turns, tail or cursor, the last turn is read."},
    "tail":{"type":"integer","minimum":1,"description":"Read the last N turns; the same as turns -N. Not with turns or cursor."},
    "source":{"type":"string","enum":["messages","backup"],"description":"messages (default) is the live transcript; backup is messages.backup.jsonl, the history before the last compaction."},
    "role":{"type":"string","enum":["user","assistant","tool"],"description":"Only entries of this role; tool is a tool call and its result."},
    "tool":{"type":"string","minLength":1,"description":"Only calls and results of tools whose name matches; * and ? are wildcards (read, exec*)."},
    "search":{"type":"string","minLength":1,"description":"Only entries whose text contains this, case-insensitive, with context entries around each."},
    "context":{"type":"integer","minimum":0,"description":"With search: how many entries to show on each side of a match. Default 0."},
    "full":{"type":"boolean","description":"Do not cut the text of one entry (the whole response is still capped)."},
    "cursor":{"type":"string","minLength":1,"description":"Continue where the previous response stopped; use the cursor it gave. Not with turns or tail."}
  },
  "additionalProperties":false
}`)

const transcriptDescription = "Read a messages.jsonl transcript as text, this session's own by default, or another profile's (profile, session), a sub-agent run's (subagent) or a file (path): user and assistant messages, tool calls and their results, by turn. Reasoning is not shown. Use turns or tail to choose which turns, role, tool and search to filter, source backup for the history before the last compaction, cursor to continue a cut response, list to see which transcript files exist."

var transcriptRejectedProperties = map[string]string{
	"offset": "transcript has no offset; select by turns, tail or cursor",
	"limit":  "transcript has no limit; the response is capped by the configuration, use turns, tail or cursor",
}

// TranscriptFiles are the transcript files of the profile the tool is built for.
type TranscriptFiles struct {
	Messages string
	Backup   string
}

// TranscriptTarget is a profile transcript the source found: the canonical session reference
// and profile ID, and the files.
type TranscriptTarget struct {
	Session string
	Profile string
	Files   TranscriptFiles
}

// TranscriptProfiles finds the transcript of a profile of a session (an empty session is the
// caller's own, an empty profile the caller's) for the caller ownerKey, the session key
// "<reference>\x00<profile>". Without one, only the own transcript can be read.
type TranscriptProfiles interface {
	Profile(ctx context.Context, ownerKey, session, profile string) (TranscriptTarget, error)
}

type transcriptTool struct {
	settings sessionfs.NativeTranscriptSettings
	own      TranscriptFiles
	profiles TranscriptProfiles
	runs     *SubagentManager
}

// NewTranscriptTool constructs native.transcript. own is the transcript of the session and
// profile the catalog is built for; profiles and runs find the others and may be nil.
func NewTranscriptTool(settings sessionfs.NativeTranscriptSettings, own TranscriptFiles, profiles TranscriptProfiles, runs *SubagentManager) (tools.Tool, error) {
	validator, err := tools.CompileSchema(transcriptInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native transcript schema: %w", err)
	}
	tool := &transcriptTool{settings: settings, own: own, profiles: profiles, runs: runs}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          transcriptToolID,
			Source:      "native",
			Name:        "transcript",
			Description: transcriptDescription,
			InputSchema: append(json.RawMessage(nil), transcriptInputSchema...),
		},
		Validator:          validator,
		ReadOnly:           true,
		RejectedProperties: transcriptRejectedProperties,
		ExampleOperation:   json.RawMessage(`{"tail":2}`),
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return tool.execute(ctx, arguments, execution), nil
		},
	}, nil
}

// transcriptSelector names a transcript other than the own one. A cursor carries it, so a
// page is checked again as the first call was.
type transcriptSelector struct {
	Profile  string `json:"p,omitempty"`
	Session  string `json:"s,omitempty"`
	Subagent string `json:"a,omitempty"`
	Path     string `json:"f,omitempty"`
}

type transcriptCursor struct {
	Selector transcriptSelector `json:"x"`
	Source   string             `json:"s"`
	Identity string             `json:"i"`
	Offset   int64              `json:"o"`
	End      int64              `json:"e"`
}

func encodeTranscriptCursor(cursor transcriptCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeTranscriptCursor(text string) (transcriptCursor, bool) {
	var cursor transcriptCursor
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Offset < 0 || cursor.End < cursor.Offset {
		return transcriptCursor{}, false
	}
	return cursor, true
}

// transcriptRange selects turns by number: from..to inclusive, or the last tail turns.
type transcriptRange struct {
	tail     int64
	from, to int64
}

func parseTranscriptRange(text string) (transcriptRange, bool) {
	text = strings.TrimSpace(text)
	number := func(s string) (int64, bool) {
		value, err := strconv.ParseInt(s, 10, 64)
		return value, err == nil && value > 0
	}
	if rest, ok := strings.CutPrefix(text, "-"); ok {
		value, valid := number(rest)
		return transcriptRange{tail: value}, valid
	}
	if rest, ok := strings.CutSuffix(text, "-"); ok {
		value, valid := number(rest)
		return transcriptRange{from: value, to: 1<<62 - 1}, valid
	}
	if from, to, ok := strings.Cut(text, "-"); ok {
		a, validA := number(from)
		b, validB := number(to)
		return transcriptRange{from: a, to: b}, validA && validB && a <= b
	}
	value, valid := number(text)
	return transcriptRange{from: value, to: value}, valid
}

func (t *transcriptTool) fail(execution tools.ExecutionContext, status tools.Status, code, summary string, information ...tools.Information) string {
	return tools.ExecutionError(execution.CallID, status, code, summary, information...)
}

func (t *transcriptTool) execute(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) string {
	if execution.AgentDepth > 0 && !t.settings.AllowChildren {
		return t.fail(execution, tools.StatusForbidden, "transcript_children_denied", "A sub-agent may not read transcripts: tools.native.transcript.allow_children is false")
	}
	if list, _ := arguments["list"].(bool); list {
		return t.list(execution)
	}
	turnsText, hasTurns := arguments["turns"].(string)
	tailValue, hasTail := safeInteger(arguments["tail"])
	cursorText, hasCursor := arguments["cursor"].(string)
	if hasTurns && (hasTail || hasCursor) || hasTail && hasCursor {
		return t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "Give only one of turns, tail and cursor")
	}
	full, _ := arguments["full"].(bool)
	source, _ := arguments["source"].(string)
	role, _ := arguments["role"].(string)
	search, _ := arguments["search"].(string)
	contextValue, _ := safeInteger(arguments["context"])
	var toolFilter *compiledGlob
	if pattern, _ := arguments["tool"].(string); pattern != "" {
		var err error
		if toolFilter, err = compileGlob(pattern); err != nil {
			return t.fail(execution, tools.StatusInvalidInput, "invalid_tool_pattern", "tool is not a valid pattern", tools.Information{Key: "tool", Value: pattern})
		}
	}
	selector, failure := t.selectorFrom(arguments, hasCursor, execution)
	if failure != "" {
		return failure
	}
	var cursor transcriptCursor
	if hasCursor {
		var ok bool
		if cursor, ok = decodeTranscriptCursor(cursorText); !ok {
			return t.fail(execution, tools.StatusInvalidInput, "invalid_cursor", "cursor is not a cursor this tool gave")
		}
		if source != "" && source != cursor.Source {
			return t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "source differs from the one the cursor was made for")
		}
		source = cursor.Source
		selector = cursor.Selector
	}
	if source == "" {
		source = "messages"
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.settings.TimeoutMs)*time.Millisecond)
	defer cancel()
	files, label, failure := t.resolve(ctx, selector, source, execution)
	if failure != "" {
		return failure
	}
	path := files.Messages
	if source == "backup" {
		path = files.Backup
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return t.fail(execution, tools.StatusNotFound, "no_transcript", "There is no "+source+" transcript yet")
		}
		return t.fail(execution, tools.StatusError, "transcript_unreadable", "The transcript could not be opened")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return t.fail(execution, tools.StatusError, "transcript_unreadable", "The transcript could not be inspected")
	}
	size := info.Size()
	if size > t.settings.ScanMaxBytes {
		return t.fail(execution, tools.StatusError, "transcript_too_large", "The transcript is larger than tools.native.transcript.scan_max_bytes",
			tools.Information{Key: "bytes", Value: size}, tools.Information{Key: "scan_max_bytes", Value: t.settings.ScanMaxBytes})
	}
	identity, err := transcriptIdentity(file, size)
	if err != nil {
		return t.fail(execution, tools.StatusError, "transcript_unreadable", "The transcript could not be read")
	}
	turns, complete, err := indexTranscriptTurns(ctx, file, size, t.settings.LineMaxBytes)
	if err != nil {
		return t.scanFailure(execution, err)
	}
	if len(turns) == 0 {
		return t.render(execution, "transcript is empty", nil, "", false)
	}

	var startOff, endOff int64
	var first, last int
	if hasCursor {
		if cursor.Identity != identity || cursor.End > complete {
			return t.fail(execution, tools.StatusError, "cursor_stale", "The transcript changed (compaction or reset) since this cursor was made; repeat the call without cursor")
		}
		startOff, endOff = cursor.Offset, cursor.End
		first = max(sort.Search(len(turns), func(i int) bool { return turns[i].off > startOff })-1, 0)
		last = sort.Search(len(turns), func(i int) bool { return turns[i].off >= endOff }) - 1
	} else {
		selected := transcriptRange{tail: 1}
		switch {
		case hasTail:
			selected = transcriptRange{tail: tailValue}
		case hasTurns:
			var ok bool
			if selected, ok = parseTranscriptRange(turnsText); !ok {
				return t.fail(execution, tools.StatusInvalidInput, "invalid_turns", "turns must be N, N-M, -N or N-", tools.Information{Key: "turns", Value: turnsText})
			}
		}
		if selected.tail > 0 {
			first = max(len(turns)-int(min(selected.tail, int64(len(turns)))), 0)
			last = len(turns) - 1
		} else {
			first = sort.Search(len(turns), func(i int) bool { return turns[i].number >= selected.from })
			last = sort.Search(len(turns), func(i int) bool { return turns[i].number > selected.to }) - 1
		}
		if first > last {
			return t.fail(execution, tools.StatusNotFound, "no_such_turns", fmt.Sprintf("The transcript has turns %d to %d", turns[0].number, turns[len(turns)-1].number))
		}
		if int64(last-first+1) > t.settings.MaxTurnsPerCall {
			last = first + int(t.settings.MaxTurnsPerCall) - 1
		}
		startOff = turns[first].off
		endOff = complete
		if last+1 < len(turns) {
			endOff = turns[last+1].off
		}
	}

	limit := int(t.settings.EntryMaxBytes)
	if full {
		limit = 0
	}
	var collected []transcriptEntry
	calls := map[string]string{}
	needle := strings.ToLower(search)
	turnIndex := first
	err = scanTranscript(ctx, file, startOff, endOff, t.settings.LineMaxBytes, func(line transcriptLine) (bool, error) {
		for turnIndex+1 < len(turns) && turns[turnIndex+1].off <= line.off {
			turnIndex++
		}
		number := turns[turnIndex].number
		if line.tooLong {
			collected = append(collected, transcriptEntry{turn: number, off: line.off, role: "unknown", text: fmt.Sprintf("#%d [line of %d bytes skipped: over tools.native.transcript.line_max_bytes]", number, line.length)})
			return true, nil
		}
		collected = append(collected, renderTranscriptRecord(line.data, number, line.off, limit, calls, needle)...)
		return true, nil
	})
	if err != nil {
		return t.scanFailure(execution, err)
	}
	selected := selectTranscriptEntries(collected, role, toolFilter, search != "", int(contextValue))
	var entries []string
	var used int64
	next := int64(-1)
	for index := 0; index < len(selected); {
		group := index
		var size int64
		for group < len(selected) && selected[group].off == selected[index].off {
			size += int64(len(selected[group].text)) + 1
			group++
		}
		if len(entries) > 0 && used+size > t.settings.ResponseMaxBytes {
			next = selected[index].off
			break
		}
		for _, entry := range selected[index:group] {
			entries = append(entries, entry.text)
		}
		used += size
		index = group
	}
	header := label + fmt.Sprintf("turns %d-%d; the transcript holds turns %d-%d", turns[first].number, turns[min(turnIndex, len(turns)-1)].number, turns[0].number, turns[len(turns)-1].number)
	if turns[0].number != 1 {
		header += ": it was compacted, its earlier turns may be in the backup"
	}
	nextCursor := ""
	if next >= 0 {
		nextCursor = encodeTranscriptCursor(transcriptCursor{Selector: selector, Source: source, Identity: identity, Offset: next, End: endOff})
	} else if endOff < complete {
		nextCursor = encodeTranscriptCursor(transcriptCursor{Selector: selector, Source: source, Identity: identity, Offset: endOff, End: complete})
	}
	return t.render(execution, header, entries, nextCursor, next >= 0)
}

func (t *transcriptTool) scanFailure(execution tools.ExecutionContext, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return t.fail(execution, tools.StatusError, "transcript_timeout", "Reading the transcript took longer than tools.native.transcript.timeout_ms")
	}
	if errors.Is(err, context.Canceled) {
		return t.fail(execution, tools.StatusError, "transcript_cancelled", "The call was cancelled")
	}
	return t.fail(execution, tools.StatusError, "transcript_unreadable", "The transcript could not be read")
}

func (t *transcriptTool) render(execution tools.ExecutionContext, summary string, entries []string, cursor string, cut bool) string {
	text := strings.Join(entries, "\n")
	if int64(len(text)) > t.settings.ResponseMaxBytes {
		text = cutMiddle(text, int(t.settings.ResponseMaxBytes))
		cut = true
	}
	var information []tools.Information
	if cursor != "" {
		information = append(information, tools.Information{Key: "cursor", Value: cursor})
	}
	if cut {
		information = append(information, tools.Information{Key: "truncated", Value: true})
	}
	var content *tools.Content
	if text != "" {
		content = &tools.Content{Encoding: "utf8", Text: text}
	}
	rendered, err := tools.RenderToolResult(tools.ResultOptions{ID: execution.CallID, Status: tools.StatusOK, Summary: summary, Information: information, Content: content})
	if err != nil {
		return t.fail(execution, tools.StatusError, "tool_output_failed", "The transcript could not be rendered")
	}
	return rendered
}

func (t *transcriptTool) list(execution tools.ExecutionContext) string {
	var lines []string
	for _, file := range []struct{ name, path string }{{"messages", t.own.Messages}, {"backup", t.own.Backup}} {
		if file.path == "" {
			continue
		}
		info, err := os.Stat(file.path)
		if err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s  %d bytes  %s", file.name, info.Size(), info.ModTime().UTC().Format(time.RFC3339)))
	}
	return t.render(execution, fmt.Sprintf("%d transcript files", len(lines)), lines, "", false)
}

// selectorFrom reads profile, session, subagent and path. They are mutually exclusive, and
// a cursor already names its transcript, so none may come with one.
func (t *transcriptTool) selectorFrom(arguments map[string]any, hasCursor bool, execution tools.ExecutionContext) (transcriptSelector, string) {
	var selector transcriptSelector
	selector.Profile, _ = arguments["profile"].(string)
	selector.Session, _ = arguments["session"].(string)
	selector.Subagent, _ = arguments["subagent"].(string)
	selector.Path, _ = arguments["path"].(string)
	var given []string
	for _, name := range []struct{ key, value string }{{"profile", selector.Profile}, {"subagent", selector.Subagent}, {"path", selector.Path}} {
		if name.value != "" {
			given = append(given, name.key)
		}
	}
	if len(given) > 1 {
		return selector, t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "Give only one of profile, subagent and path: got "+strings.Join(given, " and "))
	}
	if selector.Session != "" && selector.Profile == "" {
		return selector, t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "session needs profile: name the profile whose transcript to read")
	}
	if hasCursor && (len(given) > 0 || selector.Session != "") {
		return selector, t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "A cursor already names its transcript; do not give profile, session, subagent or path with it")
	}
	return selector, ""
}

// transcriptDenied reports whether the read path rules deny target: a deny rule covers it, or
// the call's scope does not reach it. Unlike a read it does not require the workspace, because
// the state of a session lives outside it.
func transcriptDenied(policy sessionfs.NativePathPolicy, target string) bool {
	for _, rule := range policy.Paths {
		if rule.Action == "deny" && sessionfs.PathWithin(rule.Path, target) {
			return true
		}
	}
	return len(policy.Within) > 0 && !withinAny(policy.Within, target)
}

// resolve finds the files of the selected transcript and checks that the caller may read them.
// label starts the header for a transcript other than the own one.
func (t *transcriptTool) resolve(ctx context.Context, selector transcriptSelector, source string, execution tools.ExecutionContext) (TranscriptFiles, string, string) {
	forbid := func(code, summary string) (TranscriptFiles, string, string) {
		return TranscriptFiles{}, "", t.fail(execution, tools.StatusForbidden, code, summary)
	}
	policy := scopedPolicy(t.settings.Policy, execution)
	callerSession, callerProfile, _ := strings.Cut(execution.SessionKey, "\x00")
	switch {
	case selector.Path != "":
		if source == "backup" {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "source backup does not apply to path: name the backup file itself")
		}
		if !t.settings.AllowPath {
			return forbid("transcript_path_disabled", "Reading a transcript by path is off: set tools.native.transcript.allow_path to true")
		}
		target, failure := resolveExistingTarget(selector.Path, execution.WorkspaceRoot, policy, execution.CallID)
		if failure != "" {
			return TranscriptFiles{}, "", failure
		}
		if !looksLikeTranscript(target.realPath, t.settings.LineMaxBytes) {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusInvalidInput, "not_a_transcript", "The file is not a messages.jsonl transcript", tools.Information{Key: "path", Value: target.displayPath})
		}
		return TranscriptFiles{Messages: target.realPath}, target.displayPath + ": ", ""
	case selector.Subagent != "":
		if source == "backup" {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "source backup does not apply to subagent: a sub-agent transcript has no backup")
		}
		if t.runs == nil {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusError, "subagent_unavailable", "Sub-agent runs are not available in this session")
		}
		dir, owner, found := t.runs.RunTranscript(selector.Subagent)
		if !found {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusNotFound, "no_such_run", "No sub-agent run "+selector.Subagent)
		}
		if owner != execution.SessionKey {
			if !t.settings.AllowChildren {
				return forbid("transcript_subagent_denied", "This run was not started by this session: tools.native.transcript.allow_children is false")
			}
			if ownerSession, _, qualified := strings.Cut(owner, "\x00"); qualified && ownerSession != callerSession && !t.settings.AllowOtherSessions {
				return forbid("transcript_session_denied", "This run belongs to another session: tools.native.transcript.allow_other_sessions is false")
			}
		}
		if dir == "" {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusNotFound, "no_transcript_yet", "Sub-agent run "+selector.Subagent+" has no transcript yet")
		}
		files := TranscriptFiles{Messages: filepath.Join(dir, "messages.jsonl")}
		if denied := t.deniedFile(files.Messages, policy); denied {
			return forbid("path_denied", "The sub-agent transcript is denied by the read path rules")
		}
		return files, "subagent " + selector.Subagent + ": ", ""
	case selector.Profile != "":
		if t.profiles == nil {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusError, "profile_unavailable", "Reading another profile's transcript is not available here")
		}
		target, err := t.profiles.Profile(ctx, execution.SessionKey, selector.Session, selector.Profile)
		if err != nil {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusNotFound, "no_such_profile", err.Error())
		}
		otherSession, otherProfile := target.Session != callerSession, target.Profile != callerProfile
		if otherSession && !t.settings.AllowOtherSessions {
			return forbid("transcript_session_denied", "Another session's transcript: tools.native.transcript.allow_other_sessions is false")
		}
		if otherProfile && !t.settings.AllowOtherProfiles {
			return forbid("transcript_profile_denied", "Another profile's transcript: tools.native.transcript.allow_other_profiles is false")
		}
		if (otherSession || otherProfile) && t.deniedFile(target.Files.Messages, policy) {
			return forbid("path_denied", "The transcript is denied by the read path rules")
		}
		return target.Files, target.Profile + " @ " + target.Session + ": ", ""
	}
	return t.own, "", ""
}

// deniedFile applies the read path rules to the file as it is on disk, symlinks resolved.
func (t *transcriptTool) deniedFile(path string, policy sessionfs.NativePathPolicy) bool {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		real = path
	}
	return transcriptDenied(policy, real)
}

// looksLikeTranscript reports whether path is a regular file whose first line is a JSON
// object with a string type, as every record of a messages.jsonl is.
func looksLikeTranscript(path string, maxLine int64) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	ok := false
	_ = scanTranscript(context.Background(), file, 0, info.Size(), maxLine, func(line transcriptLine) (bool, error) {
		var head struct {
			Type string `json:"type"`
		}
		ok = !line.tooLong && json.Unmarshal(line.data, &head) == nil && head.Type != ""
		return false, nil
	})
	return ok
}
