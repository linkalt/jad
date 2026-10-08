package anthropicadapter

import (
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SetLogger sets where upstream failures are logged. A nil logger discards them, which is also
// the state of a handler built without calling SetLogger, so tests and library users stay quiet.
func (h *Handler) SetLogger(logger *slog.Logger) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h.logger = logger
}

// upstreamFailure describes one failed upstream call for the log.
type upstreamFailure struct {
	// Kind is "http_status", "stream_error_event", "not_event_stream" or "error_body_with_200".
	Kind   string
	Status int
	Model  string
	Stream bool
	Header http.Header
	// Body is the response body, or the error event's message.
	Body string
	// ErrorType is the Anthropic error type of an error event.
	ErrorType string
	// Retried is true when the adapter replays the request after this failure.
	Retried bool
	// SentHeaders are the names (never the values) of the headers the adapter sent.
	SentHeaders []string
}

// logUpstreamFailure writes one line for a failed upstream call. It records the status, a
// configured allow-list of response headers, the start of the body, and the names of the headers
// that were sent. It never records a request header value, so the token cannot reach the log.
func (h *Handler) logUpstreamFailure(failure upstreamFailure) {
	settings := &h.config.Log
	if !settings.UpstreamErrors {
		return
	}
	attrs := []any{
		slog.String("kind", failure.Kind),
		slog.String("model", failure.Model),
		slog.Bool("stream", failure.Stream),
	}
	if failure.Status != 0 {
		attrs = append(attrs, slog.Int("status", failure.Status))
	}
	if failure.ErrorType != "" {
		attrs = append(attrs, slog.String("error_type", failure.ErrorType))
	}
	if failure.Retried {
		attrs = append(attrs, slog.Bool("retried", true))
	}
	if headers := selectHeaders(failure.Header, settings.Headers); len(headers) > 0 {
		attrs = append(attrs, slog.Group("response_headers", headers...))
	}
	if len(failure.SentHeaders) > 0 {
		attrs = append(attrs, slog.String("sent_header_names", strings.Join(failure.SentHeaders, ",")))
	}
	attrs = append(attrs, slog.String("body", clip(failure.Body, settings.BodyBytes)))
	h.logger.Warn("upstream request failed", attrs...)
}

// selectHeaders returns the response headers whose names match the allow-list. An entry ending in
// "*" matches by prefix; matching ignores case. The order is stable.
func selectHeaders(header http.Header, allow []string) []any {
	if len(header) == 0 || len(allow) == 0 {
		return nil
	}
	names := make([]string, 0, len(header))
	for name := range header {
		if headerAllowed(name, allow) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]any, 0, len(names))
	for _, name := range names {
		out = append(out, slog.String(strings.ToLower(name), strings.Join(header.Values(name), ", ")))
	}
	return out
}

func headerAllowed(name string, allow []string) bool {
	name = strings.ToLower(name)
	for _, entry := range allow {
		entry = strings.ToLower(entry)
		if prefix, ok := strings.CutSuffix(entry, "*"); ok {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		} else if name == entry {
			return true
		}
	}
	return false
}

// clip returns text on one line, cut to at most limit bytes on a rune boundary, with a note of how
// much was cut. A limit of 0 returns an empty string.
func clip(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…(" + strconv.Itoa(len(text)-cut) + " more bytes)"
}

// sentHeaderNames lists the names of the headers on an outgoing upstream request, sorted.
func sentHeaderNames(header http.Header) []string {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, strings.ToLower(name))
	}
	sort.Strings(names)
	return names
}
