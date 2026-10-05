package httpdebug

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const redactedValue = "[REDACTED]"

var (
	sensitiveHeaderPattern = regexp.MustCompile(`(?i)(?:authorization|api[-_]?key|cookie|set-cookie|proxy-authorization)`)
	debugFilePattern       = regexp.MustCompile(`^(?:request|response)([0-9]+)$`)
)

// Settings controls opt-in HTTP diagnostics. Body capture is bounded before
// text is written, and request credentials are redacted from captured headers.
type Settings struct {
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
		logger:   newFileLogger(directory),
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
		for key, value := range captureRequestBody(request, t.settings.MaxBodyBytes) {
			requestRecord[key] = value
		}
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
	response.Body = &responseBodyCapture{
		body:     response.Body,
		logger:   t.logger,
		fileName: "response" + strconv.FormatInt(requestID, 10),
		record:   responseRecord,
		maxBytes: t.settings.MaxBodyBytes,
		captured: make([]byte, 0),
	}
	return response, nil
}

func captureRequestBody(request *http.Request, maxBytes int64) map[string]any {
	if request.Body == nil || request.Body == http.NoBody {
		return map[string]any{"body": "", "body_truncated": false}
	}
	if request.GetBody == nil {
		return map[string]any{"body_unavailable": true}
	}
	body, err := request.GetBody()
	if err != nil {
		return map[string]any{"body_unavailable": true}
	}
	captured, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	_ = body.Close()
	if err != nil {
		return map[string]any{"body_unavailable": true}
	}
	truncated := int64(len(captured)) > maxBytes
	if truncated {
		captured = captured[:maxBytes]
	}
	return map[string]any{"body": string(captured), "body_truncated": truncated}
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

type responseBodyCapture struct {
	body     io.ReadCloser
	logger   *fileLogger
	fileName string
	record   map[string]any
	maxBytes int64

	mu       sync.Mutex
	captured []byte
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
	remaining := c.maxBytes - int64(len(c.captured))
	if remaining > 0 {
		if int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
		}
		c.captured = append(c.captured, chunk...)
	}
	if int64(len(c.captured)) >= c.maxBytes {
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
	if int64(len(c.captured)) >= c.maxBytes {
		truncated = true
	}
	c.record["body"] = string(c.captured)
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
}

func newFileLogger(directory string) *fileLogger {
	return &fileLogger{directory: directory, nextID: 1}
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
		log.Printf("unable to initialize HTTP debug directory %s: %v", l.directory, err)
		return
	}
	entries, err := os.ReadDir(l.directory)
	if err != nil {
		log.Printf("unable to read HTTP debug directory %s: %v", l.directory, err)
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
		log.Printf("unable to encode HTTP debug record %s: %v", name, err)
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
			log.Printf("unable to write HTTP debug file %s: %v", filepath.Join(l.directory, entry.name), err)
		}
		l.pending.Done()
	}
}
