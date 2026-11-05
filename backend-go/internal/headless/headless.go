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

	"jad/backend-go/internal/outputcheck"
)

// Run outcomes. They are the "status" of a Result and decide the exit code of "jad run".
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusAborted   = "aborted"
	StatusTimedOut  = "timed_out"
	// StatusInvalidOutput is a run that completed with a final answer that is not JSON
	// matching the request's output schema.
	StatusInvalidOutput = "invalid_output"
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
	// Output, when set, asks for a final answer that is JSON matching a schema: the run
	// sends it as output_format, and Run checks the answer against it.
	Output *OutputSchema
	// Commands makes an input that starts with a slash command run it (see ParseCommand):
	// an action ends without a turn, and a library or MCP command is replaced by its text.
	Commands bool
	// Plan starts the turn in plan mode: the model sees only the tools that never change
	// anything, and the session's plan_mode instructions. /plan sets it too.
	Plan bool
	// Tools, when not nil, narrows the turn's tools to the ones these tools.allow entries
	// cover. A custom command's allowed-tools sets it when it is nil.
	Tools []string
	// ExecutePlan carries out the plan the session's transcript ends with: the turn sends
	// the session's plan_mode.execute_prompt, followed by Input when it has text. Input may
	// be empty, and is not read for commands.
	ExecutePlan bool
	// Attachments are images and PDFs sent with Input as parts of one message. A
	// /resource command adds those of the resource it reads.
	Attachments []Attachment
}

// Attachment is an image or a PDF sent with the input: Type "image", or "file" with a Name;
// Data is base64 (or a data URL). It is a run input part as the run route reads it.
type Attachment struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	Data string `json:"data"`
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
	// Output is the final answer as JSON, set when the request had an output schema
	// and the answer matched it.
	Output json.RawMessage `json:"output,omitempty"`
	// Error is the client-safe failure message of a failed, stopped or timed-out run.
	Error string `json:"error,omitempty"`
	// Usage is the last usage summary the run published (webui.UsageSummary), if any.
	Usage json.RawMessage `json:"usage,omitempty"`
	// CostUSD is what the run's priced responses cost, from the pricing in the model
	// files; absent when no response was priced.
	CostUSD    *float64 `json:"cost_usd,omitempty"`
	DurationMS int64    `json:"duration_ms"`
	// Command is the slash command the input started with, without its slash, when
	// Request.Commands was set; CommandResult is the answer of an action's route.
	Command       string          `json:"command,omitempty"`
	CommandResult json.RawMessage `json:"command_result,omitempty"`
	// Plan is set when the turn ran in plan mode, ExecutePlan when it carried out a plan.
	Plan        bool `json:"plan,omitempty"`
	ExecutePlan bool `json:"execute_plan,omitempty"`
	// Model is the model the turn asked for (--model, or a custom command's front matter);
	// empty for the profile's default.
	Model string `json:"model,omitempty"`
	// StopHooksSkipped is the report that the stop commands were not run because the run
	// reached max_run_tokens, max_run_cost_usd or the budget; empty when they ran or the
	// profile has none.
	StopHooksSkipped string `json:"stop_hooks_skipped,omitempty"`
}

// The hook_report event's event and trigger of the stop commands skipped at a cap
// (session.StopSkippedTrigger); this client reads the wire, not the session package.
const (
	hookEventStop      = "stop"
	stopSkippedTrigger = "cap_reached"
)

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
	if strings.TrimSpace(request.Input) == "" && !request.ExecutePlan {
		return Result{}, errors.New("the prompt is empty")
	}
	if request.ExecutePlan && request.Plan {
		return Result{}, errors.New("carrying out a plan cannot run in plan mode")
	}
	if strings.TrimSpace(request.Session) == "" {
		return Result{}, errors.New("a session is required")
	}
	if request.Commands && !request.ExecutePlan {
		if command, ok := ParseCommand(request.Input); ok {
			if err := command.Check(request.Model, request.Output != nil, request.Plan); err != nil {
				return Result{}, err
			}
			word := command.Word
			if command.Kind == CommandPlan {
				// /plan [command] text: the rest is the prompt, and may start with a
				// library, MCP or custom command of its own (Check refused an action).
				request.Plan, request.Input = true, command.Text
				command, ok = ParseCommand(request.Input)
			}
			if ok {
				result, done, err := r.runCommand(ctx, &request, command)
				switch {
				case errors.Is(err, errNotACommand):
					// A /word the library has no command for is a prompt, sent as written.
					if word != CommandPlan {
						word = ""
					}
				case err != nil || done:
					return result, err
				case word != CommandPlan:
					word = command.Word
				}
			}
			result, err := r.run(ctx, request)
			if err == nil {
				result.Command = word
			}
			return result, err
		}
	}
	return r.run(ctx, request)
}

// run sends the request's input as one turn.
func (r *Runner) run(ctx context.Context, request Request) (Result, error) {
	var outputFormat json.RawMessage
	if request.Output != nil {
		format, err := request.Output.format()
		if err != nil {
			return Result{}, err
		}
		outputFormat = format
	}
	var input any = request.Input
	if len(request.Attachments) > 0 {
		parts := make([]any, 0, len(request.Attachments)+1)
		if strings.TrimSpace(request.Input) != "" {
			parts = append(parts, map[string]string{"type": "text", "text": request.Input})
		}
		for _, attachment := range request.Attachments {
			parts = append(parts, attachment)
		}
		input = parts
	}
	body, err := json.Marshal(struct {
		Input        any             `json:"input"`
		Model        string          `json:"model,omitempty"`
		OutputFormat json.RawMessage `json:"output_format,omitempty"`
		Plan         bool            `json:"plan,omitempty"`
		Tools        []string        `json:"tools,omitempty"`
		ExecutePlan  bool            `json:"execute_plan,omitempty"`
	}{Input: input, Model: request.Model, OutputFormat: outputFormat, Plan: request.Plan, Tools: request.Tools, ExecutePlan: request.ExecutePlan})
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

	result := Result{Session: request.Session, Profile: request.Profile, Model: request.Model, Plan: request.Plan, ExecutePlan: request.ExecutePlan}
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
	// serverVerdict is the server's check of the answer against the output schema.
	var serverVerdict *outputcheck.Verdict
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
			case "hook_report":
				var report struct {
					Event   string `json:"event"`
					Trigger string `json:"trigger"`
					Report  string `json:"report"`
				}
				if json.Unmarshal(event.Data, &report) == nil && report.Event == hookEventStop && report.Trigger == stopSkippedTrigger {
					result.StopHooksSkipped = report.Report
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
				var checked struct {
					Output *outputcheck.Verdict `json:"output"`
				}
				if json.Unmarshal(event.Data, &checked) == nil {
					serverVerdict = checked.Output
				}
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
				if result.Status == StatusCompleted && request.Output != nil {
					// The server checks the answer against the schema it was sent; a server
					// that gave no verdict is checked here the same way.
					if serverVerdict != nil {
						request.Output.apply(&result, *serverVerdict)
					} else {
						request.Output.check(&result)
					}
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
