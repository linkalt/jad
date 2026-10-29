package anthropicadapter

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// sseEvent is one server-sent event read from the upstream.
type sseEvent struct {
	Name string
	Data string
}

// readSSE parses a server-sent event stream and calls handle for each event
// that carries data. It stops when handle returns false or the stream ends.
// One event may span at most maxEvent bytes (all its lines, terminators included);
// a larger one ends the read with an error instead of growing the buffer.
func readSSE(reader io.Reader, maxEvent int64, handle func(sseEvent) bool) error {
	buffered := bufio.NewReader(reader)
	var name string
	var data []string
	var eventBytes int64
	dispatch := func() bool {
		defer func() { name, data, eventBytes = "", nil, 0 }()
		if len(data) == 0 {
			return true
		}
		return handle(sseEvent{Name: name, Data: strings.Join(data, "\n")})
	}
	for {
		line, err := readBoundedLine(buffered, maxEvent-eventBytes)
		eventBytes += int64(len(line))
		if err == errEventTooLarge {
			return fmt.Errorf("an upstream event is larger than upstream.max_event_bytes (%d)", maxEvent)
		}
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if !dispatch() {
					return nil
				}
			case strings.HasPrefix(line, ":"):
				// A comment line, used as keep-alive.
			default:
				field, value, _ := strings.Cut(line, ":")
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "event":
					name = value
				case "data":
					data = append(data, value)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				dispatch()
				return nil
			}
			return err
		}
	}
}

// errEventTooLarge reports that a line did not fit in the remaining event budget.
var errEventTooLarge = errors.New("sse event too large")

// readBoundedLine reads up to and including the next newline, or fails with
// errEventTooLarge once the line would exceed limit bytes.
func readBoundedLine(reader *bufio.Reader, limit int64) (string, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if int64(len(line)+len(chunk)) > limit {
			return "", errEventTooLarge
		}
		line = append(line, chunk...)
		if err != bufio.ErrBufferFull {
			return string(line), err
		}
	}
}

// sseSink writes Responses events to the caller. It numbers the events and
// remembers whether anything was sent, which decides whether a failed upstream
// attempt can still be retried invisibly.
type sseSink struct {
	w       http.ResponseWriter
	flusher http.Flusher
	begun   bool
	events  int
	// err is the first write error; once set the client is gone.
	err error
}

func newSink(w http.ResponseWriter) *sseSink {
	flusher, _ := w.(http.Flusher)
	return &sseSink{w: w, flusher: flusher}
}

// begin sends the response headers so the caller sees the stream open.
func (s *sseSink) begin() {
	if s.begun {
		return
	}
	s.begun = true
	header := s.w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.flush()
}

func (s *sseSink) flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// event writes one Responses event with the next sequence number.
func (s *sseSink) event(kind string, fields map[string]any) {
	if s.err != nil {
		return
	}
	payload := make(map[string]any, len(fields)+2)
	for key, value := range fields {
		payload[key] = value
	}
	payload["type"] = kind
	payload["sequence_number"] = s.events
	encoded, err := json.Marshal(payload)
	if err != nil {
		s.err = err
		return
	}
	s.events++
	s.write("event: " + kind + "\ndata: " + string(encoded) + "\n\n")
}

// done ends the stream the way the Responses API does.
func (s *sseSink) done() { s.write("data: [DONE]\n\n") }

// comment writes a keep-alive line the caller's SSE parser ignores.
func (s *sseSink) comment(text string) { s.write(": " + text + "\n\n") }

func (s *sseSink) write(text string) {
	if s.err != nil {
		return
	}
	s.begin()
	if _, err := io.WriteString(s.w, text); err != nil {
		s.err = err
		return
	}
	s.flush()
}
