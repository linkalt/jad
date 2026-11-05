package httpapi

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"jad/backend-go/internal/logging"
)

// observedWriter records what a handler did to the response so ServeHTTP can log
// one line per request without the handlers knowing about logging. It keeps the
// optional interfaces the handlers rely on: Flush for SSE, Unwrap for
// http.ResponseController, and Hijack when the underlying writer has it.
type observedWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	cause  error
}

func (w *observedWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	written, err := w.ResponseWriter.Write(data)
	w.bytes += int64(written)
	return written, err
}

// flushingWriter adds Flush for an underlying writer that has it. The plain
// observedWriter deliberately has no Flush, so a handler's http.Flusher check
// gives the same answer it would give for the unwrapped writer.
type flushingWriter struct{ *observedWriter }

func (w flushingWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.ResponseWriter.(http.Flusher).Flush()
}

func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *observedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, errors.New("response writer does not support hijacking")
}

// recordCause keeps the first error behind an error response.
func (w *observedWriter) recordCause(err error) {
	if w.cause == nil {
		w.cause = err
	}
}

// causeRecorder is implemented by the writer ServeHTTP installs.
type causeRecorder interface{ recordCause(error) }

// observeRequest runs next and logs the request. A response with status 500 or
// above logs its cause at error level; every other response is a debug record.
// A panic in next is logged with its stack and answered with the same 500 body
// as any other internal error, unless the response already started.
func (h *Handler) observeRequest(w http.ResponseWriter, r *http.Request, next func(http.ResponseWriter)) {
	logger := logging.Or(h.logger)
	observed := &observedWriter{ResponseWriter: w}
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			logger.Error("request panicked",
				slog.String("method", r.Method),
				slog.String("route", routeTemplate(r.URL.EscapedPath())),
				slog.Any("panic", recovered),
				slog.String("stack", string(debug.Stack())))
			if observed.status == 0 {
				writeJSON(observed, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
			}
		}
		status := observed.status
		if status == 0 {
			status = http.StatusOK
		}
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("route", routeTemplate(r.URL.EscapedPath())),
			slog.Int("status", status),
			slog.Float64("duration_ms", durationMS(time.Since(started))),
			slog.Int64("bytes", observed.bytes),
		}
		if status >= http.StatusInternalServerError {
			if observed.cause != nil {
				attrs = append(attrs, slog.String("error", observed.cause.Error()))
			}
			logger.LogAttrs(r.Context(), slog.LevelError, "request failed", attrs...)
			return
		}
		logger.LogAttrs(r.Context(), slog.LevelDebug, "request", attrs...)
	}()
	if _, ok := w.(http.Flusher); ok {
		next(flushingWriter{observed})
		return
	}
	next(observed)
}

// knownSessionRoutes lists the path suffixes after /api/sessions/{session}. A
// path outside this set is reported as "unmatched" so a client cannot put
// arbitrary text into the log through the URL.
var knownSessionRoutes = map[string]bool{
	"tools": true, "history": true, "files": true,
	"model-default": true, "compact": true, "reset": true, "front-compact": true, "statecontext": true,
	"run": true, "run/interaction": true, "run/interaction/extend": true,
	"run/stream": true, "run/stop": true, "checkpoints": true, "fork": true,
	"mcp-prompts": true, "mcp-prompts/get": true, "mcp-resources": true, "mcp-resources/read": true, "mcp-resources/subscribe": true, "mcp-resources/unsubscribe": true, "mcp-resources/events": true,
}

// routeTemplate maps a request path to a route with identifiers replaced by
// placeholders. It never returns a client-supplied segment.
func routeTemplate(escapedPath string) string {
	segments := strings.Split(strings.Trim(escapedPath, "/"), "/")
	switch {
	case len(segments) == 2 && segments[0] == "api" && segments[1] == "health":
		return "/api/health"
	case len(segments) >= 2 && segments[0] == "api" && segments[1] == "library":
		switch len(segments) {
		case 2:
			return "/api/library"
		case 3:
			return "/api/library/{kind}"
		case 4:
			return "/api/library/{kind}/{id}"
		case 5:
			if segments[4] == "expand" {
				return "/api/library/{kind}/{id}/expand"
			}
		}
	case len(segments) == 2 && segments[0] == "api" && segments[1] == "sessions":
		return "/api/sessions"
	case len(segments) == 3 && segments[0] == "api" && segments[1] == "sessions":
		return "/api/sessions/{session}"
	case len(segments) >= 2 && segments[0] == "api" && segments[1] == "catalog":
		switch len(segments) {
		case 3:
			return "/api/catalog/{kind}"
		case 5:
			return "/api/catalog/{kind}/{id}"
		}
	case len(segments) >= 4 && segments[0] == "api" && segments[1] == "sessions":
		suffix := strings.Join(segments[3:], "/")
		if knownSessionRoutes[suffix] {
			return "/api/sessions/{session}/" + suffix
		}
		if len(segments) == 5 && segments[3] == "messages" {
			return "/api/sessions/{session}/messages/{message}"
		}
		if len(segments) == 5 && segments[3] == "images" {
			return "/api/sessions/{session}/images/{image}"
		}
		if len(segments) == 8 && segments[3] == "messages" && segments[5] == "images" {
			return "/api/sessions/{session}/messages/{message}/images/{image}"
		}
		if len(segments) == 7 && segments[3] == "run" && segments[4] == "images" {
			return "/api/sessions/{session}/run/images/{call}/{image}"
		}
		if len(segments) == 6 && segments[3] == "checkpoints" && segments[5] == "restore" {
			return "/api/sessions/{session}/checkpoints/{checkpoint}/restore"
		}
	}
	return "unmatched"
}

// durationMS reports d in milliseconds. A slog.Duration is a bare nanosecond
// count in JSON output, which a reader cannot tell from any other integer.
func durationMS(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
