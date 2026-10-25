package native

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
    "turns":{"type":"string","minLength":1,"description":"Which turns to read: 3, 3-7, -5 (the last five) or 12- (from turn 12). A turn is a user message and the answer to it. Without turns, tail or cursor, the last turn is read."},
    "tail":{"type":"integer","minimum":1,"description":"Read the last N turns; the same as turns -N. Not with turns or cursor."},
    "full":{"type":"boolean","description":"Do not cut the text of one entry (the whole response is still capped)."},
    "cursor":{"type":"string","minLength":1,"description":"Continue where the previous response stopped; use the cursor it gave. Not with turns or tail."}
  },
  "additionalProperties":false
}`)

const transcriptDescription = "Read the messages.jsonl transcript of this session as text: user and assistant messages, tool calls and their results, by turn. Reasoning is not shown. Use turns or tail to choose which turns, cursor to continue a cut response, list to see which transcript files exist."

var transcriptRejectedProperties = map[string]string{
	"offset": "transcript has no offset; select by turns, tail or cursor",
	"limit":  "transcript has no limit; the response is capped by the configuration, use turns, tail or cursor",
}

// TranscriptFiles are the transcript files of the profile the tool is built for.
type TranscriptFiles struct {
	Messages string
	Backup   string
}

type transcriptTool struct {
	settings sessionfs.NativeTranscriptSettings
	own      TranscriptFiles
}

// NewTranscriptTool constructs native.transcript. own is the transcript of the session and
// profile the catalog is built for.
func NewTranscriptTool(settings sessionfs.NativeTranscriptSettings, own TranscriptFiles) (tools.Tool, error) {
	validator, err := tools.CompileSchema(transcriptInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native transcript schema: %w", err)
	}
	tool := &transcriptTool{settings: settings, own: own}
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

type transcriptCursor struct {
	Identity string `json:"i"`
	Offset   int64  `json:"o"`
	End      int64  `json:"e"`
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

	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.settings.TimeoutMs)*time.Millisecond)
	defer cancel()
	file, err := os.Open(t.own.Messages)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return t.fail(execution, tools.StatusNotFound, "no_transcript", "This session has no transcript yet")
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
		cursor, ok := decodeTranscriptCursor(cursorText)
		if !ok {
			return t.fail(execution, tools.StatusInvalidInput, "invalid_cursor", "cursor is not a cursor this tool gave")
		}
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
	var entries []string
	var used int64
	next := int64(-1)
	turnIndex := first
	err = scanTranscript(ctx, file, startOff, endOff, t.settings.LineMaxBytes, func(line transcriptLine) (bool, error) {
		for turnIndex+1 < len(turns) && turns[turnIndex+1].off <= line.off {
			turnIndex++
		}
		number := turns[turnIndex].number
		var rendered []string
		if line.tooLong {
			rendered = []string{fmt.Sprintf("#%d [line of %d bytes skipped: over tools.native.transcript.line_max_bytes]", number, line.length)}
		} else {
			rendered = renderTranscriptRecord(line.data, number, limit)
		}
		var size int64
		for _, entry := range rendered {
			size += int64(len(entry)) + 1
		}
		if len(entries) > 0 && used+size > t.settings.ResponseMaxBytes {
			next = line.off
			return false, nil
		}
		entries = append(entries, rendered...)
		used += size
		return true, nil
	})
	if err != nil {
		return t.scanFailure(execution, err)
	}
	header := fmt.Sprintf("turns %d-%d of %d", turns[first].number, turns[min(turnIndex, len(turns)-1)].number, len(turns))
	if turns[0].number != 1 {
		header += fmt.Sprintf("; the first turn is %d: the transcript was compacted, its earlier turns may be in the backup", turns[0].number)
	}
	cursor := ""
	if next >= 0 {
		cursor = encodeTranscriptCursor(transcriptCursor{Identity: identity, Offset: next, End: endOff})
	} else if endOff < complete {
		cursor = encodeTranscriptCursor(transcriptCursor{Identity: identity, Offset: endOff, End: complete})
	}
	return t.render(execution, header, entries, cursor, next >= 0)
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
