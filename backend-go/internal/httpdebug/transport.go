package httpdebug

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/logging"
)

const redactedValue = "[REDACTED]"

// bodyFileSuffix names the raw body file next to a request or response record.
const bodyFileSuffix = ".body"

var (
	sensitiveHeaderPattern = regexp.MustCompile(`(?i)(?:authorization|api[-_]?key|cookie|set-cookie|proxy-authorization)`)
	debugFilePattern       = regexp.MustCompile(`^(?:request|response)([0-9]+)$`)
)

// Settings controls opt-in HTTP diagnostics. Body capture is bounded before
// text is written, and request credentials are redacted from captured headers.
type Settings struct {
	// Logger receives the adapter's own failures (it writes debug files, not
	// log records). Nil discards them.
	Logger                 *slog.Logger
	Enabled                bool
	IncludeRequestHeaders  bool
	IncludeRequestBody     bool
	IncludeResponseHeaders bool
	IncludeResponseBody    bool
	MaxBodyBytes           int64
}

// NewTransport wraps next with best-effort provider HTTP logging. A nil next
// uses http.DefaultTransport; disabled settings return next unchanged.
func NewTransport(directory string, settings Settings, next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	if !settings.Enabled || settings.MaxBodyBytes < 1 {
		return next
	}
	return &Transport{
		next:     next,
		settings: settings,
		logger:   newFileLogger(directory, logging.Or(settings.Logger)),
	}
}

// Transport captures the same request/response records as the TypeScript
// backend's http_debug adapter without changing provider request semantics.
type Transport struct {
	next     http.RoundTripper
	settings Settings
	logger   *fileLogger
}

func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t == nil || t.next == nil {
		return nil, fmt.Errorf("HTTP debug transport is not initialized")
	}
	if request == nil {
		return nil, fmt.Errorf("HTTP debug request is nil")
	}
	requestID := t.logger.reserve()
	requestRecord := map[string]any{
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"direction": "request",
		"method":    strings.ToUpper(request.Method),
		"url":       request.URL.String(),
	}
	if t.settings.IncludeRequestHeaders {
		requestRecord["headers"] = requestHeaders(request.Header)
	}
	if t.settings.IncludeRequestBody {
		t.recordRequestBody(requestRecord, requestID, request)
	}
	t.logger.enqueue("request"+strconv.FormatInt(requestID, 10), requestRecord)

	response, err := t.next.RoundTrip(request)
	if err != nil {
		t.logger.enqueue("response"+strconv.FormatInt(requestID, 10), map[string]any{
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"direction": "response",
			"url":       request.URL.String(),
			"error":     err.Error(),
		})
		return response, err
	}
	responseRecord := map[string]any{
		"timestamp":   time.Now().UTC().Format(time.RFC3339Nano),
		"direction":   "response",
		"url":         responseURL(response, request),
		"status":      response.StatusCode,
		"status_text": responseStatusText(response),
	}
	if t.settings.IncludeResponseHeaders {
		responseRecord["headers"] = responseHeaders(response.Header)
	}
	if !t.settings.IncludeResponseBody {
		t.logger.enqueue("response"+strconv.FormatInt(requestID, 10), responseRecord)
		return response, nil
	}
	if response.Body == nil || response.Body == http.NoBody {
		responseRecord["body_unavailable"] = true
		t.logger.enqueue("response"+strconv.FormatInt(requestID, 10), responseRecord)
		return response, nil
	}
	name := "response" + strconv.FormatInt(requestID, 10)
	capture := &responseBodyCapture{
		body:     response.Body,
		logger:   t.logger,
		fileName: name,
		record:   responseRecord,
		maxBytes: t.settings.MaxBodyBytes,
		file:     t.logger.createBodyFile(name),
	}
	responseRecord["body_file"] = name + bodyFileSuffix
	t.logger.enqueue(name, responseRecord)
	response.Body = capture
	return response, nil
}

// recordRequestBody saves the bytes the request will carry, exactly as sent, to its own
// file and describes them in the request record. The SDK has already encoded the body, so
// these are the wire bytes, not the JSON the runner built.
func (t *Transport) recordRequestBody(record map[string]any, requestID int64, request *http.Request) {
	data, truncated, ok := captureRequestBody(request, t.settings.MaxBodyBytes)
	if !ok {
		record["body_unavailable"] = true
		return
	}
	name := "request" + strconv.FormatInt(requestID, 10)
	if t.logger.writeBodyFile(name, data) {
		record["body_file"] = name + bodyFileSuffix
	}
	record["body_bytes"] = len(data)
	record["body_truncated"] = truncated
}

func captureRequestBody(request *http.Request, maxBytes int64) (data []byte, truncated, ok bool) {
	if request.Body == nil || request.Body == http.NoBody {
		return nil, false, true
	}
	if request.GetBody == nil {
		return nil, false, false
	}
	body, err := request.GetBody()
	if err != nil {
		return nil, false, false
	}
	captured, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	_ = body.Close()
	if err != nil {
		return nil, false, false
	}
	if int64(len(captured)) > maxBytes {
		return captured[:maxBytes], true, true
	}
	return captured, false, true
}

func requestHeaders(headers http.Header) map[string]string {
	result := make(map[string]string, len(headers))
	for name, values := range headers {
		key := strings.ToLower(name)
		if sensitiveHeaderPattern.MatchString(key) {
			result[key] = redactedValue
			continue
		}
		result[key] = strings.Join(values, ", ")
	}
	return result
}

func responseHeaders(headers http.Header) map[string]string {
	result := make(map[string]string, len(headers))
	for name, values := range headers {
		result[strings.ToLower(name)] = strings.Join(values, ", ")
	}
	return result
}

func responseURL(response *http.Response, request *http.Request) string {
	if response.Request != nil && response.Request.URL != nil {
		return response.Request.URL.String()
	}
	return request.URL.String()
}

func responseStatusText(response *http.Response) string {
	if _, text, ok := strings.Cut(response.Status, " "); ok {
		return text
	}
	return ""
}

// responseBodyCapture copies the provider's response bytes to a file as they are read,
// so an interrupted stream keeps what arrived. The record file is rewritten at the end
// with the size and whether the cap cut the copy.
type responseBodyCapture struct {
	body     io.ReadCloser
	logger   *fileLogger
	fileName string
	record   map[string]any
	maxBytes int64

	mu       sync.Mutex
	file     *os.File
	written  int64
	finished bool
}

func (c *responseBodyCapture) Read(buffer []byte) (int, error) {
	n, err := c.body.Read(buffer)
	if n > 0 {
		c.capture(buffer[:n])
	}
	if err == io.EOF {
		c.finish(false)
	}
	return n, err
}

func (c *responseBodyCapture) Close() error {
	c.finish(false)
	return c.body.Close()
}

func (c *responseBodyCapture) capture(chunk []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return
	}
	remaining := c.maxBytes - c.written
	if remaining > 0 {
		if int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
		}
		if c.file != nil {
			if _, err := c.file.Write(chunk); err != nil {
				c.logger.log.Warn("unable to write HTTP debug body", slog.String("record", c.fileName), slog.Any("error", err))
				_ = c.file.Close()
				c.file = nil
			}
		}
		c.written += int64(len(chunk))
	}
	if c.written >= c.maxBytes {
		c.finishLocked(true)
	}
}

func (c *responseBodyCapture) finish(truncated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finishLocked(truncated)
}

func (c *responseBodyCapture) finishLocked(truncated bool) {
	if c.finished {
		return
	}
	if c.written >= c.maxBytes {
		truncated = true
	}
	if c.file != nil {
		_ = c.file.Close()
		c.file = nil
	}
	c.record["body_bytes"] = c.written
	c.record["body_truncated"] = truncated
	c.finished = true
	c.logger.enqueue(c.fileName, c.record)
}

type fileWrite struct {
	name string
	data []byte
}

type fileLogger struct {
	directory      string
	initializeOnce sync.Once
	mu             sync.Mutex
	nextID         int64
	queue          []fileWrite
	writing        bool
	pending        sync.WaitGroup
	log            *slog.Logger
}

func newFileLogger(directory string, logger *slog.Logger) *fileLogger {
	return &fileLogger{directory: directory, nextID: 1, log: logger}
}

func (l *fileLogger) reserve() int64 {
	l.initializeOnce.Do(l.initialize)
	l.mu.Lock()
	defer l.mu.Unlock()
	id := l.nextID
	l.nextID++
	return id
}

func (l *fileLogger) initialize() {
	if err := os.MkdirAll(l.directory, 0o755); err != nil {
		l.log.Warn("unable to initialize HTTP debug directory", slog.String("directory", l.directory), slog.Any("error", err))
		return
	}
	entries, err := os.ReadDir(l.directory)
	if err != nil {
		l.log.Warn("unable to read HTTP debug directory", slog.String("directory", l.directory), slog.Any("error", err))
		return
	}
	var next int64 = 1
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		match := debugFilePattern.FindStringSubmatch(entry.Name())
		if len(match) != 2 {
			continue
		}
		id, err := strconv.ParseInt(match[1], 10, 64)
		if err == nil && id >= next {
			next = id + 1
		}
	}
	l.mu.Lock()
	l.nextID = next
	l.mu.Unlock()
}

func (l *fileLogger) enqueue(name string, record map[string]any) {
	data, err := json.Marshal(record)
	if err != nil {
		l.log.Warn("unable to encode HTTP debug record", slog.String("record", name), slog.Any("error", err))
		return
	}
	data = append(data, '\n')
	l.mu.Lock()
	l.pending.Add(1)
	l.queue = append(l.queue, fileWrite{name: name, data: data})
	if !l.writing {
		l.writing = true
		go l.writeQueue()
	}
	l.mu.Unlock()
}

func (l *fileLogger) writeQueue() {
	for {
		l.mu.Lock()
		if len(l.queue) == 0 {
			l.writing = false
			l.mu.Unlock()
			return
		}
		entry := l.queue[0]
		l.queue[0] = fileWrite{}
		l.queue = l.queue[1:]
		l.mu.Unlock()

		err := os.WriteFile(filepath.Join(l.directory, entry.name), entry.data, 0o600)
		if err != nil {
			l.log.Warn("unable to write HTTP debug file", slog.String("path", filepath.Join(l.directory, entry.name)), slog.Any("error", err))
		}
		l.pending.Done()
	}
}

// createBodyFile opens the raw body file of a record, or returns nil (and logs) when it
// cannot, so the response is still passed through untouched.
func (l *fileLogger) createBodyFile(name string) *os.File {
	file, err := os.OpenFile(filepath.Join(l.directory, name+bodyFileSuffix), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		l.log.Warn("unable to create HTTP debug body file", slog.String("record", name), slog.Any("error", err))
		return nil
	}
	return file
}

func (l *fileLogger) writeBodyFile(name string, data []byte) bool {
	path := filepath.Join(l.directory, name+bodyFileSuffix)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		l.log.Warn("unable to write HTTP debug body file", slog.String("path", path), slog.Any("error", err))
		return false
	}
	return true
}
