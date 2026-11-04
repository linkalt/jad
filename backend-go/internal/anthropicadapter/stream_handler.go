package anthropicadapter

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// respondStream serves a streaming request. An upstream HTTP error status is
// returned as a plain error response before any stream byte, so the caller's
// own retry sees the real status. An error event inside the stream cannot
// change the status, so it becomes response.failed, unless stream_retry
// replays the request while nothing has been sent.
func (h *Handler) respondStream(w http.ResponseWriter, r *http.Request, apiKey string, translated *translation) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(h.config.Upstream.RequestTimeoutMS)*time.Millisecond)
	defer cancel()
	translated.Request.Stream = true
	body, apiErr := messagesPayload(h.config, &translated.Request)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	retry := h.config.Upstream.StreamRetry
	sink := newSink(w)
	for attempt := 0; ; attempt++ {
		upstream, err := h.sendMessages(ctx, apiKey, body, true)
		if err != nil {
			writeError(w, callError(err, nil))
			return
		}
		if upstreamFailed(upstream) {
			payload := readErrorBody(upstream)
			closeHTTPResponse(upstream)
			h.logUpstreamFailure(upstreamFailure{Kind: "http_status", Status: upstream.StatusCode, Model: translated.Request.Model, Stream: true,
				Header: upstream.Header, Body: string(payload), SentHeaders: sentHeaderNames(h.upstreamHeaders(""))})
			writeError(w, upstreamError(upstream.StatusCode, payload, upstream.Header))
			return
		}
		if !strings.HasPrefix(upstream.Header.Get("Content-Type"), "text/event-stream") {
			payload := readErrorBody(upstream)
			closeHTTPResponse(upstream)
			h.logUpstreamFailure(upstreamFailure{Kind: "not_event_stream", Status: upstream.StatusCode, Model: translated.Request.Model, Stream: true,
				Header: upstream.Header, Body: string(payload), SentHeaders: sentHeaderNames(h.upstreamHeaders(""))})
			writeError(w, upstreamError(http.StatusBadGateway, payload, upstream.Header))
			return
		}
		// The adapter's own parser reads the raw body: it keeps ping events, unknown
		// future events, and the configured retry handling for error events.
		if id := upstream.Header.Get("request-id"); id != "" {
			w.Header().Set("request-id", id)
		}
		translator := newStreamTranslator(sink, h.config, translated.Request.Model, h.now)
		translator.allowRetry = attempt < retry.MaxRetries
		outcome := translator.run(upstream.Body)
		if translator.sink.err == nil {
			drainUpstream(ctx, upstream.Body)
		}
		closeHTTPResponse(upstream)
		if outcome.ErrorType != "" {
			h.logUpstreamFailure(upstreamFailure{Kind: "stream_error_event", Status: upstream.StatusCode, Model: translated.Request.Model, Stream: true,
				Header: upstream.Header, Body: outcome.ErrorMessage, ErrorType: outcome.ErrorType, Retried: outcome.Retry})
		}
		if !outcome.Retry {
			return
		}
		if !sleepContext(ctx, time.Duration(retry.DelayMS)*time.Millisecond) {
			translator.fail("upstream_retry_cancelled", "the request ended while waiting to retry")
			return
		}
	}
}

// errorBodyLimit bounds how much of an upstream error body is read.
const errorBodyLimit = 1 << 20

// The translator stops at message_stop. Reading the rest of a small tail lets
// the HTTP/1.1 connection go back to the pool instead of being dropped. The
// drain runs after the last event was flushed to the client, but the response
// only ends when the handler returns, so both bounds are kept small.
const (
	drainByteLimit = 64 << 10
	drainTimeout   = 25 * time.Millisecond
)

func drainUpstream(ctx context.Context, body io.Reader) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, io.LimitReader(body, drainByteLimit))
	}()
	timer := time.NewTimer(drainTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
