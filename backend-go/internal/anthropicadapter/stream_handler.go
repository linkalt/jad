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
	retry := h.config.Upstream.StreamRetry
	sink := newSink(w)
	for attempt := 0; ; attempt++ {
		upstream, apiErr := h.callUpstream(ctx, apiKey, &translated.Request)
		if apiErr != nil {
			writeError(w, apiErr)
			return
		}
		if upstream.StatusCode < 200 || upstream.StatusCode > 299 {
			payload, _ := io.ReadAll(io.LimitReader(upstream.Body, errorBodyLimit))
			upstream.Body.Close()
			writeError(w, upstreamError(upstream.StatusCode, payload, upstream.Header))
			return
		}
		if !strings.HasPrefix(upstream.Header.Get("Content-Type"), "text/event-stream") {
			payload, _ := io.ReadAll(io.LimitReader(upstream.Body, errorBodyLimit))
			upstream.Body.Close()
			writeError(w, upstreamError(http.StatusBadGateway, payload, upstream.Header))
			return
		}
		translator := newStreamTranslator(sink, h.config, translated.Request.Model, h.now)
		translator.allowRetry = attempt < retry.MaxRetries
		outcome := translator.run(upstream.Body)
		upstream.Body.Close()
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
