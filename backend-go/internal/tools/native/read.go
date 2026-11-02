package native

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const readToolID = "native.read"

var readInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","minLength":1,"description":"Path relative to cwd or an absolute path."},
    "offset":{"type":"integer","minimum":0,"description":"Start position: a 1-based line number, or a 0-based byte offset when rangeType is bytes. Values below the start mean the beginning. Defaults to the beginning."},
    "limit":{"type":"integer","minimum":1,"description":"Maximum lines or bytes to return. Defaults to the configured content limit."},
    "rangeType":{"type":"string","enum":["lines","bytes"],"description":"Unit used by offset and limit. Defaults to lines."},
    "contentEncoding":{"type":"string","enum":["utf8","base64"],"description":"utf8 for text, base64 for binary content. Defaults to the encoding that decodes the content."},
    "includeContent":{"type":"boolean","description":"When false, return file metadata without reading file content."}
  },
  "required":["path"],
  "additionalProperties":false
}`)

// NewReadTool constructs the native read tool with already-normalized session
// limits and path policy.
func NewReadTool(settings sessionfs.NativeReadSettings) (tools.Tool, error) {
	validator, err := tools.CompileSchema(readInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native read schema: %w", err)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          readToolID,
			Source:      "native",
			Name:        "read",
			Description: "Read file content or inspect a directory.",
			InputSchema: append(json.RawMessage(nil), readInputSchema...),
		},
		Validator:          validator,
		ReadOnly:           true,
		ExampleOperation:   json.RawMessage(`{"path":"."}`),
		NormalizeArguments: normalizeReadArguments,
		LeafBudget:         true,
		Execute: func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeRead(arguments, settings, execution)
		},
	}, nil
}

type readArgs struct {
	Path            string
	Offset          int64
	HasOffset       bool
	Limit           int64
	HasLimit        bool
	RangeType       string
	ContentEncoding string
	IncludeContent  bool
	HasInclude      bool
}

type lineSpan struct {
	start   int
	end     int
	fullEnd int
}

type utf8Span struct {
	start    int
	end      int
	next     int
	adjusted bool
}

func normalizeReadArguments(arguments map[string]any) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	if encoding, ok := normalizeEncoding(arguments["contentEncoding"]); ok {
		normalized["contentEncoding"] = encoding
	}
	if rangeType, ok := normalizeRangeType(arguments["rangeType"]); ok {
		normalized["rangeType"] = rangeType
	}
	if offset, ok := safeInteger(arguments["offset"]); ok {
		if offset < 0 {
			offset = 0
		}
		normalized["offset"] = json.Number(fmt.Sprint(offset))
	}
	if limit, ok := safeInteger(arguments["limit"]); ok {
		if limit < 1 {
			limit = 1
		}
		normalized["limit"] = json.Number(fmt.Sprint(limit))
	}
	return normalized
}

func normalizeEncoding(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	var normalized strings.Builder
	for _, character := range strings.ToLower(strings.TrimSpace(text)) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			normalized.WriteRune(character)
		}
	}
	result := normalized.String()
	if result == "utf8" {
		return "utf8", true
	}
	if result == "base64" {
		return "base64", true
	}
	return "", false
}

func normalizeRangeType(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	var normalized strings.Builder
	for _, character := range strings.ToLower(strings.TrimSpace(text)) {
		if character >= 'a' && character <= 'z' {
			normalized.WriteRune(character)
		}
	}
	switch normalized.String() {
	case "line", "lines":
		return "lines", true
	case "byte", "bytes":
		return "bytes", true
	default:
		return "", false
	}
}

func safeInteger(value any) (int64, bool) {
	var rational *big.Rat
	switch typed := value.(type) {
	case json.Number:
		var ok bool
		rational, ok = new(big.Rat).SetString(typed.String())
		if !ok {
			return 0, false
		}
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		if typed > 9_007_199_254_740_991 || typed < -9_007_199_254_740_991 || math.Trunc(typed) != typed {
			return 0, false
		}
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
	if !rational.IsInt() || !rational.Num().IsInt64() {
		return 0, false
	}
	integer := rational.Num().Int64()
	if integer > 9_007_199_254_740_991 || integer < -9_007_199_254_740_991 {
		return 0, false
	}
	return integer, true
}

func integerValue(value any) (int64, bool) {
	maxInt64 := int64(^uint64(0) >> 1)
	minInt64 := -maxInt64 - 1
	switch typed := value.(type) {
	case json.Number:
		rational, ok := new(big.Rat).SetString(typed.String())
		if !ok || !rational.IsInt() {
			return 0, false
		}
		number := rational.Num()
		if number.IsInt64() {
			return number.Int64(), true
		}
		if number.Sign() < 0 {
			return minInt64, true
		}
		return maxInt64, true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed {
			return 0, false
		}
		if typed >= float64(maxInt64) {
			return maxInt64, true
		}
		if typed <= float64(minInt64) {
			return minInt64, true
		}
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
}

func parseReadArgs(arguments map[string]any) (readArgs, error) {
	path, ok := arguments["path"].(string)
	if !ok {
		return readArgs{}, errors.New("path must be a string")
	}
	result := readArgs{Path: path, RangeType: "lines"}
	if offset, exists := arguments["offset"]; exists {
		value, ok := integerValue(offset)
		if !ok {
			return readArgs{}, errors.New("offset must be an integer")
		}
		result.Offset, result.HasOffset = value, true
	}
	if limit, exists := arguments["limit"]; exists {
		value, ok := integerValue(limit)
		if !ok {
			return readArgs{}, errors.New("limit must be an integer")
		}
		result.Limit, result.HasLimit = value, true
	}
	if rangeType, ok := arguments["rangeType"].(string); ok {
		result.RangeType = rangeType
	}
	if encoding, ok := arguments["contentEncoding"].(string); ok {
		result.ContentEncoding = encoding
	}
	if include, exists := arguments["includeContent"]; exists {
		value, ok := include.(bool)
		if !ok {
			return readArgs{}, errors.New("includeContent must be a boolean")
		}
		result.IncludeContent, result.HasInclude = value, true
	}
	return result, nil
}

func executeRead(arguments map[string]any, settings sessionfs.NativeReadSettings, execution tools.ExecutionContext) (string, error) {
	args, err := parseReadArgs(arguments)
	if err != nil {
		return "", err
	}
	target, pathFailure := resolveExistingTarget(args.Path, execution.WorkspaceRoot, settings.Policy, execution.CallID)
	if pathFailure != "" {
		return pathFailure, nil
	}
	before, err := os.Lstat(target.realPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return tools.ExecutionError(execution.CallID, tools.StatusNotFound, "not_found", "Could not find "+target.errorPath, tools.Information{Key: "path", Value: target.errorPath}), nil
		}
		return tools.ExecutionError(execution.CallID, tools.StatusError, "path_stat_failed", "Could not inspect the requested path", tools.Information{Key: "path", Value: target.errorPath}), nil
	}
	if before.IsDir() {
		limit := args.Limit
		if !args.HasLimit {
			limit = settings.DefaultLimit
		}
		return renderDirectory(execution.CallID, target, settings, limit)
	}
	if !before.Mode().IsRegular() {
		return tools.ExecutionError(execution.CallID, tools.StatusError, "unsupported_file_type", "The requested path is not a regular file or directory", tools.Information{Key: "path", Value: target.errorPath}), nil
	}
	if args.HasInclude && !args.IncludeContent {
		return renderFileMetadata(execution.CallID, target, before.Size())
	}
	if before.Size() > settings.MaxFileBytes {
		return tools.ExecutionError(execution.CallID, tools.StatusBlocked, "size_limit", "The file exceeds the configured read size limit",
			tools.Information{Key: "path", Value: target.errorPath},
			tools.Information{Key: "bytes", Value: before.Size()},
			tools.Information{Key: "max_bytes", Value: settings.MaxFileBytes},
			tools.Information{Key: "suggestion", Value: "read metadata with includeContent false, or have the session raise tools.native.read.max_file_bytes"}), nil
	}
	data, after, err := readVerifiedRegularFile(target.realPath, before, settings.MaxFileBytes)
	if err != nil {
		if strings.Contains(err.Error(), "past configured limit") {
			return tools.ExecutionError(execution.CallID, tools.StatusBlocked, "size_limit", "The file exceeds the configured read size limit",
				tools.Information{Key: "path", Value: target.errorPath},
				tools.Information{Key: "bytes", Value: after.Size()},
				tools.Information{Key: "max_bytes", Value: settings.MaxFileBytes}), nil
		}
		return tools.ExecutionError(execution.CallID, tools.StatusError, "changed_during_read", "The file changed while it was being read", tools.Information{Key: "path", Value: target.errorPath}), nil
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return tools.ExecutionError(execution.CallID, tools.StatusError, "changed_during_read", "The file changed while it was being read", tools.Information{Key: "path", Value: target.errorPath}), nil
	}
	requestedEncoding, _ := normalizeEncoding(args.ContentEncoding)
	validUTF8 := utf8.Valid(data)
	if requestedEncoding == "utf8" && !validUTF8 {
		return tools.ExecutionError(execution.CallID, tools.StatusUnsupportedEncoding, "binary_file", "Could not decode "+target.errorPath+" as UTF-8",
			tools.Information{Key: "path", Value: target.errorPath},
			tools.Information{Key: "suggestion", Value: "omit contentEncoding to receive the content as base64"}), nil
	}
	encoding := requestedEncoding
	if encoding == "" {
		if validUTF8 {
			encoding = "utf8"
		} else {
			encoding = "base64"
		}
	}
	encodingReason := ""
	if requestedEncoding == "" && encoding == "base64" {
		encodingReason = "invalid_utf8"
	}
	return renderFileRange(execution.CallID, target, data, args, settings, encoding, encodingReason)
}

func renderFileMetadata(callID string, target resolvedTarget, size int64) (string, error) {
	return tools.RenderToolResult(tools.ResultOptions{ID: callID, Status: tools.StatusOK, Summary: "Inspected file metadata", Information: []tools.Information{
		{Key: "path", Value: target.displayPath},
		{Key: "kind", Value: "file"},
		{Key: "size_bytes", Value: size},
		{Key: "metadata_only", Value: true},
	}})
}

func renderDirectory(callID string, target resolvedTarget, settings sessionfs.NativeReadSettings, limit int64) (string, error) {
	entries, err := os.ReadDir(target.realPath)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "directory_read_failed", "Could not inspect "+target.errorPath, tools.Information{Key: "path", Value: target.errorPath}), nil
	}
	visible := make([]fs.DirEntry, 0, len(entries))
	for _, entry := range entries {
		entryPath := filepath.Join(target.realPath, entry.Name())
		if pathPolicyError(entryPath, target.workspaceRoot, settings.Policy, callID) != "" {
			continue
		}
		visible = append(visible, entry)
	}
	// os.ReadDir sorts by filename, matching the stable ordering expected by
	// the directory result renderer.
	if limit < 0 {
		limit = 0
	}
	visibleCount := len(visible)
	if limit < int64(visibleCount) {
		visibleCount = int(limit)
	}
	items := make([]tools.Information, 0, visibleCount+4)
	usedBytes := 0
	truncated := len(visible) > visibleCount
	for _, entry := range visible[:visibleCount] {
		kind := "other"
		var sizeBytes *int64
		sizeUnavailable := false
		info, infoErr := entry.Info()
		if entry.IsDir() {
			kind = "directory"
		} else if infoErr == nil && info.Mode().IsRegular() {
			kind = "file"
			value := info.Size()
			sizeBytes = &value
		} else if infoErr != nil && entry.Type().IsRegular() {
			kind = "file"
			sizeUnavailable = true
		}
		entryValue := struct {
			Name            string `json:"name"`
			Kind            string `json:"kind"`
			SizeBytes       *int64 `json:"size_bytes,omitempty"`
			SizeUnavailable bool   `json:"size_unavailable,omitempty"`
		}{Name: entry.Name(), Kind: kind, SizeBytes: sizeBytes, SizeUnavailable: sizeUnavailable}
		encoded, err := tools.MarshalJSON(entryValue)
		if err != nil {
			return "", err
		}
		if usedBytes+len(encoded) > int(settings.MaxBytes) {
			truncated = true
			break
		}
		usedBytes += len(encoded)
		items = append(items, tools.Information{Key: "entry", Value: entryValue})
	}
	information := []tools.Information{
		{Key: "path", Value: target.displayPath},
		{Key: "kind", Value: "directory"},
		{Key: "entry_count", Value: len(items)},
		{Key: "directory_entries_truncated", Value: truncated},
	}
	information = append(information, items...)
	return tools.RenderToolResult(tools.ResultOptions{ID: callID, Status: tools.StatusOK, Summary: "Listed directory " + target.displayPath, Information: information})
}

func renderFileRange(callID string, target resolvedTarget, data []byte, args readArgs, settings sessionfs.NativeReadSettings, encoding, encodingReason string) (string, error) {
	rangeType := args.RangeType
	if rangeType == "" {
		rangeType = "lines"
	}
	limit := args.Limit
	if !args.HasLimit {
		limit = settings.DefaultLimit
	}
	first := int64(0)
	if args.HasOffset && args.Offset > 1 && rangeType == "lines" {
		first = args.Offset - 1
	}
	collect := int64(0)
	if rangeType == "lines" && limit > 0 {
		collect = limit
	}
	totalLines, spans := scanLines(data, first, collect, settings.MaxBytes)
	information := []tools.Information{
		{Key: "path", Value: target.displayPath},
		{Key: "kind", Value: "file"},
		{Key: "size_bytes", Value: len(data)},
		{Key: "encoding", Value: encoding},
	}
	if encodingReason != "" {
		information = append(information, tools.Information{Key: "encoding_reason", Value: encodingReason})
	}
	if ending := lineEnding(data); ending != "" {
		information = append(information, tools.Information{Key: "line_ending", Value: ending})
	}
	information = append(information,
		tools.Information{Key: "total_line_count", Value: totalLines},
		tools.Information{Key: "range_type", Value: rangeType},
	)
	if rangeType == "lines" {
		requestedOffset := int64(1)
		if args.HasOffset {
			requestedOffset = args.Offset
		}
		offset := requestedOffset
		if offset < 1 {
			offset = 1
		}
		offsetAdjusted := offset != requestedOffset
		offsetClamped := offset-1 > totalLines
		startIndex := totalLines
		if offset-1 < startIndex {
			startIndex = offset - 1
		}
		requestedCount := limit
		if requestedCount < 0 {
			requestedCount = 0
		}
		if requestedCount > totalLines-startIndex {
			requestedCount = totalLines - startIndex
		}
		requestedEnd := startIndex + requestedCount
		endIndex := startIndex
		returnedBytes := int64(0)
		contentTruncated := false
		lineExceedsLimit := false
		for endIndex < requestedEnd {
			span := spans[endIndex-first]
			spanBytes := int64(span.fullEnd - span.start)
			if returnedBytes+spanBytes > settings.MaxBytes {
				contentTruncated = true
				lineExceedsLimit = endIndex == startIndex
				break
			}
			returnedBytes += spanBytes
			endIndex++
		}
		remaining := endIndex < totalLines
		var selected []byte
		if endIndex > startIndex {
			selected = data[spans[startIndex-first].start:spans[endIndex-1-first].fullEnd]
		} else {
			selected = []byte{}
		}
		text := encodeContent(selected, encoding)
		information = append(information,
			tools.Information{Key: "returned_lines", Value: endIndex - startIndex},
			tools.Information{Key: "returned_bytes", Value: returnedBytes},
			tools.Information{Key: "next_offset", Value: optionalOffset(remaining, endIndex+1)},
			tools.Information{Key: "eof", Value: !remaining},
		)
		if offsetClamped || offsetAdjusted {
			information = append(information, tools.Information{Key: "offset_clamped", Value: true})
		}
		if contentTruncated {
			reason := "system_byte_limit"
			if lineExceedsLimit {
				reason = "line_exceeds_system_byte_limit"
			}
			information = append(information,
				tools.Information{Key: "content_truncated", Value: true},
				tools.Information{Key: "truncation_reason", Value: reason},
				tools.Information{Key: "max_content_bytes", Value: settings.MaxBytes},
			)
		}
		summary := "No content at EOF"
		if endIndex > startIndex {
			summary = fmt.Sprintf("Read lines %d-%d from %s", startIndex+1, endIndex, target.displayPath)
		} else if offsetClamped {
			summary = "No content; offset is at or beyond EOF"
		}
		var content *tools.Content
		if len(selected) > 0 {
			content = &tools.Content{Text: text, Encoding: encoding}
		}
		return tools.RenderToolResult(tools.ResultOptions{ID: callID, Status: tools.StatusOK, Summary: summary, Information: information, Content: content})
	}

	requestedOffset := int64(0)
	if args.HasOffset {
		requestedOffset = args.Offset
	}
	offset := requestedOffset
	if offset < 0 {
		offset = 0
	}
	offsetAdjusted := offset != requestedOffset
	offsetClamped := offset > int64(len(data))
	start := minInt64(offset, int64(len(data)))
	requestedEnd := boundedEnd(start, limit, int64(len(data)))
	cappedEnd := boundedEnd(start, settings.MaxBytes, requestedEnd)
	span := utf8Span{start: int(start), end: int(cappedEnd), next: int(cappedEnd)}
	if encoding == "utf8" {
		span = utf8Range(data, int(start), int(cappedEnd))
	}
	selected := data[span.start:span.end]
	limitedByPolicy := cappedEnd < requestedEnd || int64(span.end) < cappedEnd
	remaining := int64(span.next) < int64(len(data))
	information = append(information,
		tools.Information{Key: "returned_bytes", Value: len(selected)},
		tools.Information{Key: "next_offset", Value: optionalOffset(remaining, int64(span.next))},
		tools.Information{Key: "eof", Value: !remaining},
	)
	if offsetClamped || offsetAdjusted {
		information = append(information, tools.Information{Key: "offset_clamped", Value: true})
	}
	if span.adjusted {
		information = append(information,
			tools.Information{Key: "encoding_warning", Value: "partial_utf8"},
			tools.Information{Key: "requested_start_offset", Value: start},
			tools.Information{Key: "requested_end_offset", Value: cappedEnd},
			tools.Information{Key: "actual_start_offset", Value: span.start},
			tools.Information{Key: "actual_end_offset", Value: span.end},
		)
	}
	if limitedByPolicy {
		information = append(information,
			tools.Information{Key: "content_truncated", Value: true},
			tools.Information{Key: "truncation_reason", Value: "system_byte_limit"},
			tools.Information{Key: "max_content_bytes", Value: settings.MaxBytes},
		)
	}
	summary := "No content at EOF"
	if len(selected) > 0 {
		summary = fmt.Sprintf("Read bytes %d-%d from %s", span.start, span.end-1, target.displayPath)
	} else if offsetClamped {
		summary = "No content; offset is at or beyond EOF"
	}
	var content *tools.Content
	if len(selected) > 0 {
		content = &tools.Content{Text: encodeContent(selected, encoding), Encoding: encoding}
	}
	return tools.RenderToolResult(tools.ResultOptions{ID: callID, Status: tools.StatusOK, Summary: summary, Information: information, Content: content})
}

// scanLines counts the lines of data and returns the spans of the lines from index
// first on, at most maxCount of them and no more than needed to pass maxBytes. A
// caller indexes the result with the line index minus first.
func scanLines(data []byte, first, maxCount, maxBytes int64) (int64, []lineSpan) {
	var total int64
	var window []lineSpan
	var windowBytes int64
	add := func(span lineSpan) {
		if total >= first && int64(len(window)) < maxCount && windowBytes <= maxBytes {
			window = append(window, span)
			windowBytes += int64(span.fullEnd - span.start)
		}
		total++
	}
	start := 0
	for cursor := 0; cursor < len(data); {
		if data[cursor] != '\n' && data[cursor] != '\r' {
			cursor++
			continue
		}
		end := cursor
		fullEnd := cursor + 1
		if data[cursor] == '\r' && fullEnd < len(data) && data[fullEnd] == '\n' {
			fullEnd++
		}
		add(lineSpan{start: start, end: end, fullEnd: fullEnd})
		start = fullEnd
		cursor = fullEnd
	}
	if start < len(data) {
		add(lineSpan{start: start, end: len(data), fullEnd: len(data)})
	}
	return total, window
}

func lineEnding(data []byte) string {
	lf, crlf, cr := false, false, false
	for index := 0; index < len(data); index++ {
		switch data[index] {
		case '\n':
			if index > 0 && data[index-1] == '\r' {
				crlf = true
			} else {
				lf = true
			}
		case '\r':
			if index+1 >= len(data) || data[index+1] != '\n' {
				cr = true
			}
		}
	}
	count := 0
	for _, present := range []bool{lf, crlf, cr} {
		if present {
			count++
		}
	}
	if count == 0 {
		return ""
	}
	if count > 1 {
		return "mixed"
	}
	if crlf {
		return "crlf"
	}
	if cr {
		return "cr"
	}
	return "lf"
}

func utf8Range(data []byte, requestedStart, requestedEnd int) utf8Span {
	start := requestedStart
	for start < requestedEnd && isContinuationByte(data[start]) {
		start++
	}
	end := requestedEnd
	for end > start && !utf8.Valid(data[start:end]) {
		end--
	}
	next := requestedEnd
	for next < len(data) && isContinuationByte(data[next]) {
		next++
	}
	return utf8Span{start: start, end: end, next: next, adjusted: start != requestedStart || end != requestedEnd || next != requestedEnd}
}

func isContinuationByte(value byte) bool {
	return value&0xc0 == 0x80
}

func encodeContent(data []byte, encoding string) string {
	if encoding == "base64" {
		return base64.StdEncoding.EncodeToString(data)
	}
	return string(data)
}

func optionalOffset(remaining bool, offset int64) any {
	if !remaining {
		return nil
	}
	return offset
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func boundedEnd(start, limit, maximum int64) int64 {
	if limit <= 0 || start >= maximum {
		return start
	}
	remaining := maximum - start
	if limit >= remaining {
		return maximum
	}
	return start + limit
}
