package native

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

const (
	ripgrepMaxStderrBytes = 8 * 1024
	ripgrepKillGrace      = 250 * time.Millisecond
	ripgrepKillDeadline   = time.Second
	ripgrepDrainDeadline  = 500 * time.Millisecond
	maxDiagnosticChars    = 200
)

type ripgrepEntryKind string

const (
	ripgrepMatch   ripgrepEntryKind = "match"
	ripgrepContext ripgrepEntryKind = "context"
)

type ripgrepEntry struct {
	kind ripgrepEntryKind
	line int64
	text string
}

type ripgrepGroup struct {
	path    string
	entries []ripgrepEntry
	matches int
}

type ripgrepFailure struct {
	kind    string
	message string
}

type ripgrepResult struct {
	groups              []ripgrepGroup
	matches             int
	textBytes           int64
	truncated           bool
	truncationReason    string
	failure             *ripgrepFailure
	binarySkipped       int
	perFileLimitReached bool
}

type ripgrepEvent struct {
	Type string `json:"type"`
	Data struct {
		Path *struct {
			Text *string `json:"text"`
		} `json:"path"`
		Lines *struct {
			Text *string `json:"text"`
		} `json:"lines"`
		LineNumber json.Number `json:"line_number"`
	} `json:"data"`
}

func runRipgrep(
	ctx context.Context,
	settings sessionfs.NativeGrepSettings,
	flags []string,
	pattern string,
	paths []string,
	cwd string,
) ripgrepResult {
	if ctx == nil {
		ctx = context.Background()
	}
	arguments := []string{
		"--json", "--no-config", "--color", "never", "--sort", "path",
		"--max-count", strconv.FormatInt(settings.MaxMatchesPerFile, 10),
		"--max-filesize", strconv.FormatInt(settings.MaxFileBytes, 10),
	}
	arguments = append(arguments, flags...)
	arguments = append(arguments, "--", pattern)
	arguments = append(arguments, paths...)

	command := exec.Command(settings.Binary, arguments...)
	command.Dir = cwd
	command.Env = environmentWithoutServerToken()
	configureProcessGroup(command)

	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return ripgrepResult{failure: &ripgrepFailure{kind: "unavailable", message: firstLine(err.Error())}}
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return ripgrepResult{failure: &ripgrepFailure{kind: "unavailable", message: firstLine(err.Error())}}
	}
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	if err := command.Start(); err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		_ = stderrReader.Close()
		_ = stderrWriter.Close()
		message := err.Error()
		if errors.Is(err, exec.ErrNotFound) {
			message = "spawn " + settings.Binary + " ENOENT"
		}
		return ripgrepResult{failure: &ripgrepFailure{kind: "unavailable", message: firstLine(message)}}
	}
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()

	var result ripgrepResult
	var resultMu sync.Mutex
	setTruncated := func(reason string) {
		resultMu.Lock()
		defer resultMu.Unlock()
		if !result.truncated {
			result.truncated = true
			result.truncationReason = reason
		}
	}
	stopRequests := make(chan string, 1)
	var stopRequestOnce sync.Once
	requestStop := func(reason string) {
		setTruncated(reason)
		stopRequestOnce.Do(func() { stopRequests <- reason })
	}

	stdoutDone := make(chan struct{})
	go func() {
		readRipgrepEvents(stdoutReader, settings, &result, requestStop)
		_ = stdoutReader.Close()
		close(stdoutDone)
	}()
	stderrTextChannel := make(chan string, 1)
	stderrDoneChannel := make(chan struct{})
	go func() {
		stderrTextChannel <- readBoundedAndDrain(stderrReader, ripgrepMaxStderrBytes)
		_ = stderrReader.Close()
		close(stderrDoneChannel)
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()

	var waitErr error
	waitDoneChannel := (<-chan error)(waitDone)
	stdoutDoneChannel := (<-chan struct{})(stdoutDone)
	stderrDoneSignal := (<-chan struct{})(stderrDoneChannel)

	timeout := time.NewTimer(time.Duration(settings.TimeoutMs) * time.Millisecond)
	defer timeout.Stop()
	timeoutChannel := (<-chan time.Time)(timeout.C)
	contextChannel := ctx.Done()
	var deadlineTimer *time.Timer
	var deadlineChannel <-chan time.Time
	cleanupStarted := false
	forceFinish := false
	startCleanup := func(reason string) {
		if reason != "" {
			setTruncated(reason)
		}
		if cleanupStarted {
			return
		}
		cleanupStarted = true
		signalProcessGroup(command, false)
		time.AfterFunc(ripgrepKillGrace, func() { signalProcessGroup(command, true) })
		deadline := ripgrepDrainDeadline
		if reason != "" {
			deadline = ripgrepKillDeadline
		}
		deadlineTimer = time.NewTimer(deadline)
		deadlineChannel = deadlineTimer.C
	}

	for waitDoneChannel != nil || stdoutDoneChannel != nil || stderrDoneSignal != nil {
		select {
		case waitErr = <-waitDoneChannel:
			waitDoneChannel = nil
			startCleanup("")
		case <-stdoutDoneChannel:
			stdoutDoneChannel = nil
		case <-stderrDoneSignal:
			stderrDoneSignal = nil
		case reason := <-stopRequests:
			startCleanup(reason)
		case <-contextChannel:
			startCleanup("timeout")
			contextChannel = nil
		case <-timeoutChannel:
			startCleanup("timeout")
			timeoutChannel = nil
		case <-deadlineChannel:
			forceFinish = true
			_ = stdoutReader.Close()
			_ = stderrReader.Close()
			deadlineChannel = nil
		}
		if forceFinish {
			break
		}
	}
	if deadlineTimer != nil {
		deadlineTimer.Stop()
	}
	_ = stdoutReader.Close()
	_ = stderrReader.Close()
	<-stdoutDone
	stderrText := <-stderrTextChannel

	resultMu.Lock()
	truncated, truncationReason := result.truncated, result.truncationReason
	resultMu.Unlock()
	if !truncated || truncationReason != "timeout" {
		if code, ok := exitCode(waitErr); ok && code > 1 {
			result.failure = ripgrepFailureForExit(code, stderrText)
		}
	}
	return result
}

func readRipgrepEvents(reader io.Reader, settings sessionfs.NativeGrepSettings, result *ripgrepResult, requestStop func(string)) {
	buffered := bufio.NewReader(reader)
	groupByPath := make(map[string]int)
	stopped := false
	for {
		line, err := buffered.ReadBytes('\n')
		if err != nil {
			// Match the TypeScript JSON-lines reader: an unterminated tail is ignored.
			return
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		if stopped {
			continue
		}
		var event ripgrepEvent
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		if event.Type != "match" && event.Type != "context" {
			continue
		}
		if event.Data.Path == nil || event.Data.Path.Text == nil || *event.Data.Path.Text == "" {
			if event.Type == "match" {
				result.binarySkipped++
			}
			continue
		}
		if event.Data.Lines == nil || event.Data.Lines.Text == nil {
			if event.Type == "match" {
				result.binarySkipped++
			}
			continue
		}
		path := strings.TrimPrefix(*event.Data.Path.Text, "./")
		kind := ripgrepContext
		if event.Type == "match" {
			kind = ripgrepMatch
		}
		lineNumber := int64(1)
		if parsed, parseErr := strconv.ParseInt(event.Data.LineNumber.String(), 10, 64); parseErr == nil {
			lineNumber = parsed
		}
		lines := strings.Split(*event.Data.Lines.Text, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		for index, raw := range lines {
			if kind == ripgrepMatch {
				if result.matches >= int(settings.MaxResults) {
					requestStop("max_results")
					stopped = true
					break
				}
				result.matches++
			}
			text := truncateJSString(raw, int(settings.MaxLineChars))
			entryBytes := int64(len(text)) + 16
			if result.textBytes+entryBytes > settings.MaxBytes {
				requestStop("max_content_bytes")
				stopped = true
				break
			}
			result.textBytes += entryBytes
			groupIndex, exists := groupByPath[path]
			if !exists {
				groupIndex = len(result.groups)
				groupByPath[path] = groupIndex
				result.groups = append(result.groups, ripgrepGroup{path: path})
			}
			group := &result.groups[groupIndex]
			group.entries = append(group.entries, ripgrepEntry{kind: kind, line: lineNumber + int64(index), text: text})
			if kind == ripgrepMatch {
				group.matches++
				if group.matches >= int(settings.MaxMatchesPerFile) {
					result.perFileLimitReached = true
				}
			}
		}
	}
}

func readBoundedAndDrain(reader io.Reader, limit int) string {
	var kept bytes.Buffer
	buffer := make([]byte, 4096)
	for {
		count, err := reader.Read(buffer)
		if count > 0 && kept.Len() < limit {
			remaining := limit - kept.Len()
			if count > remaining {
				count = remaining
			}
			_, _ = kept.Write(buffer[:count])
		}
		if err != nil {
			return kept.String()
		}
	}
}

func environmentWithoutServerToken() []string {
	environment := os.Environ()
	safe := make([]string, 0, len(environment))
	for _, value := range environment {
		if !strings.HasPrefix(value, "AGENT_API_TOKEN=") {
			safe = append(safe, value)
		}
	}
	return safe
}

func exitCode(waitErr error) (int, bool) {
	if waitErr == nil {
		return 0, true
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		return exitError.ExitCode(), true
	}
	return 0, false
}

func ripgrepFailureForExit(code int, stderr string) *ripgrepFailure {
	diagnostic := firstDiagnostic(stderr)
	lower := strings.ToLower(stderr)
	if strings.Contains(lower, "regex parse error") || strings.Contains(lower, "error parsing regex") {
		return &ripgrepFailure{kind: "invalid_pattern", message: diagnostic}
	}
	if diagnostic == "" {
		diagnostic = fmt.Sprintf("ripgrep exited with %d", code)
	}
	return &ripgrepFailure{kind: "failed", message: diagnostic}
}

func firstLine(value string) string {
	line := strings.TrimSpace(strings.Split(value, "\n")[0])
	if utf16Length(line) > maxDiagnosticChars {
		return truncateJSString(line, maxDiagnosticChars) + "..."
	}
	return line
}

func firstDiagnostic(value string) string {
	lines := strings.Split(value, "\n")
	selected := make([]string, 0, 3)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		selected = append(selected, line)
		if len(selected) == 3 {
			break
		}
	}
	joined := strings.Join(selected, " / ")
	if utf16Length(joined) > maxDiagnosticChars {
		return truncateJSString(joined, maxDiagnosticChars) + "..."
	}
	return joined
}

func utf16Length(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func truncateJSString(value string, maxUnits int) string {
	units := utf16.Encode([]rune(value))
	if len(units) <= maxUnits {
		return value
	}
	return string(utf16.Decode(units[:maxUnits])) + " [... truncated line]"
}
