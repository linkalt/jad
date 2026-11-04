// Package headless runs one session turn without a listening server. It drives the same
// run route the web client uses (POST /api/sessions/{ref}/run) in-process, so a headless
// run takes the same session and transcript locks, writes the same messages.jsonl and
// streams the same events; only the transport differs. The events are read from an
// in-memory pipe instead of a socket.
package headless

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Run outcomes. They are the "status" of a Result and decide the exit code of "jad run".
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusAborted   = "aborted"
	StatusTimedOut  = "timed_out"
)

// Request is one headless turn.
type Request struct {
	// Session is a session reference ("demo" or "root:demo").
	Session string
	// Profile is "<category>/<name>"; empty uses the session's default profile.
	Profile string
	// Model is a model reference the profile allows; empty uses the profile's default.
	Model string
	// Input is the user message.
	Input string
	// Timeout stops the run when it has not ended after this long; zero means no limit
	// beyond the run's own (max_tool_rounds, max_run_tokens).
	Timeout time.Duration
	// OnEvent, when set, receives every run-stream event in order, as the web client
	// would see it. It must not block for long: the stream waits for it.
	OnEvent func(Event)
}

// Event is one run-stream event: its SSE name and its JSON data.
type Event struct {
	Name string          `json:"event"`
	Data json.RawMessage `json:"data"`
}

// Result is the outcome of a headless turn.
type Result struct {
	Status  string `json:"status"`
	Session string `json:"session"`
	Profile string `json:"profile,omitempty"`
	TurnID  string `json:"turn_id,omitempty"`
	// Text is the final assistant message text; on a stopped run it is the text
	// streamed so far.
	Text string `json:"text"`
	// Error is the client-safe failure message of a failed, stopped or timed-out run.
	Error string `json:"error,omitempty"`
	// Usage is the last usage summary the run published (webui.UsageSummary), if any.
	Usage json.RawMessage `json:"usage,omitempty"`
	// CostUSD is what the run's priced responses cost, from the pricing in the model
	// files; absent when no response was priced.
	CostUSD    *float64 `json:"cost_usd,omitempty"`
	DurationMS int64    `json:"duration_ms"`
}

// Runner sends headless turns to an API handler.
type Runner struct {
	handler http.Handler
	token   string
}

// NewRunner returns a runner for handler, which must accept token as its bearer token.
func NewRunner(handler http.Handler, token string) *Runner {
	return &Runner{handler: handler, token: token}
}

// RequestError is a run the server refused before it started, with the HTTP status and
// the server's message (for example 404 for an unknown session, 400 for a model outside
// the profile).
type RequestError struct {
	Status  int
	Message string
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("run refused (HTTP %d): %s", e.Status, e.Message)
}

// Run starts one turn and returns when its stream has ended. Cancelling ctx stops the
// run and returns an aborted result once the run has ended; the request's timeout does
// the same with a timed_out result. The error is set only when no run was started.
func (r *Runner) Run(ctx context.Context, request Request) (Result, error) {
	if strings.TrimSpace(request.Input) == "" {
		return Result{}, errors.New("the prompt is empty")
	}
	if strings.TrimSpace(request.Session) == "" {
		return Result{}, errors.New("a session is required")
	}
	body, err := json.Marshal(struct {
		Input string `json:"input"`
		Model string `json:"model,omitempty"`
	}{Input: request.Input, Model: request.Model})
	if err != nil {
		return Result{}, err
	}
	started := time.Now()

	// The stream request has its own context: ending it only detaches the reader, while
	// the run itself is stopped through the stop route, as a client would.
	streamCtx, detach := context.WithCancel(context.WithoutCancel(ctx))
	defer detach()
	httpRequest, err := http.NewRequestWithContext(streamCtx, http.MethodPost, r.sessionPath(request.Session, "run", request.Profile), bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	r.authorize(httpRequest)
	reader, writer := io.Pipe()
	stream := newStreamWriter(writer)
	served := make(chan struct{})
	go func() {
		defer close(served)
		defer writer.Close()
		defer stream.closeStatus()
		r.handler.ServeHTTP(stream, httpRequest)
	}()
	defer func() {
		// The handler may still be writing when the reader stops early; closing the read
		// side unblocks it, and detach ends its request context.
		_ = reader.Close()
		detach()
		<-served
	}()

	status := stream.waitStatus()
	if status != http.StatusOK {
		raw, _ := io.ReadAll(reader)
		return Result{}, &RequestError{Status: status, Message: errorMessage(raw)}
	}

	result := Result{Session: request.Session, Profile: request.Profile}
	var timeout <-chan time.Time
	if request.Timeout > 0 {
		timer := time.NewTimer(request.Timeout)
		defer timer.Stop()
		timeout = timer.C
	}
	events := make(chan Event)
	readErr := make(chan error, 1)
	go func() {
		defer close(events)
		readErr <- readEvents(streamCtx, reader, events)
	}()

	var stopReason string
	var streamed string
	stop := func(reason string) {
		if stopReason != "" {
			return
		}
		stopReason = reason
		r.stop(request.Session, request.Profile)
	}
	done := ctx.Done()
	for {
		select {
		case <-done:
			done = nil
			stop(StatusAborted)
			continue
		case <-timeout:
			timeout = nil
			stop(StatusTimedOut)
			continue
		case event, ok := <-events:
			if !ok {
				if err := <-readErr; err != nil && result.Status == "" {
					result.Status, result.Error = StatusFailed, fmt.Sprintf("the run stream ended early: %v", err)
				}
				if result.Status == "" {
					result.Status, result.Error = StatusFailed, "the run stream ended without a result"
				}
				result.DurationMS = time.Since(started).Milliseconds()
				return result, nil
			}
			if request.OnEvent != nil {
				request.OnEvent(event)
			}
			switch event.Name {
			case "run":
				var summary struct {
					TurnID string `json:"turn_id"`
				}
				if json.Unmarshal(event.Data, &summary) == nil && summary.TurnID != "" {
					result.TurnID = summary.TurnID
				}
			case "ui.message":
				if text, ok := messageText(event.Data); ok {
					streamed = text
				}
			case "usage":
				var payload struct {
					Usage   json.RawMessage `json:"usage"`
					CostUSD *float64        `json:"cost_usd"`
				}
				if json.Unmarshal(event.Data, &payload) == nil && len(payload.Usage) > 0 {
					result.Usage = payload.Usage
				}
				if payload.CostUSD != nil {
					result.CostUSD = payload.CostUSD
				}
			case "completed":
				result.Status = StatusCompleted
				if text, ok := messageText(event.Data); ok {
					result.Text = text
				} else {
					result.Text = streamed
				}
			case "error":
				var payload struct {
					Message string `json:"message"`
				}
				_ = json.Unmarshal(event.Data, &payload)
				result.Error = payload.Message
				result.Text = streamed
				switch stopReason {
				case "":
					result.Status = StatusFailed
				default:
					result.Status = stopReason
				}
			case "done":
				if result.Status == "" {
					result.Status = StatusFailed
				}
				result.DurationMS = time.Since(started).Milliseconds()
				return result, nil
			}
		}
	}
}

// stop asks the server to stop the run of the session and profile. Its answer is not
// needed: the run's stream reports how it ended.
func (r *Runner) stop(session, profile string) {
	request, err := http.NewRequest(http.MethodPost, r.sessionPath(session, "run/stop", profile), strings.NewReader("{}"))
	if err != nil {
		return
	}
	r.authorize(request)
	r.handler.ServeHTTP(newDiscardWriter(), request)
}

func (r *Runner) authorize(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+r.token)
	request.Header.Set("Content-Type", "application/json")
}

func (r *Runner) sessionPath(session, action, profile string) string {
	path := "/api/sessions/" + url.PathEscape(session) + "/" + action
	if profile != "" {
		path += "?profile=" + url.QueryEscape(profile)
	}
	return path
}

// messageText reads message.text of a ui.message or completed event.
func messageText(data json.RawMessage) (string, bool) {
	var payload struct {
		Message *struct {
			Text string `json:"text"`
		} `json:"message"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Message == nil {
		return "", false
	}
	return payload.Message.Text, true
}

// errorMessage reads {"error": "..."} from a refused request, or returns the body.
func errorMessage(raw []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &payload) == nil && payload.Error != "" {
		return payload.Error
	}
	return strings.TrimSpace(string(raw))
}

// readEvents parses SSE frames ("event: name", "data: json", blank line) from reader
// and sends each one. Comment frames (keep-alives) are skipped. It returns nil at the
// end of the stream.
func readEvents(ctx context.Context, reader io.Reader, events chan<- Event) error {
	scanner := bufio.NewScanner(reader)
	// A frame is one line of JSON; a final message can be large, so the line limit only
	// bounds memory, it does not cut frames the server is allowed to send.
	scanner.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)
	var name string
	var data []byte
	for scanner.Scan() {
		line := scanner.Bytes()
		switch {
		case len(line) == 0:
			if name != "" {
				event := Event{Name: name, Data: json.RawMessage(append([]byte(nil), data...))}
				select {
				case events <- event:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			name, data = "", data[:0]
		case line[0] == ':':
		case bytes.HasPrefix(line, []byte("event: ")):
			name = string(line[len("event: "):])
		case bytes.HasPrefix(line, []byte("data: ")):
			data = append(data, line[len("data: "):]...)
		}
	}
	return scanner.Err()
}

// maxFrameBytes is the largest SSE line the reader accepts. The server bounds a frame by
// the run's message size, which is bounded by the transcript's max_item_bytes; this is
// only a guard against unbounded memory, set well above any frame the server sends.
const maxFrameBytes = 1 << 30

// streamWriter is an http.ResponseWriter and http.Flusher that writes the body into a
// pipe and reports the status code once it is known.
type streamWriter struct {
	header     http.Header
	pipe       *io.PipeWriter
	statusOnce sync.Once
	status     chan int
}

func newStreamWriter(pipe *io.PipeWriter) *streamWriter {
	return &streamWriter{header: http.Header{}, pipe: pipe, status: make(chan int, 1)}
}

func (w *streamWriter) Header() http.Header { return w.header }

func (w *streamWriter) WriteHeader(status int) {
	w.statusOnce.Do(func() { w.status <- status })
}

func (w *streamWriter) Write(data []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pipe.Write(data)
}

// Flush is a no-op: every Write already blocks until the reader has the bytes.
func (w *streamWriter) Flush() {}

// waitStatus returns the status code. A handler that returns without writing anything
// answered 200 with an empty body, as net/http would.
func (w *streamWriter) waitStatus() int {
	return <-w.status
}

// closeStatus reports 200 for a handler that wrote nothing; called when the handler has
// returned.
func (w *streamWriter) closeStatus() { w.WriteHeader(http.StatusOK) }

// discardWriter is a ResponseWriter whose answer nobody reads.
type discardWriter struct{ header http.Header }

func newDiscardWriter() *discardWriter                  { return &discardWriter{header: http.Header{}} }
func (w *discardWriter) Header() http.Header            { return w.header }
func (w *discardWriter) WriteHeader(int)                {}
func (w *discardWriter) Write(data []byte) (int, error) { return len(data), nil }
