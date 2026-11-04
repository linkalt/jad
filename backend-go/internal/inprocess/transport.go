// Package inprocess serves HTTP requests with a handler in this process, without a socket.
//
// A model whose provider speaks another protocol than the Responses API is served by a
// translator written as an http.Handler (the Anthropic adapter, the Chat Completions
// adapter). Transport puts that handler behind the ordinary Responses client, so the
// client's retry, idle guard and status handling apply to it unchanged.
package inprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// Transport is an http.RoundTripper that answers every request with Handler. The
// response body streams: the caller reads what the handler writes as it writes it, and
// closing the body cancels the handler's request context.
type Transport struct {
	Handler http.Handler
}

// errHandlerPanicked ends a stream whose handler panicked.
var errHandlerPanicked = errors.New("in-process handler panicked")

// RoundTrip runs the handler on its own goroutine and returns once it has sent its
// status, or the request was cancelled first.
func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t == nil || t.Handler == nil {
		return nil, errors.New("in-process transport has no handler")
	}
	ctx, cancel := context.WithCancel(request.Context())
	served := request.Clone(ctx)
	if served.Body == nil {
		served.Body = http.NoBody
	}
	served.RequestURI = request.URL.RequestURI()
	served.RemoteAddr = "in-process"
	reader, writer := io.Pipe()
	w := &responseWriter{header: http.Header{}, pipe: writer, ready: make(chan struct{})}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				w.abort(fmt.Errorf("%w: %v", errHandlerPanicked, recovered))
			}
			w.finish()
			if request.Body != nil {
				_ = request.Body.Close()
			}
		}()
		t.Handler.ServeHTTP(w, served)
	}()
	select {
	case <-w.ready:
	case <-request.Context().Done():
		cancel()
		_ = reader.CloseWithError(request.Context().Err())
		return nil, request.Context().Err()
	}
	if w.err != nil {
		cancel()
		_ = reader.Close()
		return nil, w.err
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", w.status, http.StatusText(w.status)),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.sent,
		Body:          &body{reader: reader, cancel: cancel},
		ContentLength: -1,
		Request:       request,
	}, nil
}

// body is the response body: closing it stops the handler.
type body struct {
	reader *io.PipeReader
	cancel context.CancelFunc
	once   sync.Once
}

func (b *body) Read(p []byte) (int, error) { return b.reader.Read(p) }

func (b *body) Close() error {
	b.once.Do(func() {
		b.cancel()
		_ = b.reader.Close()
	})
	return nil
}

// responseWriter hands the handler's status and header to RoundTrip at the first
// WriteHeader, Write or Flush, and its body through the pipe.
type responseWriter struct {
	header http.Header
	pipe   *io.PipeWriter
	ready  chan struct{}

	// Set before ready is closed and read only after it.
	status int
	sent   http.Header
	err    error

	wroteHeader bool
}

func (w *responseWriter) Header() http.Header { return w.header }

func (w *responseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	// 1xx answers are informational; the handler sends its real status later.
	if status >= 100 && status < 200 {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.sent = w.header.Clone()
	close(w.ready)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.pipe.Write(p)
}

// Flush makes the status visible; the pipe has no buffer, so written bytes already are.
func (w *responseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
}

// abort ends the exchange with an error: RoundTrip returns it when the status was not
// sent yet, and the body reader gets it otherwise.
func (w *responseWriter) abort(err error) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.err = err
		close(w.ready)
	}
	_ = w.pipe.CloseWithError(err)
}

// finish ends the body; a handler that wrote nothing answers 200 with an empty body.
func (w *responseWriter) finish() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	_ = w.pipe.Close()
}
