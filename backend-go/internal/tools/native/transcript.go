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

	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const transcriptToolID = "native.transcript"

var transcriptInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","minLength":1,"description":"Which transcript to read; omit it for your own (a sub-agent reads its own run). profile:<category>/<name> is another profile in this session and profile:<session>/<category>/<name> one in another session; subagent:<run id> (sub_xxxxxxxx) is a run of your profile, subagent:<category>/<name>/<run id> or subagent:<session>/<category>/<name>/<run id> a run of another profile; path:<file> is a file (only when tools.native.transcript.allow_path is true). remote:<name>/ before a profile: or subagent: path that names its session reads another JAD server (only when allow_remote is on; no file paths come back). The header of a read and a not-found error give the exact path to reuse."},
    "turns":{"type":"string","minLength":1,"description":"List mode: which turns to list: 3, 3-7, -5 (the last five) or 12- (from turn 12). A turn is a user message and the answer to it. Default: every turn, a page at a time."},
    "source":{"type":"string","enum":["messages","backup"],"description":"messages (default) is the live transcript; backup is messages.backup.jsonl, the history before the last compaction."},
    "role":{"type":"string","enum":["user","assistant","tool"],"description":"List mode: only steps of this role; tool is a tool call (with its result)."},
    "tool":{"type":"string","minLength":1,"description":"List mode: only calls of tools whose name matches; * and ? are wildcards (read, exec*)."},
    "search":{"type":"string","minLength":1,"description":"List mode: only steps whose whole text (a call's arguments and result too) contains this, case-insensitive; the line shows only the preview."},
    "context":{"type":"integer","minimum":0,"description":"With search: how many steps to list on each side of a match. Default 0."},
    "step":{"type":"string","minLength":1,"description":"Detail mode: read one step whole, 3.2 (turn 3, step 2), or a range 3.1-3.4, which comes back in transcript order, starting at its oldest step. Step numbers come from the list, which starts at the newest step. Not with turns, role, tool, search or context."},
    "part":{"type":"string","enum":["call","result","both","reasoning"],"description":"With step: both (default) is a tool call and its result and the text of a message; call is the call and messages only, result the results only, reasoning the reasoning steps only (they are never listed)."},
    "cursor":{"type":"string","minLength":1,"description":"Continue where the previous response stopped; use the cursor it gave. Not with turns, step or part."}
  },
  "additionalProperties":false
}`)

const transcriptDescription = "Read a messages.jsonl transcript: your own by default, or the one path names. Two modes. Without step it lists one short line per step (turn.step, e.g. 3.2), newest step first: messages cut to a preview, tool calls with their arguments cut, ok or error and the result size; results and reasoning are not listed. With step it returns that step whole, paged by cursor."

var transcriptRejectedProperties = map[string]string{
	"offset":   "transcript has no offset; select by turns or cursor",
	"tail":     "transcript has no tail; the list is newest first, so read the first page, or use turns -N for the last N turns",
	"limit":    "transcript has no limit; the response is capped by the configuration, use turns or cursor",
	"full":     "transcript has no full; give step (3.2 or 3.1-3.4) to read a step whole",
	"list":     "transcript has no list; a not-found error names the available profiles and sub-agent runs, and path selects one: profile:<category>/<name>, subagent:<run id> or path:<file>",
	"profile":  "transcript has no profile; use path \"profile:<category>/<name>\"",
	"session":  "transcript has no session; use path \"profile:<session>/<category>/<name>\"",
	"subagent": "transcript has no subagent; use path \"subagent:<run id>\"",
}

// TranscriptFiles are the transcript files of the profile the tool is built for.
type TranscriptFiles struct {
	Messages string
	Backup   string
	// OtherSession is set when the transcript belongs to another session than the caller.
	OtherSession bool
}

// TranscriptTarget is a profile transcript the source found: the canonical session reference
// and profile ID, and the files.
type TranscriptTarget struct {
	Session string
	Profile string
	Files   TranscriptFiles
	// SubagentDir is the profile's sub-agent storage directory, empty when it has none; Runs is
	// its live manager, nil when none is running.
	SubagentDir string
	Runs        *SubagentManager
	// PersistRuns is whether the profile writes a manifest per run; without it a finished
	// run is known only to the server that holds it.
	PersistRuns bool
	// MayAgent, when set, limits which sub-agent runs of this profile are visible by agent
	// name; a run it refuses looks exactly like one that does not exist.
	MayAgent func(agent string) bool
}

// TranscriptProfiles finds the transcript of a profile of a session (an empty session is the
// caller's own, an empty profile the caller's) for the caller ownerKey, the session key
// "<reference>\x00<profile>". Without one, only the own transcript can be read.
type TranscriptProfiles interface {
	Profile(ctx context.Context, ownerKey, session, profile string) (TranscriptTarget, error)
	// Candidates names what a failed Profile could have meant: the profiles of the session
	// (an empty session is the caller's) or, when that session cannot be read, the sessions.
	Candidates(ctx context.Context, ownerKey, session string) (profiles, sessions []string)
}

type transcriptTool struct {
	settings sessionfs.NativeTranscriptSettings
	own      TranscriptFiles
	profiles TranscriptProfiles
	runs     *SubagentManager
	remotes  remoteReader
}

// NewTranscriptTool constructs native.transcript. own is the transcript of the session and
// profile the catalog is built for; profiles and runs find the others and may be nil.
func NewTranscriptTool(settings sessionfs.NativeTranscriptSettings, own TranscriptFiles, profiles TranscriptProfiles, runs *SubagentManager) (tools.Tool, error) {
	return NewTranscriptToolWithRemotes(settings, own, profiles, runs, nil)
}

// NewTranscriptToolWithRemotes is NewTranscriptTool plus the remotes folder; it reads a remote
// only when settings.AllowRemote is on and the name is in settings.Remotes.
func NewTranscriptToolWithRemotes(settings sessionfs.NativeTranscriptSettings, own TranscriptFiles, profiles TranscriptProfiles, runs *SubagentManager, remotes *remote.Directory) (tools.Tool, error) {
	validator, err := tools.CompileSchema(transcriptInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native transcript schema: %w", err)
	}
	tool := &transcriptTool{settings: settings, own: own, profiles: profiles, runs: runs, remotes: remoteReader{allow: settings.AllowRemote, names: settings.Remotes, directory: remotes}}
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
		ExampleOperation:   json.RawMessage(`{"turns":"-2"}`),
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return tool.execute(ctx, arguments, execution), nil
		},
	}, nil
}

type transcriptCursor struct {
	Selector string `json:"x"`
	Source   string `json:"s"`
	Identity string `json:"i"`
	Offset   int64  `json:"o"`
	End      int64  `json:"e"`
	// From is the newest step still to list (list mode; the list goes toward older steps) or the first step of the detail range;
	// To, Part and Bytes are set only in detail mode (Bytes: how far into its text).
	From  string `json:"f,omitempty"`
	To    string `json:"t,omitempty"`
	Part  string `json:"p,omitempty"`
	Bytes int64  `json:"b,omitempty"`
}

func encodeTranscriptCursor(cursor transcriptCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeTranscriptCursor(text string) (transcriptCursor, bool) {
	var cursor transcriptCursor
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Offset < 0 || cursor.Bytes < 0 || cursor.End < cursor.Offset {
		return transcriptCursor{}, false
	}
	return cursor, true
}

// transcriptRange selects turns by number: from..to inclusive, or, for -N, the last tail turns.
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
	turnsText, hasTurns := arguments["turns"].(string)
	cursorText, hasCursor := arguments["cursor"].(string)
	if name, _, wrapped := splitRemoteCursor(cursorText); wrapped && hasCursor {
		return t.executeRemote(ctx, arguments, execution, name)
	}
	if selector, _ := arguments["path"].(string); strings.HasPrefix(selector, remote.Prefix) {
		name, _, _ := remote.Split(selector)
		return t.executeRemote(ctx, arguments, execution, name)
	}
	if hasTurns && hasCursor {
		return t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "Give only one of turns and cursor")
	}
	stepText, hasStep := arguments["step"].(string)
	part, hasPart := arguments["part"].(string)
	if hasStep || hasPart {
		if hasCursor {
			return t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "A cursor already names its step; do not give step or part with it")
		}
		for _, key := range []string{"turns", "role", "tool", "search", "context"} {
			if _, given := arguments[key]; given {
				return t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "step reads one step whole; it does not take "+key)
			}
		}
		if !hasStep {
			return t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "part needs step")
		}
	}
	var stepFrom, stepTo transcriptStepID
	if hasStep {
		var ok bool
		if stepFrom, stepTo, ok = parseTranscriptSteps(stepText); !ok {
			return t.fail(execution, tools.StatusInvalidInput, "invalid_step", "step must be turn.step, like 3.2, or a range like 3.1-3.4", tools.Information{Key: "step", Value: stepText})
		}
	}
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
	selector, _ := arguments["path"].(string)
	if hasCursor && selector != "" {
		return t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "A cursor already names its transcript; do not give path with it")
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
		if cursor.To != "" {
			var okFrom, okTo bool
			stepFrom, okFrom = parseTranscriptStepID(cursor.From)
			stepTo, okTo = parseTranscriptStepID(cursor.To)
			if !okFrom || !okTo {
				return t.fail(execution, tools.StatusInvalidInput, "invalid_cursor", "cursor is not a cursor this tool gave")
			}
			hasStep, part = true, cursor.Part
		}
	}
	if part == "" {
		part = "both"
	}
	if source == "" {
		source = "messages"
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.settings.TimeoutMs)*time.Millisecond)
	defer cancel()
	files, canonical, failure := t.resolve(ctx, selector, source, execution)
	if failure != "" {
		return failure
	}
	selector = canonical
	label := ""
	if canonical != "" {
		label = canonical + ": "
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
	turns, complete, err := transcriptIndexes.turns(ctx, file, path, identity, size, t.settings.LineMaxBytes, int(t.settings.IndexCacheEntries))
	if err != nil {
		return t.scanFailure(execution, err)
	}
	if len(turns) == 0 {
		return t.render(execution, "transcript is empty", path, nil, "", false)
	}

	if hasStep {
		var offset int64
		if hasCursor {
			if cursor.Identity != identity {
				return t.fail(execution, tools.StatusError, "cursor_stale", "The transcript changed (compaction or reset) since this cursor was made; repeat the call without cursor")
			}
			offset = cursor.Bytes
		}
		if part == "reasoning" && files.OtherSession && !t.settings.AllowOtherReasoning {
			return t.fail(execution, tools.StatusForbidden, "reasoning_denied", "Another session's reasoning is not shown: tools.native.transcript.allow_other_reasoning is false")
		}
		return t.detail(transcriptRun{ctx: ctx, execution: execution, file: file, turns: turns, complete: complete, identity: identity, selector: selector, source: source, path: path, label: label},
			stepFrom, stepTo, part, offset)
	}

	var startOff, endOff int64
	var first, last int
	var resume transcriptStepID
	hasResume := false
	if hasCursor {
		if cursor.Identity != identity || cursor.End > complete {
			return t.fail(execution, tools.StatusError, "cursor_stale", "The transcript changed (compaction or reset) since this cursor was made; repeat the call without cursor")
		}
		startOff, endOff = cursor.Offset, cursor.End
		first = max(sort.Search(len(turns), func(i int) bool { return turns[i].off > startOff })-1, 0)
		last = sort.Search(len(turns), func(i int) bool { return turns[i].off >= endOff }) - 1
		resume, hasResume = parseTranscriptStepID(cursor.From)
	} else {
		selected := transcriptRange{from: 1, to: 1<<62 - 1}
		switch {
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
		startOff = turns[first].off
		endOff = complete
		if last+1 < len(turns) {
			endOff = turns[last+1].off
		}
	}

	list := transcriptList{tool: t, ctx: ctx, file: file, turns: turns, first: first, last: last, endOff: endOff,
		search: strings.ToLower(search), role: role, toolFilter: toolFilter, context: int(contextValue), resume: resume, hasResume: hasResume}
	selected, lastTurn, err := list.steps()
	if err != nil {
		return t.scanFailure(execution, err)
	}
	var lines []string
	var used int64
	var next *transcriptStep
	for index := len(selected) - 1; index >= 0; index-- {
		step := selected[index]
		line := stepLine(step)
		if len(lines) > 0 && (int64(len(lines)) >= t.settings.MaxListEntries || used+int64(len(line))+1 > t.settings.ResponseMaxBytes) {
			next = step
			break
		}
		lines = append(lines, line)
		used += int64(len(line)) + 1
	}
	header := label + fmt.Sprintf("turns %d-%d; the transcript holds turns %d-%d", turns[first].number, turns[min(lastTurn, len(turns)-1)].number, turns[0].number, turns[len(turns)-1].number)
	if turns[0].number != 1 {
		header += ": it was compacted, its earlier turns may be in the backup"
	}
	header += "; newest step first; steps are turn.step in file order, N.0 is the user message, gaps are reasoning"
	if len(lines) == 0 {
		header += "; no step matches"
	}
	nextCursor := ""
	if next != nil {
		nextCursor = encodeTranscriptCursor(transcriptCursor{Selector: selector, Source: source, Identity: identity, Offset: startOff, End: endOff, From: next.id.String()})
	}
	return t.render(execution, header, path, lines, nextCursor, next != nil)
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

func (t *transcriptTool) render(execution tools.ExecutionContext, summary, path string, entries []string, cursor string, cut bool) string {
	text := strings.Join(entries, "\n")
	if int64(len(text)) > t.settings.ResponseMaxBytes {
		text = midCut(text, int(t.settings.ResponseMaxBytes))
		cut = true
	}
	var information []tools.Information
	if !t.settings.HidePaths {
		information = append(information, tools.Information{Key: "transcript", Value: path})
	}
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
